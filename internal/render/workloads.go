package render

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/lpogosu/k8s-model-operator/api/v1alpha1"
)

const (
	portName     = "http"
	weightsVol   = "weights"
	shmVol       = "dshm"
	initWaitName = "wait-for-weights"
)

// shmSize is the size of /dev/shm inside the serving pod. The container runtime
// default is 64Mi, and every vLLM deployment with tensor parallelism above one
// dies on that limit inside NCCL — with an error that does not mention /dev/shm.
var shmSize = resource.MustParse("2Gi")

func ptr[T any](v T) *T { return &v }

// Deployment renders the serving Deployment.
//
// paused expresses the central rule of this operator: a rollout to weights that
// are not on the cache yet is held back, so the pods that serve the previous
// model keep serving it instead of being replaced by pods that will spend the
// next ten minutes downloading. Replicas still scale while paused — the
// Deployment controller scales a paused Deployment, it only refuses to roll it.
func Deployment(md *v1alpha1.ModelDeployment, cacheKey string, replicas int32, paused bool) *appsv1.Deployment {
	labels := PodLabels(md)
	rt := md.Spec.Runtime
	probe := func(period, failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: HealthPath(Engine(md)),
				Port: intstr.FromString(portName),
			}},
			PeriodSeconds:    period,
			TimeoutSeconds:   3,
			FailureThreshold: failures,
		}
	}

	container := corev1.Container{
		Name:      "server",
		Image:     Image(md),
		Args:      ServingArgs(md),
		Env:       servingEnv(md),
		Resources: rt.Resources,
		Ports: []corev1.ContainerPort{{
			Name:          portName,
			ContainerPort: rt.Port,
			Protocol:      corev1.ProtocolTCP,
		}},
		VolumeMounts: []corev1.VolumeMount{
			{Name: weightsVol, MountPath: MountPath(md)},
			{Name: shmVol, MountPath: "/dev/shm"},
		},
		// The startup probe is what keeps liveness from killing a pod that is
		// simply still loading forty gigabytes into GPU memory. Its budget is the
		// user's load timeout; only after it passes do the other two probes start.
		StartupProbe:   probe(10, max(rt.LoadTimeoutSeconds/10, 1)),
		ReadinessProbe: probe(5, 3),
		LivenessProbe:  probe(20, 3),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	if md.Spec.Model.SecretRef != nil {
		container.EnvFrom = []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: *md.Spec.Model.SecretRef},
		}}
	}

	pod := corev1.PodSpec{
		Containers:   []corev1.Container{container},
		Volumes:      []corev1.Volume{weightsVolume(md), shmVolume()},
		NodeSelector: rt.NodeSelector,
		Tolerations:  rt.Tolerations,
		// A generation in flight is not worth aborting: dropping it costs the
		// client the whole request, not a retryable fragment of one.
		TerminationGracePeriodSeconds: ptr(int64(120)),
	}
	if md.Spec.WeightCache != nil {
		pod.InitContainers = []corev1.Container{waitForWeights(md, cacheKey)}
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DeploymentName(md),
			Namespace: md.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(replicas),
			Paused:   paused,
			Selector: &metav1.LabelSelector{MatchLabels: SelectorLabels(md)},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					// Never trade away serving capacity during a model switch: the
					// replacement pod has to be Ready before the old one goes.
					MaxUnavailable: ptr(intstr.FromInt32(0)),
					MaxSurge:       ptr(intstr.FromInt32(1)),
				},
			},
			// A pod that answers /health once and then dies should not count as a
			// completed rollout step.
			MinReadySeconds: 15,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					// Without this annotation a revision bump would leave the pod
					// spec unchanged and the running pods would keep the old weights.
					Annotations: map[string]string{AnnCacheKey: cacheKey},
				},
				Spec: pod,
			},
		},
	}
}

// waitForWeights guards the window in which the warm-up Job has finished but the
// shared volume has not made the marker visible on this node yet. It reuses the
// serving image so the node pulls one image instead of two, and it gives up
// rather than hanging forever: a pod stuck in Init is much harder to notice than
// one in CrashLoopBackOff.
func waitForWeights(md *v1alpha1.ModelDeployment, cacheKey string) corev1.Container {
	marker := MarkerPath(md, cacheKey)
	script := fmt.Sprintf(`deadline=$(( $(date +%%s) + %d ))
while [ ! -f "%s" ]; do
  if [ "$(date +%%s)" -ge "$deadline" ]; then
    echo "weight cache marker %s never appeared" >&2
    exit 1
  fi
  sleep 5
done`, md.Spec.Runtime.LoadTimeoutSeconds, marker, marker)

	return corev1.Container{
		Name:         initWaitName,
		Image:        Image(md),
		Command:      []string{"/bin/sh", "-euc", script},
		VolumeMounts: []corev1.VolumeMount{{Name: weightsVol, MountPath: MountPath(md), ReadOnly: true}},
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			ReadOnlyRootFilesystem:   ptr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
}

func weightsVolume(md *v1alpha1.ModelDeployment) corev1.Volume {
	if md.Spec.WeightCache == nil {
		return corev1.Volume{
			Name:         weightsVol,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}
	}
	return corev1.Volume{
		Name: weightsVol,
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: PVCName(md),
		}},
	}
}

func shmVolume() corev1.Volume {
	return corev1.Volume{
		Name: shmVol,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium:    corev1.StorageMediumMemory,
			SizeLimit: &shmSize,
		}},
	}
}

// Service exposes the runtime inside the cluster. Endpoints only ever contain
// Ready pods, which is what keeps a request from reaching a replica that is
// still loading its weights — the operator does not have to do anything for
// that beyond making readiness mean what it should.
func Service(md *v1alpha1.ModelDeployment) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceName(md),
			Namespace: md.Namespace,
			Labels:    PodLabels(md),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: SelectorLabels(md),
			Ports: []corev1.ServicePort{{
				Name:       portName,
				Port:       md.Spec.Runtime.Port,
				TargetPort: intstr.FromString(portName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// PDBMinAvailable returns the budget to write, and whether a budget makes sense
// at all. With one replica any budget above zero blocks every node drain
// forever, which is a much worse outage than the one it was meant to prevent.
func PDBMinAvailable(md *v1alpha1.ModelDeployment, replicas int32) (int32, bool) {
	if replicas < 2 {
		return 0, false
	}
	requested := md.Spec.Disruption.MinAvailable
	if requested < 1 {
		return 0, false
	}
	return min(requested, replicas-1), true
}

// PDB renders the PodDisruptionBudget.
func PDB(md *v1alpha1.ModelDeployment, minAvailable int32) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: policyv1.SchemeGroupVersion.String(), Kind: "PodDisruptionBudget"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      PDBName(md),
			Namespace: md.Namespace,
			Labels:    PodLabels(md),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr(intstr.FromInt32(minAvailable)),
			Selector:     &metav1.LabelSelector{MatchLabels: SelectorLabels(md)},
		},
	}
}

// PVC renders the weight cache claim.
//
// ReadWriteMany is not a preference, it is the requirement that makes the whole
// design work: the warm-up Job writes the weights from one node and every
// serving pod, wherever it lands, reads them. On a cluster whose only storage is
// ReadWriteOnce the cache has to be left unset.
func PVC(md *v1alpha1.ModelDeployment) *corev1.PersistentVolumeClaim {
	cache := md.Spec.WeightCache
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      PVCName(md),
			Namespace: md.Namespace,
			Labels:    PodLabels(md),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: cache.StorageClassName,
		},
	}
	if cache.Size != nil {
		pvc.Spec.Resources = corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: *cache.Size},
		}
	}
	return pvc
}

// WarmJob downloads the weights onto the shared cache.
//
// It requests no GPU: pulling files off the network does not need one, and a Job
// that asked for a GPU would sit pending behind the very serving pods it is
// supposed to unblock.
func WarmJob(md *v1alpha1.ModelDeployment, cacheKey string) *batchv1.Job {
	labels := map[string]string{
		"app.kubernetes.io/name":       "model-weight-warmer",
		"app.kubernetes.io/instance":   md.Name,
		"app.kubernetes.io/managed-by": FieldOwner,
		LabelInstance:                  md.Name,
		LabelRole:                      RoleWarm,
		LabelCacheKey:                  cacheKey,
	}
	env := append(runtimeEnv(md), corev1.EnvVar{Name: "MARKER", Value: MarkerPath(md, cacheKey)})
	container := corev1.Container{
		Name:         "download",
		Image:        WarmImage(md),
		Command:      []string{"/bin/sh", "-euc", warmScript(Engine(md))},
		Env:          env,
		VolumeMounts: []corev1.VolumeMount{{Name: weightsVol, MountPath: MountPath(md)}},
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	if md.Spec.Model.SecretRef != nil {
		container.EnvFrom = []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: *md.Spec.Model.SecretRef},
		}}
	}

	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      WarmJobName(md, cacheKey),
			Namespace: md.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr(int32(3)),
			// A download that has not finished within the budget is not going to:
			// something is wrong with the network or the revision does not exist.
			ActiveDeadlineSeconds: ptr(int64(md.Spec.WeightCache.WarmTimeoutSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// Never, not OnFailure: a fresh pod per attempt keeps the logs
					// of the failed one around, which is where the reason is.
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{container},
					Volumes:       []corev1.Volume{weightsVolume(md)},
				},
			},
		},
	}
}

// warmScript writes the marker only after the download returned successfully, so
// a half-transferred cache can never be mistaken for a warm one. A partially
// written marker is not a concern: the file is a few bytes and is written last.
func warmScript(engine v1alpha1.Engine) string {
	if engine == v1alpha1.EngineOllama {
		return `mkdir -p "$OLLAMA_MODELS"
ollama serve &
server=$!
# "ollama pull" is a client command; it needs a server in this same pod because
# the shared volume, not the network, is what the weights have to end up on.
until ollama list >/dev/null 2>&1; do sleep 1; done
ollama pull "$MODEL_NAME"
kill "$server"
mkdir -p "$(dirname "$MARKER")"
date -u +%Y-%m-%dT%H:%M:%SZ > "$MARKER"`
	}
	return `mkdir -p "$HF_HOME"
# "hf" replaced "huggingface-cli" in huggingface_hub 0.34; runtime images in the
# wild still ship either one.
if command -v hf >/dev/null 2>&1; then
  hf download "$MODEL_NAME" --revision "$MODEL_REVISION"
else
  huggingface-cli download "$MODEL_NAME" --revision "$MODEL_REVISION"
fi
mkdir -p "$(dirname "$MARKER")"
date -u +%Y-%m-%dT%H:%M:%SZ > "$MARKER"`
}
