// The memory-tier operator: reconciles MemoryTiering custom resources
// into DeviceClasses, driver RBAC, and the per-node driver DaemonSet.
package main

import (
	"flag"
	"os"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	memtirv1 "github.com/shanemcd/memory-tier-operator/api/v1alpha1"
	"github.com/shanemcd/memory-tier-operator/internal/controller"
	mtwebhook "github.com/shanemcd/memory-tier-operator/internal/webhook"
	kubevirtv1 "kubevirt.io/api/core/v1"
	crwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(resourcev1.AddToScheme(scheme))
	utilruntime.Must(memtirv1.AddToScheme(scheme))
	utilruntime.Must(kubevirtv1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var operatorImage string
	var webhookCertDir string
	var allowMemLockBacking bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint bind address")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe endpoint bind address")
	flag.StringVar(&operatorImage, "operator-image", os.Getenv("OPERATOR_IMAGE"), "image the operator runs as (the driver defaults to it)")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs", "directory containing tls.crt/tls.key for the VMI admission webhook")
	flag.BoolVar(&allowMemLockBacking, "allow-memlock-backing", false, "permit dram-only policies to render reservedOverhead.memLock (experimental; KubeVirt v1.10+ alpha)")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	metricsOpts := metricsserver.Options{BindAddress: metricsAddr}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOpts,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         true,
		LeaderElectionID:       "memory-tier-operator",
		WebhookServer: crwebhook.NewServer(crwebhook.Options{
			CertDir: webhookCertDir,
		}),
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

	// VMI admission governance: resolve the selected MemoryPolicy class
	// and render it into the incoming VMI before the apiserver persists
	// it. Fails closed on unknown/forbidden classes; the MutatingWebhook
	// Configuration selects governed namespaces and CREATE only.
	mgr.GetWebhookServer().Register("/mutate-kubevirt-io-v1-virtualmachineinstance",
		&admission.Webhook{Handler: &mtwebhook.VMIMutation{
			Client:              mgr.GetClient(),
			Decoder:             admission.NewDecoder(scheme),
			AllowMemLockBacking: allowMemLockBacking,
		}})

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "manager exited")
		os.Exit(1)
	}
}
