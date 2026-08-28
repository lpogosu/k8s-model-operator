// Package scaling decides how many replicas an inference deployment should run.
//
// It is deliberately a pure function of a policy and an observation. Autoscaling
// bugs are the kind that only show up as a bill at the end of the month, so the
// rules have to be readable on their own and testable without a cluster, a
// clock, or a metrics server.
package scaling

import "time"

// Reason explains a decision. It ends up verbatim in an event and in the
// Degraded condition message, so the wording is part of the contract.
type Reason string

// The reasons a decision can carry.
const (
	ReasonAtTarget          Reason = "QueueAtTarget"
	ReasonQueueAboveTarget  Reason = "QueueAboveTarget"
	ReasonQueueBelowTarget  Reason = "QueueBelowTarget"
	ReasonScaleUpBlocked    Reason = "ScaleUpStabilizing"
	ReasonScaleDownBlocked  Reason = "ScaleDownStabilizing"
	ReasonWeightsNotCached  Reason = "WeightsNotCached"
	ReasonBelowMinReplicas  Reason = "BelowMinReplicas"
	ReasonAboveMaxReplicas  Reason = "AboveMaxReplicas"
	ReasonAutoscalingPaused Reason = "AutoscalingDisabled"
)

// Policy is the user's scaling configuration, already resolved.
type Policy struct {
	MinReplicas      int32
	MaxReplicas      int32
	TargetQueueDepth int32
	MaxScaleUpStep   int32
	ScaleUpAfter     time.Duration
	ScaleDownAfter   time.Duration
}

// State is what the controller observed this reconcile.
type State struct {
	// CurrentReplicas is the replica count on the Deployment right now.
	CurrentReplicas int32
	// QueueDepth is the number of requests accepted by the runtimes and not yet
	// generating, summed over all replicas.
	QueueDepth int32
	// WeightsCached reports whether the shared cache holds the weights for the
	// spec as it stands. While it is false, a new replica would be a replica of
	// the model version that is about to be replaced.
	WeightsCached bool
	// LastScale is when the replica count last changed. The zero value means it
	// never has, and both stabilization windows are then considered elapsed.
	LastScale time.Time
	// Now is the current time, injected so the windows are testable.
	Now time.Time
}

// Decision is the outcome. Replicas is always a legal value even when Changed is
// false, so the caller can write it unconditionally.
type Decision struct {
	Replicas int32
	Changed  bool
	Reason   Reason
}

// Decide applies the policy.
//
// The target itself is the same arithmetic the HorizontalPodAutoscaler uses —
// desired = ceil(queue / target) — but three rules around it differ, and each
// exists because a replica here costs a GPU and several minutes rather than a
// container start:
//
//   - Scaling down moves one replica at a time. A queue that empties reads as a
//     metric of zero, and the HPA formula would answer "go to the minimum" in a
//     single step. On a stateless web service that is fine. Here it throws away
//     capacity that costs minutes to rebuild.
//   - Scaling up is capped by MaxScaleUpStep, because a metric spike should not
//     be able to claim every GPU in the pool before anyone can look at it.
//   - Scaling up is refused entirely while the weights are not cached.
//
// Bound violations are fixed immediately and ignore both windows: if the user
// just lowered MaxReplicas, waiting fifteen minutes to obey is not stabilization,
// it is disobedience.
func Decide(p Policy, s State) Decision {
	if s.CurrentReplicas > p.MaxReplicas {
		return Decision{Replicas: p.MaxReplicas, Changed: true, Reason: ReasonAboveMaxReplicas}
	}
	if s.CurrentReplicas < p.MinReplicas {
		if !s.WeightsCached {
			return Decision{Replicas: s.CurrentReplicas, Reason: ReasonWeightsNotCached}
		}
		return Decision{Replicas: p.MinReplicas, Changed: true, Reason: ReasonBelowMinReplicas}
	}

	target := clamp(ceilDiv(s.QueueDepth, p.TargetQueueDepth), p.MinReplicas, p.MaxReplicas)

	switch {
	case target > s.CurrentReplicas:
		if !s.WeightsCached {
			return Decision{Replicas: s.CurrentReplicas, Reason: ReasonWeightsNotCached}
		}
		if !elapsed(s, p.ScaleUpAfter) {
			return Decision{Replicas: s.CurrentReplicas, Reason: ReasonScaleUpBlocked}
		}
		step := min(target-s.CurrentReplicas, p.MaxScaleUpStep)
		return Decision{Replicas: s.CurrentReplicas + step, Changed: true, Reason: ReasonQueueAboveTarget}

	case target < s.CurrentReplicas:
		if !elapsed(s, p.ScaleDownAfter) {
			return Decision{Replicas: s.CurrentReplicas, Reason: ReasonScaleDownBlocked}
		}
		return Decision{Replicas: s.CurrentReplicas - 1, Changed: true, Reason: ReasonQueueBelowTarget}

	default:
		return Decision{Replicas: s.CurrentReplicas, Reason: ReasonAtTarget}
	}
}

func elapsed(s State, window time.Duration) bool {
	if s.LastScale.IsZero() {
		return true
	}
	return !s.Now.Before(s.LastScale.Add(window))
}

func ceilDiv(a, b int32) int32 {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

func clamp(v, lo, hi int32) int32 {
	return min(max(v, lo), hi)
}
