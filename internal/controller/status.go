package controller

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/lpogosu/k8s-model-operator/api/v1alpha1"
	"github.com/lpogosu/k8s-model-operator/internal/render"
)

// statusBuilder collects what the reconcile observed and turns it into a status
// in one place. Scattering condition writes through the reconcile is how
// conditions end up contradicting each other.
type statusBuilder struct {
	md         *v1alpha1.ModelDeployment
	cacheKey   string
	cache      cacheState
	workload   workloadState
	paused     bool
	queueDepth int32
	lastScale  *metav1.Time
	scaleErr   error
}

func newStatusBuilder(md *v1alpha1.ModelDeployment, cacheKey string) *statusBuilder {
	return &statusBuilder{md: md, cacheKey: cacheKey, queueDepth: md.Status.ObservedQueueDepth}
}

func (b *statusBuilder) applyCache(c cacheState)               { b.cache = c }
func (b *statusBuilder) applyWorkload(w workloadState, p bool) { b.workload, b.paused = w, p }

// build returns the status to persist, derived from the previous one so that the
// LastTransitionTime of every condition survives an unchanged reconcile.
func (b *statusBuilder) build(prev v1alpha1.ModelDeploymentStatus) v1alpha1.ModelDeploymentStatus {
	next := *prev.DeepCopy()
	next.ObservedGeneration = b.md.Generation
	next.CacheKey = b.cacheKey
	next.Replicas = b.workload.replicas
	next.ReadyReplicas = b.workload.readyReplicas
	next.ObservedQueueDepth = b.queueDepth
	next.Endpoint = render.Endpoint(b.md)
	next.Selector = labels.Set(render.SelectorLabels(b.md)).String()
	if b.lastScale != nil {
		next.LastScaleTime = b.lastScale
	}

	for _, c := range []metav1.Condition{b.weightsCached(), b.ready(), b.degraded()} {
		c.ObservedGeneration = b.md.Generation
		meta.SetStatusCondition(&next.Conditions, c)
	}
	return next
}

func (b *statusBuilder) weightsCached() metav1.Condition {
	c := metav1.Condition{Type: v1alpha1.ConditionWeightsCached, Status: metav1.ConditionFalse}
	switch {
	case !b.cache.enabled:
		c.Reason = v1alpha1.ReasonCacheDisabled
		c.Message = "no shared weight cache is configured: every replica downloads its own copy"
	case b.cache.failed:
		c.Reason = v1alpha1.ReasonWarmFailed
		c.Message = b.cache.message
	case b.cache.warm:
		c.Status = metav1.ConditionTrue
		c.Reason = v1alpha1.ReasonCached
		c.Message = fmt.Sprintf("weights for cache key %s are on the shared volume", b.cacheKey)
	default:
		c.Reason = v1alpha1.ReasonWarming
		c.Message = fmt.Sprintf("job %s is downloading the weights", b.cache.jobName)
	}
	return c
}

func (b *statusBuilder) ready() metav1.Condition {
	c := metav1.Condition{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse}
	desired := b.md.Spec.Replicas
	switch {
	case desired == 0:
		c.Reason = v1alpha1.ReasonScaledToZero
		c.Message = "no replicas requested"
	case b.paused:
		c.Reason = v1alpha1.ReasonWaitingForWeights
		c.Message = "the rollout is held until the weights for the current spec are cached"
	case b.workload.exists && b.workload.readyReplicas >= desired:
		c.Status = metav1.ConditionTrue
		c.Reason = v1alpha1.ReasonReplicasReady
		c.Message = fmt.Sprintf("%d/%d replicas are serving", b.workload.readyReplicas, desired)
	default:
		c.Reason = v1alpha1.ReasonReplicasUnavailable
		c.Message = fmt.Sprintf("%d/%d replicas are serving", b.workload.readyReplicas, desired)
	}
	return c
}

func (b *statusBuilder) degraded() metav1.Condition {
	c := metav1.Condition{Type: v1alpha1.ConditionDegraded, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonAsExpected}
	switch {
	case b.cache.failed:
		c.Status = metav1.ConditionTrue
		c.Reason = v1alpha1.ReasonWarmFailed
		c.Message = b.cache.message
	case b.scaleErr != nil:
		c.Status = metav1.ConditionTrue
		c.Reason = v1alpha1.ReasonQueueMetricMissing
		c.Message = b.scaleErr.Error()
	default:
		c.Message = "serving the requested model with the requested policy"
	}
	return c
}
