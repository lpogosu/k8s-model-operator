// Package render turns a ModelDeployment spec into the objects the controller
// applies. Everything here is a pure function of the spec: no client, no clock,
// no randomness. That is what makes the object shape testable without a cluster
// and what makes a second reconcile produce byte-identical output.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	"github.com/lpogosu/k8s-model-operator/api/v1alpha1"
)

// FieldOwner is the server-side apply field manager. Every object the operator
// applies carries it, which is how the API server knows which fields are ours
// and may safely revert drift in them without touching anyone else's.
const FieldOwner = "k8s-model-operator"

// Finalizer blocks deletion of a ModelDeployment until the operator has had a
// chance to run teardown.
const Finalizer = "serving.lpogosu.dev/teardown"

// Labels that carry operator state. The cache key is deliberately not part of
// the selector: a selector change would orphan the running pods.
const (
	LabelInstance = "serving.lpogosu.dev/model-deployment"
	LabelCacheKey = "serving.lpogosu.dev/cache-key"
	LabelRole     = "serving.lpogosu.dev/role"

	RoleServer = "server"
	RoleWarm   = "weight-warmer"

	// AnnCacheKey on the pod template makes a model change a rolling update.
	// Without it the pod spec would be identical across a revision bump and the
	// running pods would keep serving the old weights.
	AnnCacheKey = "serving.lpogosu.dev/cache-key"
)

// defaultImages are tags, not digests, on purpose: this repository has no way to
// verify a digest for images it does not build. Anything running for real should
// set spec.runtime.image to a digest.
var defaultImages = map[v1alpha1.Engine]string{
	v1alpha1.EngineVLLM:   "vllm/vllm-openai:v0.11.0",
	v1alpha1.EngineOllama: "ollama/ollama:0.12.3",
}

// CacheKey identifies a set of weights on disk.
//
// It covers the model name, the revision and the engine, and nothing else. The
// container image is excluded on purpose: upgrading vLLM does not change the
// bytes that were downloaded, and making the key depend on the image would throw
// away a warm cache on every image bump. The engine is included because vLLM and
// Ollama store weights in incompatible layouts under the same mount.
func CacheKey(md *v1alpha1.ModelDeployment) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s",
		Engine(md), md.Spec.Model.Name, md.Spec.Model.Revision)))
	return hex.EncodeToString(sum[:])[:12]
}

// Engine returns the configured engine.
func Engine(md *v1alpha1.ModelDeployment) v1alpha1.Engine {
	return md.Spec.Runtime.Engine
}

// Image returns the serving image: the override if given, otherwise the pinned
// default for the engine.
func Image(md *v1alpha1.ModelDeployment) string {
	if md.Spec.Runtime.Image != "" {
		return md.Spec.Runtime.Image
	}
	return defaultImages[Engine(md)]
}

// WarmImage returns the image of the warm-up Job. It defaults to the serving
// image because that image already contains the download client, and using it
// means the node has to pull one image instead of two.
func WarmImage(md *v1alpha1.ModelDeployment) string {
	if md.Spec.WeightCache != nil && md.Spec.WeightCache.WarmImage != "" {
		return md.Spec.WeightCache.WarmImage
	}
	return Image(md)
}

// ServiceName is the name of the ClusterIP Service. Child names are derived
// rather than configurable: an operator that lets you rename its children cannot
// find them again after a restart.
func ServiceName(md *v1alpha1.ModelDeployment) string { return md.Name }

// DeploymentName is the name of the serving Deployment.
func DeploymentName(md *v1alpha1.ModelDeployment) string { return md.Name }

// PDBName is the name of the PodDisruptionBudget.
func PDBName(md *v1alpha1.ModelDeployment) string { return md.Name }

// PVCName is the cache claim: either the shared claim the user provided, or one
// owned by this ModelDeployment.
func PVCName(md *v1alpha1.ModelDeployment) string {
	if md.Spec.WeightCache != nil && md.Spec.WeightCache.ExistingClaimName != "" {
		return md.Spec.WeightCache.ExistingClaimName
	}
	return md.Name + "-weights"
}

// OwnsPVC reports whether the operator created the cache claim and may delete it.
// A claim the user pointed at is never deleted: it may hold weights other
// ModelDeployments are still serving.
func OwnsPVC(md *v1alpha1.ModelDeployment) bool {
	return md.Spec.WeightCache != nil && md.Spec.WeightCache.ExistingClaimName == ""
}

// WarmJobName includes the cache key so that a model change creates a new Job
// instead of trying to mutate the immutable pod template of the old one.
func WarmJobName(md *v1alpha1.ModelDeployment, cacheKey string) string {
	return fmt.Sprintf("%s-warm-%s", md.Name, cacheKey)
}

// SelectorLabels identify the serving pods. They must never change for a given
// ModelDeployment: Deployment.spec.selector is immutable.
func SelectorLabels(md *v1alpha1.ModelDeployment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "model-server",
		"app.kubernetes.io/instance": md.Name,
		LabelInstance:                md.Name,
	}
}

// PodLabels are the selector labels plus the ones that may change over time.
func PodLabels(md *v1alpha1.ModelDeployment) map[string]string {
	l := SelectorLabels(md)
	l["app.kubernetes.io/managed-by"] = FieldOwner
	l[LabelRole] = RoleServer
	return l
}

// MountPath is where the weight cache is mounted.
func MountPath(md *v1alpha1.ModelDeployment) string {
	if md.Spec.WeightCache != nil {
		return md.Spec.WeightCache.MountPath
	}
	// Without a shared cache the same path is backed by an emptyDir, so the rest
	// of the rendering does not have to care which one it is.
	return "/models"
}

// MarkerPath is the file the warm-up Job writes once the download finished. Its
// existence under the cache key is the entire cache-hit protocol: the operator
// does not try to validate the weights themselves, because re-hashing sixty
// gigabytes on every pod start would cost more than the download it saves.
func MarkerPath(md *v1alpha1.ModelDeployment, cacheKey string) string {
	return path.Join(MountPath(md), ".warm", cacheKey)
}

// Endpoint is the in-cluster address of the OpenAI-compatible API.
func Endpoint(md *v1alpha1.ModelDeployment) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", ServiceName(md), md.Namespace, md.Spec.Runtime.Port)
}

// HealthPath is the endpoint both probes hit. vLLM answers /health only once the
// engine finished loading, which is exactly the signal needed; Ollama has no
// such endpoint, so /api/tags stands in — it answers as soon as the server is up,
// which makes readiness on Ollama weaker than on vLLM.
func HealthPath(engine v1alpha1.Engine) string {
	if engine == v1alpha1.EngineOllama {
		return "/api/tags"
	}
	return "/health"
}

// runtimeEnv is the environment shared by the serving container and the warm-up
// Job, so that both agree on where the weights live.
func runtimeEnv(md *v1alpha1.ModelDeployment) []corev1.EnvVar {
	mount := MountPath(md)
	env := []corev1.EnvVar{
		{Name: "MODEL_NAME", Value: md.Spec.Model.Name},
		{Name: "MODEL_REVISION", Value: md.Spec.Model.Revision},
	}
	switch Engine(md) {
	case v1alpha1.EngineOllama:
		env = append(env,
			corev1.EnvVar{Name: "OLLAMA_MODELS", Value: path.Join(mount, "ollama")},
			corev1.EnvVar{Name: "OLLAMA_HOST", Value: "0.0.0.0:" + strconv.Itoa(int(md.Spec.Runtime.Port))},
		)
	case v1alpha1.EngineVLLM:
		env = append(env, corev1.EnvVar{Name: "HF_HOME", Value: path.Join(mount, "hf")})
	}
	return env
}

// servingEnv adds the variables that only make sense in the serving pod.
func servingEnv(md *v1alpha1.ModelDeployment) []corev1.EnvVar {
	env := runtimeEnv(md)
	if md.Spec.WeightCache != nil && Engine(md) == v1alpha1.EngineVLLM {
		// With a warm cache the weights are guaranteed to be local. Forcing the
		// hub offline turns a silent multi-minute re-download — the usual way a
		// "warm" pod is still slow — into an immediate, visible failure.
		env = append(env, corev1.EnvVar{Name: "HF_HUB_OFFLINE", Value: "1"})
	}
	return env
}

// ServingArgs builds the runtime command line. Extra arguments are appended last
// so that a user flag overrides a generated one on engines that take the last
// occurrence.
func ServingArgs(md *v1alpha1.ModelDeployment) []string {
	if Engine(md) == v1alpha1.EngineOllama {
		// Ollama is configured through the environment; the image entrypoint
		// already runs the server.
		return append([]string{"serve"}, md.Spec.Runtime.ExtraArgs...)
	}
	args := []string{
		"--model", md.Spec.Model.Name,
		"--served-model-name", md.Spec.Model.Name,
		"--revision", md.Spec.Model.Revision,
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(int(md.Spec.Runtime.Port)),
	}
	return append(args, md.Spec.Runtime.ExtraArgs...)
}
