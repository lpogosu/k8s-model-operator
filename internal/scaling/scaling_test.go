package scaling

import (
	"testing"
	"time"
)

func basePolicy() Policy {
	return Policy{
		MinReplicas:      1,
		MaxReplicas:      8,
		TargetQueueDepth: 4,
		MaxScaleUpStep:   2,
		ScaleUpAfter:     time.Minute,
		ScaleDownAfter:   15 * time.Minute,
	}
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 8, 12, 21, 0, 0, 0, time.UTC)
	settled := now.Add(-time.Hour)
	justScaled := now.Add(-10 * time.Second)

	tests := []struct {
		name    string
		policy  func(Policy) Policy
		state   State
		want    int32
		changed bool
		reason  Reason
	}{
		{
			name:   "empty queue at the minimum stays put",
			state:  State{CurrentReplicas: 1, QueueDepth: 0, WeightsCached: true, LastScale: settled, Now: now},
			want:   1,
			reason: ReasonAtTarget,
		},
		{
			name:    "queue exactly at target for the current size does not scale",
			state:   State{CurrentReplicas: 2, QueueDepth: 8, WeightsCached: true, LastScale: settled, Now: now},
			want:    2,
			reason:  ReasonAtTarget,
			changed: false,
		},
		{
			name:    "one request over target adds a replica",
			state:   State{CurrentReplicas: 2, QueueDepth: 9, WeightsCached: true, LastScale: settled, Now: now},
			want:    3,
			changed: true,
			reason:  ReasonQueueAboveTarget,
		},
		{
			name:    "a spike is capped by the scale-up step",
			state:   State{CurrentReplicas: 1, QueueDepth: 400, WeightsCached: true, LastScale: settled, Now: now},
			want:    3,
			changed: true,
			reason:  ReasonQueueAboveTarget,
		},
		{
			name:   "scale-up waits out its stabilization window",
			state:  State{CurrentReplicas: 1, QueueDepth: 40, WeightsCached: true, LastScale: justScaled, Now: now},
			want:   1,
			reason: ReasonScaleUpBlocked,
		},
		{
			name:   "cold weights block scale-up however long the queue is",
			state:  State{CurrentReplicas: 2, QueueDepth: 100, WeightsCached: false, LastScale: settled, Now: now},
			want:   2,
			reason: ReasonWeightsNotCached,
		},
		{
			name:    "an empty queue gives up one replica, not all of them",
			state:   State{CurrentReplicas: 6, QueueDepth: 0, WeightsCached: true, LastScale: settled, Now: now},
			want:    5,
			changed: true,
			reason:  ReasonQueueBelowTarget,
		},
		{
			name:   "scale-down waits out its much longer window",
			state:  State{CurrentReplicas: 6, QueueDepth: 0, WeightsCached: true, LastScale: now.Add(-5 * time.Minute), Now: now},
			want:   6,
			reason: ReasonScaleDownBlocked,
		},
		{
			name:    "scaling down never crosses the minimum",
			state:   State{CurrentReplicas: 1, QueueDepth: 0, WeightsCached: true, LastScale: settled, Now: now},
			want:    1,
			reason:  ReasonAtTarget,
			changed: false,
		},
		{
			name:    "a lowered ceiling takes effect at once, without waiting",
			policy:  func(p Policy) Policy { p.MaxReplicas = 4; return p },
			state:   State{CurrentReplicas: 9, QueueDepth: 100, WeightsCached: true, LastScale: justScaled, Now: now},
			want:    4,
			changed: true,
			reason:  ReasonAboveMaxReplicas,
		},
		{
			name:    "a raised floor takes effect at once",
			policy:  func(p Policy) Policy { p.MinReplicas = 3; return p },
			state:   State{CurrentReplicas: 1, QueueDepth: 0, WeightsCached: true, LastScale: justScaled, Now: now},
			want:    3,
			changed: true,
			reason:  ReasonBelowMinReplicas,
		},
		{
			name:   "a raised floor still waits for the weights",
			policy: func(p Policy) Policy { p.MinReplicas = 3; return p },
			state:  State{CurrentReplicas: 1, QueueDepth: 0, WeightsCached: false, LastScale: settled, Now: now},
			want:   1,
			reason: ReasonWeightsNotCached,
		},
		{
			name:    "a deployment that never scaled is not held by any window",
			state:   State{CurrentReplicas: 1, QueueDepth: 12, WeightsCached: true, Now: now},
			want:    3,
			changed: true,
			reason:  ReasonQueueAboveTarget,
		},
		{
			name:    "scale-to-zero is reached one replica at a time",
			policy:  func(p Policy) Policy { p.MinReplicas = 0; return p },
			state:   State{CurrentReplicas: 1, QueueDepth: 0, WeightsCached: true, LastScale: settled, Now: now},
			want:    0,
			changed: true,
			reason:  ReasonQueueBelowTarget,
		},
		{
			name:    "traffic on a scaled-to-zero deployment brings back one replica",
			policy:  func(p Policy) Policy { p.MinReplicas = 0; return p },
			state:   State{CurrentReplicas: 0, QueueDepth: 1, WeightsCached: true, LastScale: settled, Now: now},
			want:    1,
			changed: true,
			reason:  ReasonQueueAboveTarget,
		},
		{
			name:   "the stabilization boundary is inclusive",
			state:  State{CurrentReplicas: 1, QueueDepth: 12, WeightsCached: true, LastScale: now.Add(-time.Minute), Now: now},
			want:   3,
			reason: ReasonQueueAboveTarget, changed: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := basePolicy()
			if tc.policy != nil {
				p = tc.policy(p)
			}
			got := Decide(p, tc.state)
			if got.Replicas != tc.want {
				t.Errorf("replicas = %d, want %d", got.Replicas, tc.want)
			}
			if got.Changed != tc.changed {
				t.Errorf("changed = %v, want %v", got.Changed, tc.changed)
			}
			if got.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.reason)
			}
		})
	}
}

// TestDecideConverges guards against the rule set oscillating: repeatedly
// applying the decision at a steady queue depth has to settle, and settle at the
// size the queue actually calls for.
func TestDecideConverges(t *testing.T) {
	p := basePolicy()
	state := State{CurrentReplicas: 1, QueueDepth: 18, WeightsCached: true, Now: time.Unix(0, 0)}

	for step := range 20 {
		d := Decide(p, state)
		if !d.Changed {
			if d.Replicas != 5 {
				t.Fatalf("settled at %d replicas after %d steps, want 5 for a queue of 18", d.Replicas, step)
			}
			return
		}
		state.CurrentReplicas = d.Replicas
		// Each step is far enough apart that no window blocks it.
		state.Now = state.Now.Add(time.Hour)
	}
	t.Fatal("the scaling rules did not converge in 20 steps")
}
