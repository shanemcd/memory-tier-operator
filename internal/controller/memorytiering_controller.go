// Package controller reconciles MemoryTiering custom resources into the
// tiering deployment: the DeviceClasses (the claim vocabulary), the driver
// ServiceAccount + RBAC, and the per-node driver DaemonSet.
package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	memtirv1 "github.com/shanemcd/memory-tier-operator/api/v1alpha1"
)

const (
	// DriverName is the DRA driver identity the driver daemon registers
	// with the kubelet. Device attributes are namespaced under this
	// domain.
	DriverName = "memorytier.openshift.io"

	// AttrDomain is the attribute domain for the driver's published
	// device attributes and capacities.
	AttrDomain = "memorytier.openshift.io"

	operatorName = "memory-tier-operator"
	driverName   = "memory-tier-driver"
)

// MemoryTieringReconciler reconciles a MemoryTiering object.
type MemoryTieringReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// OperatorImage is the image the operator itself runs as; the driver
	// DaemonSet defaults to it (the same image carries both entrypoints).
	OperatorImage string
}

// +kubebuilder:rbac:groups=memorytier.shanemcd.com,resources=memorytierings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=memorytier.shanemcd.com,resources=memorytierings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=resource.k8s.io,resources=deviceclasses,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update

func (r *MemoryTieringReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var mt memtirv1.MemoryTiering
	if err := r.Get(ctx, req.NamespacedName, &mt); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.ensureDeviceClasses(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure device classes: %w", err)
	}
	if err := r.ensureDriverRBAC(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure driver RBAC: %w", err)
	}
	if err := r.ensureDriverDaemonSet(ctx, &mt); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure driver daemonset: %w", err)
	}

	logger.Info("reconciled", "name", mt.Name)
	return ctrl.Result{}, nil
}

func (r *MemoryTieringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&memtirv1.MemoryTiering{}).
		// Reconcile when managed objects are deleted externally, so the
		// operator recreates them without waiting for a CR touch.
		Owns(&resourcev1.DeviceClass{}).
		Owns(&appsv1.DaemonSet{}).
		Complete(r)
}

// ensureDeviceClasses creates the tier vocabulary: dram-tier and cxl-tier
// DeviceClasses selecting on the driver's `type` attribute.
func (r *MemoryTieringReconciler) ensureDeviceClasses(ctx context.Context) error {
	for _, tier := range []struct {
		name string
		attr string
	}{
		{"dram-tier", "dram"},
		{"cxl-tier", "cxl"},
	} {
		desired := &resourcev1.DeviceClass{
			ObjectMeta: metav1.ObjectMeta{Name: tier.name},
			Spec: resourcev1.DeviceClassSpec{
				Selectors: []resourcev1.DeviceSelector{{
					CEL: &resourcev1.CELDeviceSelector{
						Expression: fmt.Sprintf(
							`device.attributes["%s"]["type"] == "%s"`, AttrDomain, tier.attr),
					},
				}},
			},
		}
		existing := &resourcev1.DeviceClass{}
		if err := r.Get(ctx, types.NamespacedName{Name: tier.name}, existing); err != nil {
			if !errors.IsNotFound(err) {
				return err
			}
			if err := r.Create(ctx, desired); err != nil && !errors.IsAlreadyExists(err) {
				return err
			}
			continue
		}
		if !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
			existing.Spec = desired.Spec
			if err := r.Update(ctx, existing); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureDriverRBAC creates the driver's ServiceAccount, Role, and
// RoleBinding in the operator's namespace.
func (r *MemoryTieringReconciler) ensureDriverRBAC(ctx context.Context) error {
	ns := r.driverNamespace()

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: driverName, Namespace: ns},
	}
	if err := r.ensureObject(ctx, sa, func() error {
		return r.Client.Create(ctx, sa)
	}); err != nil {
		return err
	}

	// ResourceSlices are cluster-scoped; the driver needs a ClusterRole
	// (a namespaced Role cannot list them).
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"resource.k8s.io"}, Resources: []string{"resourceslices"}, Verbs: []string{"create", "delete", "get", "list", "watch", "patch", "update"}},
			{APIGroups: []string{"resource.k8s.io"}, Resources: []string{"resourceclaims"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch"}},
		},
	}
	if err := r.ensureObject(ctx, role, func() error {
		return r.Create(ctx, role)
	}); err != nil {
		return err
	}

	rb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: driverName, Namespace: ns}},
		RoleRef:   rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: driverName},
	}
	return r.ensureObject(ctx, rb, func() error {
		return r.Create(ctx, rb)
	})
}

// ensureDriverDaemonSet renders the per-node driver daemon from the CR.
func (r *MemoryTieringReconciler) ensureDriverDaemonSet(ctx context.Context, mt *memtirv1.MemoryTiering) error {
	ns := r.driverNamespace()

	image := mt.Spec.DriverImage
	if image == "" {
		image = r.OperatorImage
	}

	nodeSelector := map[string]string{}
	for k, v := range mt.Spec.NodeSelector {
		nodeSelector[k] = v
	}

	labels := map[string]string{"app": driverName}
	desired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: driverName, Namespace: ns, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: driverName,
					NodeSelector:       nodeSelector,
					Containers: []corev1.Container{{
						Name:            "driver",
						Image:           image,
						ImagePullPolicy: corev1.PullAlways,
						Command:         []string{"/driver"},
						Args: []string{
							"-driver-name=" + DriverName,
						},
						Env: []corev1.EnvVar{{
							Name: "NODE_NAME",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
							},
						}},
						SecurityContext: &corev1.SecurityContext{
							Privileged: boolPtr(true),
							RunAsUser:  int64Ptr(0),
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "kubelet-plugins", MountPath: "/var/lib/kubelet/plugins"},
							{Name: "kubelet-plugins-registry", MountPath: "/var/lib/kubelet/plugins_registry"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "kubelet-plugins", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kubelet/plugins", Type: hostPathTypePtr(corev1.HostPathDirectoryOrCreate)}}},
						{Name: "kubelet-plugins-registry", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kubelet/plugins_registry", Type: hostPathTypePtr(corev1.HostPathDirectoryOrCreate)}}},
					},
				},
			},
		},
	}
	existing := &appsv1.DaemonSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: driverName, Namespace: ns}, existing); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		return r.Create(ctx, desired)
	}
	if !equality.Semantic.DeepEqual(existing.Spec.Template.Spec, desired.Spec.Template.Spec) {
		existing.Spec = desired.Spec
		return r.Update(ctx, existing)
	}
	return nil
}

func (r *MemoryTieringReconciler) driverNamespace() string {
	// The driver runs in the operator's namespace (single-namespace
	// deployment; the operator itself may run anywhere).
	return "kubevirt-mt"
}

func (r *MemoryTieringReconciler) ensureObject(ctx context.Context, obj client.Object, create func() error) error {
	key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	err := r.Client.Get(ctx, key, obj)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}
	if err := create(); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func boolPtr(b bool) *bool                    { return &b }
func int64Ptr(i int64) *int64                { return &i }
func hostPathTypePtr(t corev1.HostPathType) *corev1.HostPathType { return &t }
