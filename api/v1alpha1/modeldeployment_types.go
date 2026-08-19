package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Engine identifies the inference server that serves the model.
// +kubebuilder:validation:Enum=vllm;ollama
type Engine string

const (
	// EngineVLLM is an OpenAI-compatible vLLM server.
	EngineVLLM Engine = "vllm"
	// EngineOllama is an Ollama server.
	EngineOllama Engine = "ollama"
)

// Condition types reported on ModelDeployment.status.
const (
	// ConditionReady is true when the requested number of replicas serve traffic.
	ConditionReady = "Ready"
	// ConditionWeightsCached is true when the shared cache holds the weights for
	// the current cache key, so a new pod can start without downloading them.
	ConditionWeightsCached = "WeightsCached"
	// ConditionDegraded is true when the deployment serves, but something the
	// user asked for is not working — most often the queue-depth metric.
	ConditionDegraded = "Degraded"
)

// Condition reasons. They are part of the API contract: users alert on them.
const (
	ReasonWarming             = "Warming"
	ReasonWarmFailed          = "WarmJobFailed"
	ReasonCacheDisabled       = "CacheDisabled"
	ReasonCached              = "Cached"
	ReasonReplicasReady       = "ReplicasReady"
	ReasonReplicasUnavailable = "ReplicasUnavailable"
	ReasonWaitingForWeights   = "WaitingForWeights"
	ReasonQueueMetricMissing  = "QueueMetricUnavailable"
	ReasonScaledToZero        = "ScaledToZero"
	ReasonAsExpected          = "AsExpected"
)

// ModelSpec identifies the weights to serve.
type ModelSpec struct {
	// Name is the model identifier passed to the runtime, for example
	// "Qwen/Qwen2.5-7B-Instruct" for vLLM or "llama3.1:8b" for Ollama.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Revision pins the model version. For Hugging Face repositories this is a
	// branch, tag or commit SHA. Changing it changes the cache key, which is the
	// only way the operator can tell two sets of weights apart.
	// +kubebuilder:default="main"
	// +optional
	Revision string `json:"revision,omitempty"`

	// SecretRef names a Secret whose keys are exposed to the runtime and to the
	// warm-up Job as environment variables. Use it for HF_TOKEN on gated models.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`
}

// RuntimeSpec configures the inference server container.
type RuntimeSpec struct {
	// Engine selects the inference server. It changes the default image, the
	// serving arguments and the health endpoint.
	// +kubebuilder:default=vllm
	Engine Engine `json:"engine,omitempty"`

	// Image overrides the container image. It must be pinned by digest in any
	// environment where reproducibility matters; the built-in defaults are tags.
	// +optional
	Image string `json:"image,omitempty"`

	// ExtraArgs are appended to the generated runtime arguments, after them, so
	// they win on duplicate flags.
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`

	// Port is the port the runtime listens on.
	// +kubebuilder:default=8000
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// LoadTimeoutSeconds bounds how long a pod may spend loading weights into GPU
	// memory before it is declared broken. It becomes the startup probe budget,
	// so it must exceed the real load time of the largest model served here.
	// +kubebuilder:default=900
	// +kubebuilder:validation:Minimum=30
	// +optional
	LoadTimeoutSeconds int32 `json:"loadTimeoutSeconds,omitempty"`

	// Resources are the requests and limits of the serving container. GPUs are
	// requested through the usual extended resource, for example
	// "nvidia.com/gpu: 1" — the operator does not invent its own GPU field.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector constrains serving pods, typically to a GPU node pool.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations let serving pods land on tainted GPU nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// WeightCacheSpec configures the shared volume that holds downloaded weights.
// +kubebuilder:validation:XValidation:rule="has(self.size) || has(self.existingClaimName)",message="either size or existingClaimName must be set"
type WeightCacheSpec struct {
	// Size is the requested capacity of the cache volume. It must hold every
	// revision that is still referenced, not just the current one. Required
	// unless ExistingClaimName is used.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// StorageClassName selects the storage class. It has to provide
	// ReadWriteMany, otherwise only one node can mount the cache and scale-up
	// stops at that node's capacity.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// ExistingClaimName reuses a PersistentVolumeClaim instead of creating one.
	// The claim is not owned by the ModelDeployment and survives its deletion,
	// which is the point: several ModelDeployments can share one cache.
	// +optional
	ExistingClaimName string `json:"existingClaimName,omitempty"`

	// MountPath is where the cache is mounted in the runtime and warm-up pods.
	// +kubebuilder:default="/models"
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// WarmImage runs the download Job. It defaults to the runtime image, which
	// already contains the client libraries needed to fetch the weights.
	// +optional
	WarmImage string `json:"warmImage,omitempty"`

	// WarmTimeoutSeconds fails the warm-up Job if the download takes longer.
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=60
	// +optional
	WarmTimeoutSeconds int32 `json:"warmTimeoutSeconds,omitempty"`
}

// AutoscalingSpec turns on queue-depth based scaling. When it is nil the replica
// count is exactly Replicas and the operator never changes it.
type AutoscalingSpec struct {
	// MinReplicas is the floor. Zero is allowed and means the deployment scales
	// to nothing when idle, at the cost of a cold start on the next request.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the ceiling. On GPU nodes this is usually the number of
	// GPUs available, not a load estimate.
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// TargetQueueDepth is how many requests may wait per replica before another
	// replica is added. This is the scaling signal: the runtime's own count of
	// requests admitted but not yet generating.
	// +kubebuilder:default=4
	// +kubebuilder:validation:Minimum=1
	// +optional
	TargetQueueDepth int32 `json:"targetQueueDepth,omitempty"`

	// MetricName is the custom metric served by the metrics adapter. The default
	// is the counter vLLM exports; Ollama does not export an equivalent.
	// +kubebuilder:default="vllm:num_requests_waiting"
	// +optional
	MetricName string `json:"metricName,omitempty"`

	// ScaleUpStabilizationSeconds is the minimum age of the last scaling change
	// before scaling up again. Kept short: a queue that is growing costs latency
	// on every request in it.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=0
	// +optional
	ScaleUpStabilizationSeconds int32 `json:"scaleUpStabilizationSeconds,omitempty"`

	// ScaleDownStabilizationSeconds is the same for scaling down. Kept long: a
	// removed replica costs minutes to bring back, so flapping is expensive in a
	// way it is not for stateless HTTP services.
	// +kubebuilder:default=900
	// +kubebuilder:validation:Minimum=0
	// +optional
	ScaleDownStabilizationSeconds int32 `json:"scaleDownStabilizationSeconds,omitempty"`

	// MaxScaleUpStep bounds how many replicas may be added at once. Each replica
	// claims a GPU, so an unbounded jump on a metric spike is an expensive way to
	// discover that the metric was wrong.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxScaleUpStep int32 `json:"maxScaleUpStep,omitempty"`
}

// DisruptionSpec configures the PodDisruptionBudget guarding the serving pods.
type DisruptionSpec struct {
	// MinAvailable is how many replicas must survive a voluntary disruption such
	// as a node drain. It is capped at Replicas-1 by the controller, because a
	// budget that can never be satisfied blocks drains forever.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinAvailable int32 `json:"minAvailable,omitempty"`
}

// ModelDeploymentSpec is the desired state of a served model.
type ModelDeploymentSpec struct {
	// Model identifies the weights.
	Model ModelSpec `json:"model"`

	// Runtime configures the inference server.
	// +kubebuilder:default={}
	// +optional
	Runtime RuntimeSpec `json:"runtime,omitempty"`

	// Replicas is the desired replica count and the only place it is stored.
	// With Autoscaling set the operator writes its own decision back here, the
	// way a HorizontalPodAutoscaler writes to a Deployment: one number, visible
	// in `kubectl get`, reachable through the scale subresource.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// WeightCache configures the shared weight volume. Nil disables it, and then
	// every pod downloads the weights into its own emptyDir — correct, but it
	// costs the full download on every scale-up. It is not enabled by default
	// because it requires a ReadWriteMany storage class, which a default cluster
	// does not have.
	// +optional
	WeightCache *WeightCacheSpec `json:"weightCache,omitempty"`

	// Autoscaling enables queue-depth scaling. Nil means a fixed replica count.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// Disruption configures the PodDisruptionBudget.
	// +kubebuilder:default={}
	// +optional
	Disruption DisruptionSpec `json:"disruption,omitempty"`
}

// ModelDeploymentStatus is the observed state.
type ModelDeploymentStatus struct {
	// ObservedGeneration is the spec generation this status was computed from.
	// Anything comparing status to spec must check it first.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries Ready, WeightsCached and Degraded.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CacheKey identifies the weights currently being served. It changes when
	// the model, its revision or the engine changes, and that change is what
	// invalidates the cache and triggers a new warm-up.
	// +optional
	CacheKey string `json:"cacheKey,omitempty"`

	// Replicas is how many pods the Deployment reports. It is the status half of
	// the scale subresource, so it must count real pods, not intentions.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is how many of them serve traffic.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// ObservedQueueDepth is the last queue depth read from the metrics adapter.
	// +optional
	ObservedQueueDepth int32 `json:"observedQueueDepth,omitempty"`

	// LastScaleTime is when the operator last changed the replica count. The
	// stabilization windows are measured from it.
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`

	// Endpoint is the in-cluster address of the OpenAI-compatible API.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Selector is the label selector of the serving pods, in the string form the
	// scale subresource requires.
	// +optional
	Selector string `json:"selector,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:shortName=md;modeldep
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model.name`
// +kubebuilder:printcolumn:name="Engine",type=string,JSONPath=`.spec.runtime.engine`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Cached",type=string,JSONPath=`.status.conditions[?(@.type=="WeightsCached")].status`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelDeployment serves a single model with a single runtime configuration.
type ModelDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelDeploymentSpec   `json:"spec,omitempty"`
	Status ModelDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelDeploymentList is a list of ModelDeployment.
type ModelDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelDeployment `json:"items"`
}
