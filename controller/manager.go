package controller

import (
	"context"
	"fmt"

	v1alpha1 "gitlab.prplanit.com/precisionplanit/hasteward/api/v1alpha1"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// Run starts the hasteward operator: controller-runtime manager + cron scheduler.
func Run(ctx context.Context, kubeconfig string) error {
	// Build scheme with core types + hasteward CRDs
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	// Init the k8s package (engines use global clients)
	c, err := k8s.Init(kubeconfig)
	if err != nil {
		return fmt.Errorf("kubernetes init failed: %w", err)
	}

	// Create controller-runtime manager
	mgr, err := ctrl.NewManager(c.RestConfig, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: ":8080",
		},
		HealthProbeBindAddress: ":8081",
	})
	if err != nil {
		return fmt.Errorf("unable to create manager: %w", err)
	}

	// Create scheduler
	sched := NewScheduler(mgr.GetClient())

	// Register database controllers (one per engine type)
	if err := SetupControllers(mgr, sched); err != nil {
		return fmt.Errorf("unable to setup controllers: %w", err)
	}

	// Health probes
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to setup health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to setup ready check: %w", err)
	}

	// Start cron scheduler
	sched.Start()
	defer sched.Stop()

	common.InfoLog("Starting hasteward operator")
	return mgr.Start(ctx)
}
