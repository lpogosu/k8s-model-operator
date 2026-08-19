// Command manager runs the ModelDeployment controller.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/lpogosu/k8s-model-operator/api/v1alpha1"
	"github.com/lpogosu/k8s-model-operator/internal/controller"
	"github.com/lpogosu/k8s-model-operator/internal/queue"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "manager exited:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		metricsAddr    string
		probeAddr      string
		leaderElection bool
		metricsTimeout time.Duration
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address of the operator's own metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address of the health and readiness endpoints")
	flag.BoolVar(&leaderElection, "leader-elect", false, "run only one active manager across replicas")
	flag.DurationVar(&metricsTimeout, "queue-metrics-timeout", 10*time.Second,
		"timeout for a custom.metrics.k8s.io request; the generated client has no context support, so this is the only bound on it")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElection,
		LeaderElectionID:       "modeldeployment.serving.lpogosu.dev",
	})
	if err != nil {
		return fmt.Errorf("build manager: %w", err)
	}

	// The custom metrics client gets its own config: its only timeout is the one
	// on the REST client, and a stuck metrics adapter must not stall reconciles.
	metricsCfg := ctrl.GetConfigOrDie()
	metricsCfg.Timeout = metricsTimeout
	queueSource, err := queue.NewCustomMetrics(metricsCfg, mgr.GetRESTMapper())
	if err != nil {
		return fmt.Errorf("build queue metrics client: %w", err)
	}

	reconciler := &controller.ModelDeploymentReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("modeldeployment"),
		Queue:    queueSource,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register controller: %w", err)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("add health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("add ready check: %w", err)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("run manager: %w", err)
	}
	return nil
}
