// The memory-tier operator: reconciles MemoryTiering custom resources
// into DeviceClasses, driver RBAC, and the per-node driver DaemonSet.
package main

import (
	"net/http"
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	resourcev1 "k8s.io/api/resource/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	memtirv1 "github.com/shanemcd/memory-tier-operator/api/v1alpha1"
	"github.com/shanemcd/memory-tier-operator/internal/controller"
	"github.com/shanemcd/memory-tier-operator/internal/webhook"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(resourcev1.AddToScheme(scheme))
	utilruntime.Must(memtirv1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var operatorImage string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint bind address")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe endpoint bind address")
	flag.StringVar(&operatorImage, "operator-image", os.Getenv("OPERATOR_IMAGE"), "image the operator runs as (the driver defaults to it)")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	metricsOpts := metricsserver.Options{BindAddress: metricsAddr}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOpts,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         true,
		LeaderElectionID:       "memory-tier-operator",
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.MemoryTieringReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		OperatorImage: operatorImage,
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Register the VMI mutating webhook handler. The webhook is optional:
	// without TLS certs (no MutatingWebhookConfiguration deployed) the
	// server would fail manager start, so only register when the cert
	// exists. The tier system works without it — VMs reference claims
	// directly via spec.resourceClaims.
	if _, err := os.Stat("/tmp/k8s-webhook-server/serving-certs/tls.crt"); err == nil {
		mgr.GetWebhookServer().Register("/mutate-vmi", http.HandlerFunc(webhook.HandleMutate))
		ctrl.Log.Info("VMI mutating webhook registered", "path", "/mutate-vmi")
	} else {
		ctrl.Log.Info("webhook certs not found — VMI mutating webhook disabled (VMs use spec.resourceClaims directly)")
	}

	ctrl.Log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "manager exited")
		os.Exit(1)
	}
}
