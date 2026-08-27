package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lpogosu/k8s-model-operator/api/v1alpha1"
	"github.com/lpogosu/k8s-model-operator/internal/render"
)

type fakeQueue struct {
	depth int32
	err   error
}

func (f *fakeQueue) Depth(context.Context, string, string, labels.Selector) (int32, error) {
	return f.depth, f.err
}

func ptr[T any](v T) *T { return &v }

// newCachedModelDeployment is the fixture most tests start from: a cached vLLM
// deployment of two replicas. Everything not set here is filled in by the CRD
// defaults, which TestCRDDefaults pins down.
func newCachedModelDeployment(t *testing.T, ns string) *v1alpha1.ModelDeployment {
	t.Helper()
	size := resource.MustParse("200Gi")
	md := &v1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: ns},
		Spec: v1alpha1.ModelDeploymentSpec{
			Model:    v1alpha1.ModelSpec{Name: "Qwen/Qwen2.5-7B-Instruct", Revision: "main"},
			Replicas: 2,
			WeightCache: &v1alpha1.WeightCacheSpec{
				Size:             &size,
				StorageClassName: ptr("nfs-rwx"),
			},
		},
	}
	if err := k8sClient.Create(context.Background(), md); err != nil {
		t.Fatalf("create modeldeployment: %v", err)
	}
	return md
}

func conditionOf(t *testing.T, md *v1alpha1.ModelDeployment, condType string) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(md.Status.Conditions, condType)
	if c == nil {
		t.Fatalf("condition %s is missing; have %+v", condType, md.Status.Conditions)
	}
	return c
}

func assertCondition(t *testing.T, md *v1alpha1.ModelDeployment, condType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := conditionOf(t, md, condType)
	if c.Status != status || c.Reason != reason {
		t.Errorf("condition %s = %s/%s, want %s/%s (message: %s)", condType, c.Status, c.Reason, status, reason, c.Message)
	}
	if c.ObservedGeneration != md.Generation {
		t.Errorf("condition %s observedGeneration = %d, want %d", condType, c.ObservedGeneration, md.Generation)
	}
}

func getDeployment(t *testing.T, ns, name string) *appsv1.Deployment {
	t.Helper()
	dep := &appsv1.Deployment{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, dep); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	return dep
}

func warmJobFor(t *testing.T, md *v1alpha1.ModelDeployment) *batchv1.Job {
	t.Helper()
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: md.Namespace, Name: render.WarmJobName(md, render.CacheKey(md))}
	if err := k8sClient.Get(context.Background(), key, job); err != nil {
		t.Fatalf("get warm job %s: %v", key.Name, err)
	}
	return job
}

// markWarmJobSucceeded is what the Job controller would do once the download
// finished. envtest runs no controllers, so the test plays that part.
func markWarmJobSucceeded(t *testing.T, job *batchv1.Job) {
	t.Helper()
	job.Status.Succeeded = 1
	if err := k8sClient.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("mark warm job succeeded: %v", err)
	}
}

func TestReconcileCreatesChildrenAndHoldsTheRollout(t *testing.T) {
	ns := newTestNamespace(t)
	md := newCachedModelDeployment(t, ns)
	r := newReconciler(t, nil, time.Time{})

	reconcileOnce(t, r, ns, md.Name) // adds the finalizer
	reconcileOnce(t, r, ns, md.Name) // creates the children

	md = getModelDeployment(t, ns, md.Name)
	cacheKey := render.CacheKey(md)

	pvc := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: md.Name + "-weights"}, pvc); err != nil {
		t.Fatalf("get weight cache claim: %v", err)
	}
	if got := pvc.Spec.AccessModes; len(got) != 1 || got[0] != corev1.ReadWriteMany {
		t.Errorf("cache claim access modes = %v, want [ReadWriteMany]", got)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "200Gi" {
		t.Errorf("cache claim size = %s, want 200Gi", got.String())
	}

	job := warmJobFor(t, md)
	if got := job.Labels[render.LabelCacheKey]; got != cacheKey {
		t.Errorf("warm job cache-key label = %q, want %q", got, cacheKey)
	}
	warmContainer := job.Spec.Template.Spec.Containers[0]
	if _, asked := warmContainer.Resources.Requests["nvidia.com/gpu"]; asked {
		t.Error("the warm-up job must not request a GPU: it would queue behind the pods it unblocks")
	}
	if got := job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; got != pvc.Name {
		t.Errorf("warm job mounts claim %q, want %q", got, pvc.Name)
	}

	dep := getDeployment(t, ns, md.Name)
	if !dep.Spec.Paused {
		t.Error("the deployment must stay paused while the weights are not cached")
	}
	if *dep.Spec.Replicas != 2 {
		t.Errorf("deployment replicas = %d, want 2", *dep.Spec.Replicas)
	}
	if got := dep.Spec.Template.Annotations[render.AnnCacheKey]; got != cacheKey {
		t.Errorf("pod template cache-key annotation = %q, want %q", got, cacheKey)
	}
	if len(dep.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("expected one init container gating on the cache, got %d", len(dep.Spec.Template.Spec.InitContainers))
	}
	if got := dep.Spec.Template.Spec.Containers[0].StartupProbe; got == nil {
		t.Error("a serving container without a startup probe gets killed by liveness while loading weights")
	}
	if got := dep.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue(); got != 0 {
		t.Errorf("rolling update maxUnavailable = %d, want 0", got)
	}
	if len(dep.OwnerReferences) != 1 || dep.OwnerReferences[0].Name != md.Name {
		t.Errorf("deployment owner references = %+v, want one pointing at %s", dep.OwnerReferences, md.Name)
	}

	svc := &corev1.Service{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: md.Name}, svc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Spec.Ports[0].Port != 8000 {
		t.Errorf("service port = %d, want 8000", svc.Spec.Ports[0].Port)
	}

	pdb := &policyv1.PodDisruptionBudget{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: md.Name}, pdb); err != nil {
		t.Fatalf("get pdb: %v", err)
	}
	if got := pdb.Spec.MinAvailable.IntValue(); got != 1 {
		t.Errorf("pdb minAvailable = %d, want 1", got)
	}

	assertCondition(t, md, v1alpha1.ConditionWeightsCached, metav1.ConditionFalse, v1alpha1.ReasonWarming)
	assertCondition(t, md, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonWaitingForWeights)
	assertCondition(t, md, v1alpha1.ConditionDegraded, metav1.ConditionFalse, v1alpha1.ReasonAsExpected)
	if md.Status.ObservedGeneration != md.Generation {
		t.Errorf("status observedGeneration = %d, want %d", md.Status.ObservedGeneration, md.Generation)
	}
	if md.Status.CacheKey != cacheKey {
		t.Errorf("status cacheKey = %q, want %q", md.Status.CacheKey, cacheKey)
	}
	if want := "http://" + md.Name + "." + ns + ".svc:8000"; md.Status.Endpoint != want {
		t.Errorf("status endpoint = %q, want %q", md.Status.Endpoint, want)
	}
}

func TestWarmCacheReleasesTheRolloutAndReportsReady(t *testing.T) {
	ns := newTestNamespace(t)
	md := newCachedModelDeployment(t, ns)
	r := newReconciler(t, nil, time.Time{})

	reconcileOnce(t, r, ns, md.Name)
	reconcileOnce(t, r, ns, md.Name)
	markWarmJobSucceeded(t, warmJobFor(t, getModelDeployment(t, ns, md.Name)))
	reconcileOnce(t, r, ns, md.Name)

	dep := getDeployment(t, ns, md.Name)
	if dep.Spec.Paused {
		t.Error("the deployment must be released once the weights are cached")
	}
	md = getModelDeployment(t, ns, md.Name)
	assertCondition(t, md, v1alpha1.ConditionWeightsCached, metav1.ConditionTrue, v1alpha1.ReasonCached)
	assertCondition(t, md, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonReplicasUnavailable)

	// Now play the Deployment controller: two pods came up and passed readiness.
	dep.Status.Replicas = 2
	dep.Status.ReadyReplicas = 2
	if err := k8sClient.Status().Update(context.Background(), dep); err != nil {
		t.Fatalf("update deployment status: %v", err)
	}
	reconcileOnce(t, r, ns, md.Name)

	md = getModelDeployment(t, ns, md.Name)
	assertCondition(t, md, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonReplicasReady)
	if md.Status.ReadyReplicas != 2 || md.Status.Replicas != 2 {
		t.Errorf("status replicas = %d/%d, want 2/2", md.Status.ReadyReplicas, md.Status.Replicas)
	}
	if md.Status.Selector == "" {
		t.Error("status.selector backs the scale subresource and must not be empty")
	}
}

// TestReconcileIsIdempotent is the property that separates a controller from a
// script: once converged, running it again must not touch anything. A write on
// every pass would restart the rollout, wake every watcher and, on a Deployment,
// churn pods forever.
func TestReconcileIsIdempotent(t *testing.T) {
	ns := newTestNamespace(t)
	md := newCachedModelDeployment(t, ns)
	r := newReconciler(t, nil, time.Time{})

	reconcileOnce(t, r, ns, md.Name)
	reconcileOnce(t, r, ns, md.Name)
	markWarmJobSucceeded(t, warmJobFor(t, getModelDeployment(t, ns, md.Name)))
	settle(t, r, ns, md.Name)

	type tracked struct {
		name string
		obj  client.Object
	}
	objects := []tracked{
		{"modeldeployment", &v1alpha1.ModelDeployment{}},
		{"deployment", &appsv1.Deployment{}},
		{"service", &corev1.Service{}},
		{"poddisruptionbudget", &policyv1.PodDisruptionBudget{}},
	}
	before := map[string]string{}
	for _, o := range objects {
		if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: md.Name}, o.obj); err != nil {
			t.Fatalf("get %s: %v", o.name, err)
		}
		before[o.name] = o.obj.GetResourceVersion()
	}

	// Two more passes: one would only prove the first was a no-op by accident.
	reconcileOnce(t, r, ns, md.Name)
	reconcileOnce(t, r, ns, md.Name)

	for _, o := range objects {
		if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: md.Name}, o.obj); err != nil {
			t.Fatalf("re-get %s: %v", o.name, err)
		}
		if got := o.obj.GetResourceVersion(); got != before[o.name] {
			t.Errorf("%s was written by a no-op reconcile: resourceVersion %s -> %s", o.name, before[o.name], got)
		}
	}
}

func TestModelChangeRotatesTheCacheAndHoldsTheOldPods(t *testing.T) {
	ns := newTestNamespace(t)
	md := newCachedModelDeployment(t, ns)
	r := newReconciler(t, nil, time.Time{})

	reconcileOnce(t, r, ns, md.Name)
	reconcileOnce(t, r, ns, md.Name)
	oldKey := render.CacheKey(getModelDeployment(t, ns, md.Name))
	oldJobName := render.WarmJobName(md, oldKey)
	markWarmJobSucceeded(t, warmJobFor(t, getModelDeployment(t, ns, md.Name)))
	settle(t, r, ns, md.Name)

	md = getModelDeployment(t, ns, md.Name)
	md.Spec.Model.Revision = "refs/pr/12"
	if err := k8sClient.Update(context.Background(), md); err != nil {
		t.Fatalf("update revision: %v", err)
	}
	reconcileOnce(t, r, ns, md.Name)

	md = getModelDeployment(t, ns, md.Name)
	newKey := render.CacheKey(md)
	if newKey == oldKey {
		t.Fatal("changing the revision must change the cache key")
	}
	if md.Status.CacheKey != newKey {
		t.Errorf("status cacheKey = %q, want %q", md.Status.CacheKey, newKey)
	}

	newJob := &batchv1.Job{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: render.WarmJobName(md, newKey)}, newJob); err != nil {
		t.Fatalf("the new revision must get its own warm job: %v", err)
	}
	oldJob := &batchv1.Job{}
	err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: oldJobName}, oldJob)
	if err == nil && oldJob.DeletionTimestamp == nil {
		t.Error("the warm job of the previous cache key should have been pruned")
	} else if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get old warm job: %v", err)
	}

	dep := getDeployment(t, ns, md.Name)
	if !dep.Spec.Paused {
		t.Error("the rollout must be held again until the new weights are cached")
	}
	if got := dep.Spec.Template.Annotations[render.AnnCacheKey]; got != newKey {
		t.Errorf("pod template annotation = %q, want the new key %q", got, newKey)
	}
	assertCondition(t, md, v1alpha1.ConditionWeightsCached, metav1.ConditionFalse, v1alpha1.ReasonWarming)
}

func TestWarmJobFailureIsReportedAsDegraded(t *testing.T) {
	ns := newTestNamespace(t)
	md := newCachedModelDeployment(t, ns)
	r := newReconciler(t, nil, time.Time{})

	reconcileOnce(t, r, ns, md.Name)
	reconcileOnce(t, r, ns, md.Name)

	job := warmJobFor(t, getModelDeployment(t, ns, md.Name))
	// The API server validates this transition: a Job may only carry Failed once
	// it has a start time and a FailureTarget condition. Reproducing the real
	// shape is the point of running against a real API server.
	now := metav1.Now()
	failure := func(t batchv1.JobConditionType) batchv1.JobCondition {
		return batchv1.JobCondition{
			Type:               t,
			Status:             corev1.ConditionTrue,
			Reason:             "BackoffLimitExceeded",
			Message:            "revision refs/pr/999 does not exist",
			LastProbeTime:      now,
			LastTransitionTime: now,
		}
	}
	job.Status.StartTime = &now
	job.Status.Failed = 4
	job.Status.Conditions = []batchv1.JobCondition{failure(batchv1.JobFailureTarget), failure(batchv1.JobFailed)}
	if err := k8sClient.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("fail the warm job: %v", err)
	}
	reconcileOnce(t, r, ns, md.Name)

	md = getModelDeployment(t, ns, md.Name)
	assertCondition(t, md, v1alpha1.ConditionWeightsCached, metav1.ConditionFalse, v1alpha1.ReasonWarmFailed)
	assertCondition(t, md, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonWarmFailed)
	if got := conditionOf(t, md, v1alpha1.ConditionDegraded).Message; got == "" {
		t.Error("the degraded message must carry the reason the download failed")
	}
	if !getDeployment(t, ns, md.Name).Spec.Paused {
		t.Error("a failed download must not release the rollout")
	}
}

func TestSingleReplicaGetsNoDisruptionBudget(t *testing.T) {
	ns := newTestNamespace(t)
	md := newCachedModelDeployment(t, ns)
	r := newReconciler(t, nil, time.Time{})

	reconcileOnce(t, r, ns, md.Name)
	reconcileOnce(t, r, ns, md.Name)
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: md.Name}, &policyv1.PodDisruptionBudget{}); err != nil {
		t.Fatalf("two replicas should have a budget: %v", err)
	}

	md = getModelDeployment(t, ns, md.Name)
	md.Spec.Replicas = 1
	if err := k8sClient.Update(context.Background(), md); err != nil {
		t.Fatalf("scale down: %v", err)
	}
	reconcileOnce(t, r, ns, md.Name)

	err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: md.Name}, &policyv1.PodDisruptionBudget{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("a budget in front of a single replica blocks every drain; want it removed, got %v", err)
	}
}

func TestDeletionWaitsForPodsBeforeReleasingTheCache(t *testing.T) {
	ns := newTestNamespace(t)
	md := newCachedModelDeployment(t, ns)
	r := newReconciler(t, nil, time.Time{})

	reconcileOnce(t, r, ns, md.Name)
	reconcileOnce(t, r, ns, md.Name)

	// Stand in for a serving pod: envtest runs no Deployment controller, so no
	// pod exists unless the test makes one.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "qwen-serving",
			Namespace: ns,
			Labels:    render.PodLabels(getModelDeployment(t, ns, md.Name)),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "vllm/vllm-openai:v0.11.0"}}},
	}
	if err := k8sClient.Create(context.Background(), pod); err != nil {
		t.Fatalf("create serving pod: %v", err)
	}

	if err := k8sClient.Delete(context.Background(), getModelDeployment(t, ns, md.Name)); err != nil {
		t.Fatalf("delete modeldeployment: %v", err)
	}

	res := reconcileOnce(t, r, ns, md.Name)
	if res.RequeueAfter == 0 {
		t.Error("teardown must requeue while pods are still running")
	}
	held := getModelDeployment(t, ns, md.Name)
	if len(held.Finalizers) == 0 {
		t.Fatal("the finalizer must be held until the pods are gone")
	}
	err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: md.Name}, &appsv1.Deployment{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("the deployment should be deleted first; got %v", err)
	}

	if err := k8sClient.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	reconcileOnce(t, r, ns, md.Name)

	err = k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: md.Name}, &v1alpha1.ModelDeployment{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("the finalizer should have been released; got %v", err)
	}
}

func TestAutoscalingScalesOnQueueDepth(t *testing.T) {
	ns := newTestNamespace(t)
	md := newCachedModelDeployment(t, ns)
	md.Spec.Replicas = 1
	md.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		MinReplicas:                   1,
		MaxReplicas:                   6,
		TargetQueueDepth:              4,
		MaxScaleUpStep:                2,
		MetricName:                    "vllm:num_requests_waiting",
		ScaleUpStabilizationSeconds:   60,
		ScaleDownStabilizationSeconds: 900,
	}
	if err := k8sClient.Update(context.Background(), md); err != nil {
		t.Fatalf("enable autoscaling: %v", err)
	}

	q := &fakeQueue{depth: 20}
	clock := time.Date(2026, 8, 12, 21, 0, 0, 0, time.UTC)
	r := newReconciler(t, q, clock)

	reconcileOnce(t, r, ns, md.Name)
	reconcileOnce(t, r, ns, md.Name)
	markWarmJobSucceeded(t, warmJobFor(t, getModelDeployment(t, ns, md.Name)))
	reconcileOnce(t, r, ns, md.Name)

	md = getModelDeployment(t, ns, md.Name)
	// The queue asks for five replicas; one step of two is what it gets.
	if md.Spec.Replicas != 3 {
		t.Errorf("replicas = %d, want 3 (one step of two from one)", md.Spec.Replicas)
	}
	if *getDeployment(t, ns, md.Name).Spec.Replicas != 3 {
		t.Error("the deployment must follow the decision written to the spec")
	}
	if md.Status.ObservedQueueDepth != 20 {
		t.Errorf("status observedQueueDepth = %d, want 20", md.Status.ObservedQueueDepth)
	}
	if md.Status.LastScaleTime == nil {
		t.Fatal("a scaling change must record lastScaleTime")
	}
	assertCondition(t, md, v1alpha1.ConditionDegraded, metav1.ConditionFalse, v1alpha1.ReasonAsExpected)

	// The same reconcile a second later must not scale again: the stabilization
	// window has not elapsed.
	r.Now = func() time.Time { return clock.Add(time.Second) }
	reconcileOnce(t, r, ns, md.Name)
	if got := getModelDeployment(t, ns, md.Name).Spec.Replicas; got != 3 {
		t.Errorf("replicas = %d during the stabilization window, want 3", got)
	}
}

func TestAutoscalingWithoutAMetricsAdapterIsDegraded(t *testing.T) {
	ns := newTestNamespace(t)
	md := newCachedModelDeployment(t, ns)
	md.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		MinReplicas: 1, MaxReplicas: 6, TargetQueueDepth: 4, MaxScaleUpStep: 2,
		MetricName: "vllm:num_requests_waiting",
	}
	if err := k8sClient.Update(context.Background(), md); err != nil {
		t.Fatalf("enable autoscaling: %v", err)
	}

	q := &fakeQueue{err: context.DeadlineExceeded}
	r := newReconciler(t, q, time.Time{})

	reconcileOnce(t, r, ns, md.Name)
	reconcileOnce(t, r, ns, md.Name)

	md = getModelDeployment(t, ns, md.Name)
	assertCondition(t, md, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonQueueMetricMissing)
	if md.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want the deployment left at its current size of 2", md.Spec.Replicas)
	}
	if !getDeployment(t, ns, md.Name).Spec.Paused {
		t.Error("an unreadable metric must not release the rollout either")
	}
}

func TestWithoutAWeightCacheThereIsNoJobAndNoPause(t *testing.T) {
	ns := newTestNamespace(t)
	md := &v1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "llama", Namespace: ns},
		Spec: v1alpha1.ModelDeploymentSpec{
			Model:    v1alpha1.ModelSpec{Name: "llama3.1:8b"},
			Runtime:  v1alpha1.RuntimeSpec{Engine: v1alpha1.EngineOllama},
			Replicas: 1,
		},
	}
	if err := k8sClient.Create(context.Background(), md); err != nil {
		t.Fatalf("create modeldeployment: %v", err)
	}
	r := newReconciler(t, nil, time.Time{})
	reconcileOnce(t, r, ns, md.Name)
	reconcileOnce(t, r, ns, md.Name)

	jobs := &batchv1.JobList{}
	if err := k8sClient.List(context.Background(), jobs, client.InNamespace(ns)); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("no shared cache means no warm-up job, got %d", len(jobs.Items))
	}
	err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: md.Name + "-weights"}, &corev1.PersistentVolumeClaim{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("no shared cache means no claim; got %v", err)
	}

	dep := getDeployment(t, ns, md.Name)
	if dep.Spec.Paused {
		t.Error("without a cache there is nothing to wait for, so the rollout must not be held")
	}
	if len(dep.Spec.Template.Spec.InitContainers) != 0 {
		t.Error("without a cache there is no marker to wait on")
	}
	if dep.Spec.Template.Spec.Volumes[0].EmptyDir == nil {
		t.Error("without a cache the weights volume must be an emptyDir")
	}

	md = getModelDeployment(t, ns, md.Name)
	assertCondition(t, md, v1alpha1.ConditionWeightsCached, metav1.ConditionFalse, v1alpha1.ReasonCacheDisabled)
}

// TestCRDDefaults pins the values every other test and the rendering code assume
// the API server fills in. They are part of the published API: changing one is a
// change users notice.
func TestCRDDefaults(t *testing.T) {
	ns := newTestNamespace(t)
	size := resource.MustParse("50Gi")
	md := &v1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "minimal", Namespace: ns},
		Spec: v1alpha1.ModelDeploymentSpec{
			Model:       v1alpha1.ModelSpec{Name: "Qwen/Qwen2.5-7B-Instruct"},
			WeightCache: &v1alpha1.WeightCacheSpec{Size: &size},
		},
	}
	if err := k8sClient.Create(context.Background(), md); err != nil {
		t.Fatalf("create minimal modeldeployment: %v", err)
	}
	md = getModelDeployment(t, ns, md.Name)

	for _, c := range []struct {
		field string
		got   any
		want  any
	}{
		{"model.revision", md.Spec.Model.Revision, "main"},
		{"runtime.engine", md.Spec.Runtime.Engine, v1alpha1.EngineVLLM},
		{"runtime.port", md.Spec.Runtime.Port, int32(8000)},
		{"runtime.loadTimeoutSeconds", md.Spec.Runtime.LoadTimeoutSeconds, int32(900)},
		{"replicas", md.Spec.Replicas, int32(1)},
		{"weightCache.mountPath", md.Spec.WeightCache.MountPath, "/models"},
		{"weightCache.warmTimeoutSeconds", md.Spec.WeightCache.WarmTimeoutSeconds, int32(3600)},
		{"disruption.minAvailable", md.Spec.Disruption.MinAvailable, int32(1)},
	} {
		if c.got != c.want {
			t.Errorf("default %s = %v, want %v", c.field, c.got, c.want)
		}
	}
}

// TestWeightCacheRequiresSizeOrClaim exercises the CEL rule on the CRD, which is
// the kind of validation that only exists once a real API server evaluates it.
func TestWeightCacheRequiresSizeOrClaim(t *testing.T) {
	ns := newTestNamespace(t)
	md := &v1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "no-size", Namespace: ns},
		Spec: v1alpha1.ModelDeploymentSpec{
			Model:       v1alpha1.ModelSpec{Name: "Qwen/Qwen2.5-7B-Instruct"},
			WeightCache: &v1alpha1.WeightCacheSpec{},
		},
	}
	err := k8sClient.Create(context.Background(), md)
	if err == nil {
		t.Fatal("a weight cache with neither a size nor an existing claim must be rejected")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("want an Invalid error from the CEL rule, got %v", err)
	}
}
