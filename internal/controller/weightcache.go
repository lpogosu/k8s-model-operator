package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/lpogosu/k8s-model-operator/api/v1alpha1"
	"github.com/lpogosu/k8s-model-operator/internal/render"
)

// cacheState is what the reconcile learned about the weight cache.
type cacheState struct {
	enabled bool
	warm    bool
	failed  bool
	jobName string
	message string
}

// reconcileWeightCache makes sure the claim exists and that a warm-up Job for the
// current cache key has been run, and reports whether the weights are on disk.
func (r *ModelDeploymentReconciler) reconcileWeightCache(
	ctx context.Context, md *v1alpha1.ModelDeployment, cacheKey string,
) (cacheState, error) {
	if md.Spec.WeightCache == nil {
		return cacheState{}, nil
	}
	if err := r.ensureClaim(ctx, md); err != nil {
		return cacheState{}, err
	}

	state := cacheState{enabled: true, jobName: render.WarmJobName(md, cacheKey)}
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: md.Namespace, Name: state.jobName}, job)
	switch {
	case apierrors.IsNotFound(err):
		desired := render.WarmJob(md, cacheKey)
		if err := controllerutil.SetControllerReference(md, desired, r.Scheme); err != nil {
			return state, fmt.Errorf("set owner reference on warm job: %w", err)
		}
		// Created, not applied: a Job's pod template is immutable, so there is
		// nothing an apply could ever converge. A changed spec produces a new
		// cache key and therefore a differently named Job.
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return state, fmt.Errorf("create warm job: %w", err)
		}
		r.Recorder.Eventf(md, nil, corev1.EventTypeNormal, v1alpha1.ReasonWarming, "WarmCache",
			"downloading weights for %s@%s into the shared cache", md.Spec.Model.Name, md.Spec.Model.Revision)
	case err != nil:
		return state, fmt.Errorf("read warm job: %w", err)
	default:
		state.warm = job.Status.Succeeded > 0
		if cond := findJobCondition(job, batchv1.JobFailed); cond != nil && cond.Status == corev1.ConditionTrue {
			state.failed = true
			state.message = fmt.Sprintf("warm-up job %s failed: %s", state.jobName, cond.Message)
		}
	}

	if err := r.pruneStaleWarmJobs(ctx, md, cacheKey); err != nil {
		return state, err
	}
	return state, nil
}

// ensureClaim creates the cache claim if it is missing.
//
// It is never updated. Almost every field of a PersistentVolumeClaim is
// immutable once bound, and the one that is not — the requested size — can only
// grow and only on a storage class that allows expansion. Silently attempting
// that on every reconcile would turn a typo in the spec into a failed apply loop,
// so resizing the cache is left as an explicit operation on the claim.
func (r *ModelDeploymentReconciler) ensureClaim(ctx context.Context, md *v1alpha1.ModelDeployment) error {
	name := render.PVCName(md)
	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, client.ObjectKey{Namespace: md.Namespace, Name: name}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read weight cache claim: %w", err)
	}
	if !render.OwnsPVC(md) {
		return fmt.Errorf("weight cache claim %q does not exist and is not managed by this ModelDeployment", name)
	}

	pvc := render.PVC(md)
	if err := controllerutil.SetControllerReference(md, pvc, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference on weight cache claim: %w", err)
	}
	if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create weight cache claim: %w", err)
	}
	return nil
}

// pruneStaleWarmJobs deletes the warm-up Jobs of previous cache keys.
//
// The weights they downloaded stay on the volume: the operator has no way to
// know whether another ModelDeployment sharing this claim is still serving them.
// Reclaiming that space is a deliberate gap, documented rather than guessed at.
func (r *ModelDeploymentReconciler) pruneStaleWarmJobs(ctx context.Context, md *v1alpha1.ModelDeployment, cacheKey string) error {
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs,
		client.InNamespace(md.Namespace),
		client.MatchingLabels{render.LabelInstance: md.Name, render.LabelRole: render.RoleWarm},
	); err != nil {
		return fmt.Errorf("list warm jobs: %w", err)
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Labels[render.LabelCacheKey] == cacheKey {
			continue
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, job,
			client.PropagationPolicy(metav1.DeletePropagationBackground),
		)); err != nil {
			return fmt.Errorf("delete stale warm job %s: %w", job.Name, err)
		}
	}
	return nil
}

func findJobCondition(job *batchv1.Job, t batchv1.JobConditionType) *batchv1.JobCondition {
	for i := range job.Status.Conditions {
		if job.Status.Conditions[i].Type == t {
			return &job.Status.Conditions[i]
		}
	}
	return nil
}
