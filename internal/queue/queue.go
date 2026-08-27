// Package queue reads the scaling signal: how many requests are waiting in the
// inference runtimes.
package queue

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/metrics/pkg/client/custom_metrics"
)

// Source reports the total queue depth of the pods matching a selector.
type Source interface {
	Depth(ctx context.Context, namespace, metricName string, sel labels.Selector) (int32, error)
}

var podGK = schema.GroupKind{Kind: "Pod"}

// CustomMetrics reads the depth from the custom.metrics.k8s.io API.
//
// That API does not exist in a cluster on its own. Something has to serve it —
// in practice prometheus-adapter, configured to expose the runtime's own gauge
// (vllm:num_requests_waiting) as a per-pod metric. This is the honest cost of
// scaling on a signal Kubernetes has no built-in notion of, and the operator
// reports it as a Degraded condition rather than silently not scaling.
type CustomMetrics struct {
	client custom_metrics.CustomMetricsClient
}

// NewCustomMetrics builds a client against the cluster's custom metrics API. It
// resolves the served API version through discovery on every call path, so an
// adapter installed after the operator started is picked up without a restart.
func NewCustomMetrics(cfg *rest.Config, mapper meta.RESTMapper) (*CustomMetrics, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	return &CustomMetrics{
		client: custom_metrics.NewForConfig(cfg, mapper, custom_metrics.NewAvailableAPIsGetter(dc)),
	}, nil
}

// Depth sums the metric over the selected pods.
//
// The context is accepted for the sake of the interface and is not honoured:
// the generated custom-metrics client has no context-aware methods, so the call
// is bounded by the timeout on the rest.Config instead.
func (c *CustomMetrics) Depth(_ context.Context, namespace, metricName string, sel labels.Selector) (int32, error) {
	values, err := c.client.NamespacedMetrics(namespace).
		GetForObjects(podGK, sel, metricName, labels.Everything())
	if err != nil {
		return 0, fmt.Errorf("read %q from custom.metrics.k8s.io: %w", metricName, err)
	}

	var total int64
	for i := range values.Items {
		// Queue depth is a whole number of requests; the adapter still returns it
		// as a Quantity, and a rounded-down milli value is the exact integer.
		total += values.Items[i].Value.MilliValue() / 1000
	}
	if total < 0 {
		return 0, fmt.Errorf("metric %q returned a negative total (%d)", metricName, total)
	}
	return int32(min(total, int64(1<<31-1))), nil
}
