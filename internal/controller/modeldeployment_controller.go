// Package controller reconciles ModelDeployment objects into the workloads that
// serve a model.
package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lpogosu/k8s-model-operator/api/v1alpha1"
	"github.com/lpogosu/k8s-model-operator/internal/queue"
	"github.com/lpogosu/k8s-model-operator/internal/render"
	"github.com/lpogosu/k8s-model-operator/internal/scaling"
)

// resyncInterval is how often a ModelDeployment with autoscaling enabled is
// reconciled without an event. The queue depth lives outside the API server, so
// nothing wakes the controller when it changes; polling is the only option.
const resyncInterval = 30 * time.Second

// terminationPollInterval is how often teardown re-checks for surviving pods.
const terminationPollInterval = 5 * time.Second

// ModelDeploymentReconciler reconciles a ModelDeployment.
type ModelDeploymentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Queue reads the scaling signal. It may be nil, in which case a
	// ModelDeployment that asks for autoscaling is reported as Degraded instead
	// of silently running at a fixed size.
	Queue queue.Source

	// Now is injected so that the stabilization windows can be tested.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=serving.lpogosu.dev,resources=modeldeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.lpogosu.dev,resources=modeldeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=serving.lpogosu.dev,resources=modeldeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=custom.metrics.k8s.io,resources=*,verbs=get;list

// Reconcile drives one ModelDeployment towards its spec.
func (r *ModelDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	md := &v1alpha1.ModelDeployment{}
	if err := r.Get(ctx, req.NamespacedName, md); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !md.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, md)
	}

	// The finalizer has to be in place before the first child object is created,
	// otherwise a deletion racing the first reconcile leaves pods behind.
	if controllerutil.AddFinalizer(md, render.Finalizer) {
		return ctrl.Result{}, r.Update(ctx, md)
	}

	return r.sync(ctx, md)
}

func (r *ModelDeploymentReconciler) sync(ctx context.Context, md *v1alpha1.ModelDeployment) (ctrl.Result, error) {
	cacheKey := render.CacheKey(md)
	st := newStatusBuilder(md, cacheKey)

	cache, err := r.reconcileWeightCache(ctx, md, cacheKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	st.applyCache(cache)

	replicas, scaleErr := r.decideReplicas(ctx, md, cache.warm, st)
	if scaleErr != nil {
		// A missing metrics adapter must not stop the rest of the reconcile: the
		// model should keep serving at whatever size it has.
		st.scaleErr = scaleErr
		log.FromContext(ctx).Info("queue depth unavailable, holding replicas", "replicas", replicas, "error", scaleErr)
	}
	if replicas != md.Spec.Replicas {
		// The replica count lives in exactly one place. Writing the autoscaler's
		// decision back to the spec is what keeps `kubectl get`, the scale
		// subresource and the Deployment from each showing a different number.
		md.Spec.Replicas = replicas
		if err := r.Update(ctx, md); err != nil {
			return ctrl.Result{}, fmt.Errorf("write replica decision: %w", err)
		}
	}

	// Holding back the rollout is the operator's whole reason to exist. While the
	// weights for the current spec are not on the cache, the Deployment is paused:
	// pods serving the previous model keep serving it, and no pod is created that
	// would sit on a GPU downloading.
	paused := md.Spec.WeightCache != nil && !cache.warm
	if err := r.applyOwned(ctx, md, render.Deployment(md, cacheKey, replicas, paused)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyOwned(ctx, md, render.Service(md)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcilePDB(ctx, md, replicas); err != nil {
		return ctrl.Result{}, err
	}

	observed, err := r.observeDeployment(ctx, md)
	if err != nil {
		return ctrl.Result{}, err
	}
	st.applyWorkload(observed, paused)

	if err := r.writeStatus(ctx, md, st); err != nil {
		return ctrl.Result{}, err
	}

	if md.Spec.Autoscaling != nil {
		return ctrl.Result{RequeueAfter: resyncInterval}, nil
	}
	return ctrl.Result{}, nil
}

// decideReplicas returns the replica count to write and, separately, the error
// that made autoscaling impossible. Both are returned: the caller needs the
// count to keep serving and the error to report Degraded.
func (r *ModelDeploymentReconciler) decideReplicas(
	ctx context.Context,
	md *v1alpha1.ModelDeployment,
	warm bool,
	st *statusBuilder,
) (int32, error) {
	current := md.Spec.Replicas
	as := md.Spec.Autoscaling
	if as == nil {
		// Without an autoscaling policy the spec is the only input, which is also
		// what makes `kubectl scale` work through the scale subresource.
		return current, nil
	}
	if r.Queue == nil {
		return current, fmt.Errorf("autoscaling is enabled but the operator was started without a queue metrics source")
	}

	sel := labels.SelectorFromSet(render.SelectorLabels(md))
	depth, err := r.Queue.Depth(ctx, md.Namespace, as.MetricName, sel)
	if err != nil {
		return current, err
	}
	st.queueDepth = depth

	decision := scaling.Decide(scaling.Policy{
		MinReplicas:      as.MinReplicas,
		MaxReplicas:      as.MaxReplicas,
		TargetQueueDepth: as.TargetQueueDepth,
		MaxScaleUpStep:   as.MaxScaleUpStep,
		ScaleUpAfter:     time.Duration(as.ScaleUpStabilizationSeconds) * time.Second,
		ScaleDownAfter:   time.Duration(as.ScaleDownStabilizationSeconds) * time.Second,
	}, scaling.State{
		CurrentReplicas: current,
		QueueDepth:      depth,
		WeightsCached:   warm || md.Spec.WeightCache == nil,
		LastScale:       lastScaleTime(md),
		Now:             r.now(),
	})

	if decision.Changed {
		st.lastScale = &metav1.Time{Time: r.now()}
		r.Recorder.Eventf(md, nil, corev1.EventTypeNormal, string(decision.Reason), "Scaling",
			"scaling from %d to %d replicas at queue depth %d", current, decision.Replicas, depth)
	}
	return decision.Replicas, nil
}

// workloadState is what the Deployment controller made of the spec.
type workloadState struct {
	replicas      int32
	readyReplicas int32
	exists        bool
}

// observeDeployment reads the Deployment back after applying it.

func (r *ModelDeploymentReconciler) observeDeployment(ctx context.Context, md *v1alpha1.ModelDeployment) (workloadState, error) {
	dep := &appsv1.Deployment{}
	err := r.Get(ctx, client.ObjectKey{Namespace: md.Namespace, Name: render.DeploymentName(md)}, dep)
	if apierrors.IsNotFound(err) {
		return workloadState{}, nil
	}
	if err != nil {
		return workloadState{}, fmt.Errorf("read deployment status: %w", err)
	}
	return workloadState{replicas: dep.Status.Replicas, readyReplicas: dep.Status.ReadyReplicas, exists: true}, nil
}

// reconcilePDB creates the budget only when it can be satisfied, and removes it
// when it no longer can. A PodDisruptionBudget of minAvailable=1 in front of a
// single replica does not protect availability, it blocks every node drain in
// the cluster until someone deletes it by hand.
func (r *ModelDeploymentReconciler) reconcilePDB(ctx context.Context, md *v1alpha1.ModelDeployment, replicas int32) error {
	minAvailable, wanted := render.PDBMinAvailable(md, replicas)
	if wanted {
		return r.applyOwned(ctx, md, render.PDB(md, minAvailable))
	}
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{
		Name:      render.PDBName(md),
		Namespace: md.Namespace,
	}}
	return client.IgnoreNotFound(r.Delete(ctx, pdb))
}

// applyOwned sends a server-side apply for an object the ModelDeployment owns.
//
// Server-side apply rather than get-modify-update: the API server then knows
// which fields belong to this operator, reverts drift in exactly those and
// leaves the rest — defaulted fields, another controller's annotations — alone.
// A request is sent on every reconcile, but an unchanged apply does not bump
// resourceVersion, so it causes no rollout, no generation change and no watch
// event. That property is what TestReconcileIsIdempotent asserts.
func (r *ModelDeploymentReconciler) applyOwned(ctx context.Context, md *v1alpha1.ModelDeployment, obj client.Object) error {
	if err := controllerutil.SetControllerReference(md, obj, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference on %T: %w", obj, err)
	}
	cfg, err := applyConfiguration(obj)
	if err != nil {
		return err
	}
	if err := r.Apply(ctx, cfg, client.FieldOwner(render.FieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply %T %s: %w", obj, obj.GetName(), err)
	}
	return nil
}

// applyConfiguration converts a rendered object into an apply configuration.
//
// The typed structs are kept as the source of truth because they are far easier
// to read and to unit-test than a tree of pointer builders. The conversion has
// to drop two things the Go types always serialize: the null creationTimestamp
// that metav1.ObjectMeta emits, and the empty status subobject. Left in, both
// would be claimed by this field manager, and the status ownership in particular
// would start a fight with the Deployment controller.
func applyConfiguration(obj client.Object) (runtime.ApplyConfiguration, error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("convert %T to an apply configuration: %w", obj, err)
	}
	u := &unstructured.Unstructured{Object: raw}
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "spec", "template", "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "status")
	return client.ApplyConfigurationFromUnstructured(u), nil
}

// finalize tears the deployment down in an order garbage collection cannot
// guarantee on its own.
//
// Owner references alone would delete the Deployment and the cache claim in
// whatever order the garbage collector picks. Detaching a ReadWriteMany volume
// from underneath running pods is how a namespace ends up stuck in Terminating,
// so the pods go first and the finalizer is only released once none are left.
func (r *ModelDeploymentReconciler) finalize(ctx context.Context, md *v1alpha1.ModelDeployment) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(md, render.Finalizer) {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: render.DeploymentName(md), Namespace: md.Namespace}}
	if err := client.IgnoreNotFound(r.Delete(ctx, dep, client.PropagationPolicy(metav1.DeletePropagationBackground))); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete deployment: %w", err)
	}
	if err := r.DeleteAllOf(ctx, &batchv1.Job{},
		client.InNamespace(md.Namespace),
		client.MatchingLabels{render.LabelInstance: md.Name},
		client.PropagationPolicy(metav1.DeletePropagationBackground),
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete warm jobs: %w", err)
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(md.Namespace),
		client.MatchingLabels{render.LabelInstance: md.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) > 0 {
		logger.Info("waiting for pods to terminate before releasing the weight cache", "pods", len(pods.Items))
		return ctrl.Result{RequeueAfter: terminationPollInterval}, nil
	}

	controllerutil.RemoveFinalizer(md, render.Finalizer)
	return ctrl.Result{}, r.Update(ctx, md)
}

func (r *ModelDeploymentReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func lastScaleTime(md *v1alpha1.ModelDeployment) time.Time {
	if md.Status.LastScaleTime == nil {
		return time.Time{}
	}
	return md.Status.LastScaleTime.Time
}

// writeStatus persists the status only when it actually differs. Writing an
// identical status would bump resourceVersion, wake every watcher and reconcile
// again — a loop that costs nothing visible until the cluster has a few hundred
// of these objects.
func (r *ModelDeploymentReconciler) writeStatus(ctx context.Context, md *v1alpha1.ModelDeployment, st *statusBuilder) error {
	next := st.build(md.Status)
	if equality.Semantic.DeepEqual(md.Status, next) {
		return nil
	}
	md.Status = next
	if err := r.Status().Update(ctx, md); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	return nil
}

// SetupWithManager wires the controller and its owned objects.
func (r *ModelDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ModelDeployment{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&batchv1.Job{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("modeldeployment").
		Complete(r)
}
