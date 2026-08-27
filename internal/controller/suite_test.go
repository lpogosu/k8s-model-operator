package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/lpogosu/k8s-model-operator/api/v1alpha1"
)

var (
	testEnv    *envtest.Environment
	restConfig *rest.Config
	k8sClient  client.Client
	testScheme = runtime.NewScheme()
)

// TestMain brings up a real API server and etcd once for the whole package.
// These tests talk to an actual apiserver on purpose: defaulting, CEL
// validation, the status subresource, owner references and server-side apply
// are exactly the parts a fake client does not model, and they are exactly the
// parts this controller depends on.
func TestMain(m *testing.M) {
	if err := clientgoscheme.AddToScheme(testScheme); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(testScheme); err != nil {
		panic(err)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: envtestAssets(),
	}

	var err error
	restConfig, err = testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest failed to start: %v\n", err)
		os.Exit(1)
	}

	k8sClient, err = client.New(restConfig, client.Options{Scheme: testScheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "build client: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	code := m.Run()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "envtest failed to stop: %v\n", err)
	}
	os.Exit(code)
}

// envtestAssets locates the apiserver and etcd binaries. KUBEBUILDER_ASSETS, set
// by `make test`, wins; the fallback lets `go test ./...` work straight from an
// IDE after the binaries have been downloaded once.
func envtestAssets() string {
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir
	}
	root := filepath.Join("..", "..", "bin", "envtest", "k8s")
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	suffix := fmt.Sprintf("-%s-%s", goruntime.GOOS, goruntime.GOARCH)
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			return filepath.Join(root, e.Name())
		}
	}
	return ""
}

// newTestNamespace gives every test its own namespace. envtest has no namespace
// controller, so deleting one would only mark it Terminating; a fresh name per
// test is the only real isolation available.
func newTestNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "md-test-",
	}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return ns.Name
}

// newReconciler builds a reconciler wired to the test API server. The tests
// drive Reconcile directly rather than starting a manager: a background loop
// would make "reconciling twice changes nothing" impossible to assert.
func newReconciler(t *testing.T, q *fakeQueue, now time.Time) *ModelDeploymentReconciler {
	t.Helper()
	r := &ModelDeploymentReconciler{
		Client:   k8sClient,
		Scheme:   testScheme,
		Recorder: events.NewFakeRecorder(64),
	}
	if q != nil {
		r.Queue = q
	}
	if !now.IsZero() {
		r.Now = func() time.Time { return now }
	}
	return r
}

func reconcileOnce(t *testing.T, r *ModelDeploymentReconciler, ns, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: ns, Name: name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

// settle runs the reconcile until it stops changing the ModelDeployment itself.
// The first pass only adds the finalizer, the second creates the children, and a
// spec write by the autoscaler costs one more. Bounding the loop keeps a
// non-converging reconcile from looking like a passing test.
func settle(t *testing.T, r *ModelDeploymentReconciler, ns, name string) {
	t.Helper()
	const maxPasses = 5
	for i := 0; i < maxPasses; i++ {
		before := getModelDeployment(t, ns, name)
		reconcileOnce(t, r, ns, name)
		after := getModelDeployment(t, ns, name)
		if before.ResourceVersion == after.ResourceVersion && i > 0 {
			return
		}
	}
	t.Fatalf("reconcile did not converge in %d passes", maxPasses)
}

func getModelDeployment(t *testing.T, ns, name string) *v1alpha1.ModelDeployment {
	t.Helper()
	md := &v1alpha1.ModelDeployment{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, md); err != nil {
		t.Fatalf("get modeldeployment %s/%s: %v", ns, name, err)
	}
	return md
}
