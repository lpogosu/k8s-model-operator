package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/lpogosu/k8s-model-operator/api/v1alpha1"
)

// TestSamplesApplyCleanly runs the files in config/samples through the real API
// server. Samples rot quietly: a field renamed in the API keeps working in every
// unit test and only breaks for the person who copy-pasted the README.
func TestSamplesApplyCleanly(t *testing.T) {
	dir := filepath.Join("..", "..", "config", "samples")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read samples: %v", err)
	}

	ns := newTestNamespace(t)
	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") || e.Name() == "kustomization.yaml" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			md := &v1alpha1.ModelDeployment{}
			// UnmarshalStrict rejects a field the API does not have, which is the
			// failure mode a plain apply would silently swallow.
			if err := yaml.UnmarshalStrict(raw, md); err != nil {
				t.Fatalf("decode %s: %v", e.Name(), err)
			}
			md.ObjectMeta = metav1.ObjectMeta{Name: md.Name, Namespace: ns}
			if err := k8sClient.Create(context.Background(), md); err != nil {
				t.Fatalf("the API server rejected %s: %v", e.Name(), err)
			}

			r := newReconciler(t, nil, time.Time{})
			reconcileOnce(t, r, ns, md.Name)
			reconcileOnce(t, r, ns, md.Name)
		})
		found++
	}
	if found == 0 {
		t.Fatal("no samples were checked; config/samples is empty or misnamed")
	}
}
