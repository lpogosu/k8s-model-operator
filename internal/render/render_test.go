package render

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lpogosu/k8s-model-operator/api/v1alpha1"
)

// fixture mirrors what the API server hands the controller: every default
// already filled in. TestCRDDefaults in the controller package is what keeps
// these values honest.
func fixture(mutate func(*v1alpha1.ModelDeployment)) *v1alpha1.ModelDeployment {
	size := resource.MustParse("200Gi")
	md := &v1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "inference"},
		Spec: v1alpha1.ModelDeploymentSpec{
			Model: v1alpha1.ModelSpec{Name: "Qwen/Qwen2.5-7B-Instruct", Revision: "main"},
			Runtime: v1alpha1.RuntimeSpec{
				Engine:             v1alpha1.EngineVLLM,
				Port:               8000,
				LoadTimeoutSeconds: 900,
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("1"),
				}},
			},
			Replicas: 2,
			WeightCache: &v1alpha1.WeightCacheSpec{
				Size:               &size,
				MountPath:          "/models",
				WarmTimeoutSeconds: 3600,
			},
			Disruption: v1alpha1.DisruptionSpec{MinAvailable: 1},
		},
	}
	if mutate != nil {
		mutate(md)
	}
	return md
}

func TestCacheKeyDependsOnWeightsAndNothingElse(t *testing.T) {
	base := fixture(nil)
	key := CacheKey(base)

	same := []struct {
		name   string
		mutate func(*v1alpha1.ModelDeployment)
	}{
		{"a different image serves the same bytes", func(md *v1alpha1.ModelDeployment) {
			md.Spec.Runtime.Image = "vllm/vllm-openai:v0.12.0"
		}},
		{"replica count is not a property of the weights", func(md *v1alpha1.ModelDeployment) {
			md.Spec.Replicas = 9
		}},
		{"serving flags do not change what was downloaded", func(md *v1alpha1.ModelDeployment) {
			md.Spec.Runtime.ExtraArgs = []string{"--max-model-len", "8192"}
		}},
		{"the name of the object is not part of the cache", func(md *v1alpha1.ModelDeployment) {
			md.Name = "other"
		}},
	}
	for _, tc := range same {
		t.Run(tc.name, func(t *testing.T) {
			if got := CacheKey(fixture(tc.mutate)); got != key {
				t.Errorf("cache key changed to %q, want it unchanged at %q", got, key)
			}
		})
	}

	differ := []struct {
		name   string
		mutate func(*v1alpha1.ModelDeployment)
	}{
		{"a different model", func(md *v1alpha1.ModelDeployment) { md.Spec.Model.Name = "Qwen/Qwen2.5-14B-Instruct" }},
		{"a different revision", func(md *v1alpha1.ModelDeployment) { md.Spec.Model.Revision = "refs/pr/12" }},
		{"a different engine stores weights differently", func(md *v1alpha1.ModelDeployment) {
			md.Spec.Runtime.Engine = v1alpha1.EngineOllama
		}},
	}
	for _, tc := range differ {
		t.Run(tc.name, func(t *testing.T) {
			if got := CacheKey(fixture(tc.mutate)); got == key {
				t.Errorf("cache key stayed %q, want it to change", got)
			}
		})
	}

	if len(key) != 12 {
		t.Errorf("cache key %q is %d characters; it is used in object names and must stay short", key, len(key))
	}
	if CacheKey(base) != key {
		t.Error("the cache key must be stable across calls")
	}
}

func TestPDBMinAvailable(t *testing.T) {
	tests := []struct {
		name      string
		requested int32
		replicas  int32
		want      int32
		wantPDB   bool
	}{
		{"a single replica gets no budget at all", 1, 1, 0, false},
		{"a scaled-to-zero deployment gets no budget", 1, 0, 0, false},
		{"two replicas keep one available", 1, 2, 1, true},
		{"a budget larger than the deployment is capped below it", 5, 3, 2, true},
		{"asking for none disables the budget", 0, 5, 0, false},
		{"the cap leaves room for exactly one eviction", 3, 4, 3, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := fixture(func(md *v1alpha1.ModelDeployment) {
				md.Spec.Disruption.MinAvailable = tc.requested
			})
			got, ok := PDBMinAvailable(md, tc.replicas)
			if ok != tc.wantPDB {
				t.Fatalf("wantPDB = %v, got %v", tc.wantPDB, ok)
			}
			if ok && got != tc.want {
				t.Errorf("minAvailable = %d, want %d", got, tc.want)
			}
			if ok && got >= tc.replicas {
				t.Errorf("minAvailable %d must leave room for one eviction out of %d replicas", got, tc.replicas)
			}
		})
	}
}

func TestServingArgs(t *testing.T) {
	vllm := ServingArgs(fixture(nil))
	for _, want := range []string{"--model", "Qwen/Qwen2.5-7B-Instruct", "--revision", "main", "--port", "8000"} {
		if !slices.Contains(vllm, want) {
			t.Errorf("vLLM args %v are missing %q", vllm, want)
		}
	}

	withExtra := ServingArgs(fixture(func(md *v1alpha1.ModelDeployment) {
		md.Spec.Runtime.ExtraArgs = []string{"--max-model-len", "8192"}
	}))
	if withExtra[len(withExtra)-2] != "--max-model-len" {
		t.Errorf("extra args must come last so they win on duplicates, got %v", withExtra)
	}

	ollama := ServingArgs(fixture(func(md *v1alpha1.ModelDeployment) {
		md.Spec.Runtime.Engine = v1alpha1.EngineOllama
	}))
	if len(ollama) != 1 || ollama[0] != "serve" {
		t.Errorf("Ollama args = %v, want just [serve]: it is configured through the environment", ollama)
	}
}

func TestDeploymentGatesOnTheCache(t *testing.T) {
	md := fixture(nil)
	key := CacheKey(md)
	dep := Deployment(md, key, 2, true)

	if !dep.Spec.Paused {
		t.Error("a paused deployment is what keeps the old pods serving during a model switch")
	}
	if dep.Spec.Template.Annotations[AnnCacheKey] != key {
		t.Error("the pod template must carry the cache key so a revision bump rolls the pods")
	}
	if got := dep.Spec.Selector.MatchLabels[LabelCacheKey]; got != "" {
		t.Error("the cache key must not be part of the selector: a change would orphan the running pods")
	}

	init := dep.Spec.Template.Spec.InitContainers
	if len(init) != 1 {
		t.Fatalf("want one init container gating on the marker, got %d", len(init))
	}
	if !strings.Contains(init[0].Command[2], MarkerPath(md, key)) {
		t.Errorf("the init container must wait on %s, got %q", MarkerPath(md, key), init[0].Command[2])
	}
	if init[0].Image != Image(md) {
		t.Error("reusing the serving image saves the node a second pull")
	}

	server := dep.Spec.Template.Spec.Containers[0]
	if server.StartupProbe.FailureThreshold*server.StartupProbe.PeriodSeconds < md.Spec.Runtime.LoadTimeoutSeconds {
		t.Errorf("the startup probe budget (%ds) is shorter than the load timeout (%ds), so liveness would kill a loading pod",
			server.StartupProbe.FailureThreshold*server.StartupProbe.PeriodSeconds, md.Spec.Runtime.LoadTimeoutSeconds)
	}
	if !hasEnv(server.Env, "HF_HUB_OFFLINE", "1") {
		t.Error("with a warm cache the runtime must be forbidden from silently re-downloading")
	}
	if !hasEnv(server.Env, "HF_HOME", "/models/hf") {
		t.Error("the serving container must read the weights from the shared mount")
	}

	shm := findVolume(dep.Spec.Template.Spec.Volumes, shmVol)
	if shm == nil || shm.EmptyDir == nil || shm.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Error("vLLM needs more than the 64Mi of /dev/shm a container gets by default")
	}
	weights := findVolume(dep.Spec.Template.Spec.Volumes, weightsVol)
	if weights == nil || weights.PersistentVolumeClaim == nil || weights.PersistentVolumeClaim.ClaimName != PVCName(md) {
		t.Error("the weights volume must be the shared claim")
	}
}

func TestDeploymentWithoutCacheUsesAnEmptyDir(t *testing.T) {
	md := fixture(func(md *v1alpha1.ModelDeployment) { md.Spec.WeightCache = nil })
	dep := Deployment(md, CacheKey(md), 1, false)

	if len(dep.Spec.Template.Spec.InitContainers) != 0 {
		t.Error("there is no marker to wait for without a shared cache")
	}
	weights := findVolume(dep.Spec.Template.Spec.Volumes, weightsVol)
	if weights == nil || weights.EmptyDir == nil {
		t.Fatal("the weights volume must fall back to an emptyDir")
	}
	if hasEnv(dep.Spec.Template.Spec.Containers[0].Env, "HF_HUB_OFFLINE", "1") {
		t.Error("without a cache the runtime has to be allowed to download")
	}
}

func TestWarmJobDownloadsBeforeWritingTheMarker(t *testing.T) {
	md := fixture(nil)
	key := CacheKey(md)
	job := WarmJob(md, key)

	if job.Name != WarmJobName(md, key) {
		t.Errorf("job name = %q, want the cache key in it", job.Name)
	}
	if *job.Spec.ActiveDeadlineSeconds != 3600 {
		t.Errorf("activeDeadlineSeconds = %d, want the configured warm timeout", *job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Error("a fresh pod per attempt is what keeps the failed attempt's logs")
	}

	script := job.Spec.Template.Spec.Containers[0].Command[2]
	download := strings.Index(script, "download")
	marker := strings.Index(script, "$MARKER\"")
	if download < 0 || marker < 0 || download > marker {
		t.Error("the marker must be written after the download, otherwise a partial cache reads as warm")
	}
	if !hasEnv(job.Spec.Template.Spec.Containers[0].Env, "MARKER", MarkerPath(md, key)) {
		t.Error("the job and the serving pod must agree on the marker path")
	}

	ollama := WarmJob(fixture(func(md *v1alpha1.ModelDeployment) {
		md.Spec.Runtime.Engine = v1alpha1.EngineOllama
	}), key)
	ollamaScript := ollama.Spec.Template.Spec.Containers[0].Command[2]
	if !strings.Contains(ollamaScript, "ollama serve") || !strings.Contains(ollamaScript, "ollama pull") {
		t.Error("ollama pull needs a server in the same pod to write into the shared volume")
	}
}

func TestPVCDemandsReadWriteMany(t *testing.T) {
	pvc := PVC(fixture(nil))
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("access modes = %v; a single-node cache cannot serve pods on other nodes", pvc.Spec.AccessModes)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "200Gi" {
		t.Errorf("size = %s, want 200Gi", got.String())
	}
}

func TestExistingClaimIsNeverOwned(t *testing.T) {
	shared := fixture(func(md *v1alpha1.ModelDeployment) {
		md.Spec.WeightCache.ExistingClaimName = "team-model-cache"
	})
	if PVCName(shared) != "team-model-cache" {
		t.Errorf("claim name = %q, want the one the user pointed at", PVCName(shared))
	}
	if OwnsPVC(shared) {
		t.Error("a shared claim may hold weights another ModelDeployment still serves; it must not be garbage collected")
	}
	if !OwnsPVC(fixture(nil)) {
		t.Error("a claim the operator created is its own to delete")
	}
}

func TestEndpointAndHealthPath(t *testing.T) {
	if got, want := Endpoint(fixture(nil)), "http://qwen.inference.svc:8000"; got != want {
		t.Errorf("endpoint = %q, want %q", got, want)
	}
	if got := HealthPath(v1alpha1.EngineVLLM); got != "/health" {
		t.Errorf("vLLM health path = %q", got)
	}
	if got := HealthPath(v1alpha1.EngineOllama); got != "/api/tags" {
		t.Errorf("Ollama health path = %q", got)
	}
}

func TestImageDefaults(t *testing.T) {
	if got := Image(fixture(nil)); !strings.HasPrefix(got, "vllm/vllm-openai:") {
		t.Errorf("default vLLM image = %q", got)
	}
	pinned := fixture(func(md *v1alpha1.ModelDeployment) { md.Spec.Runtime.Image = "registry.local/vllm@sha256:abc" })
	if got := Image(pinned); got != "registry.local/vllm@sha256:abc" {
		t.Errorf("image override ignored, got %q", got)
	}
	if got := WarmImage(pinned); got != Image(pinned) {
		t.Errorf("the warm job should reuse the serving image by default, got %q", got)
	}
}

func hasEnv(env []corev1.EnvVar, name, value string) bool {
	for _, e := range env {
		if e.Name == name {
			return e.Value == value
		}
	}
	return false
}

func findVolume(volumes []corev1.Volume, name string) *corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}
	return nil
}
