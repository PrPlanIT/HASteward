package controller

import (
	"context"

	v1alpha1 "gitlab.prplanit.com/precisionplanit/hasteward/api/v1alpha1"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/metrics"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// DatabaseReconciler watches database CRs for hasteward annotations
// and registers them with the scheduler.
type DatabaseReconciler struct {
	client    client.Client
	engine    string // "cnpg" or "galera"
	gvk       schema.GroupVersionKind
	scheduler *Scheduler
}

// SetupControllers registers a reconciler for each supported engine type.
func SetupControllers(mgr ctrl.Manager, sched *Scheduler) error {
	// CNPG Cluster controller
	cnpgObj := &unstructured.Unstructured{}
	cnpgObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster",
	})
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("cnpg").
		For(cnpgObj).
		Complete(&DatabaseReconciler{
			client:    mgr.GetClient(),
			engine:    "cnpg",
			gvk:       cnpgObj.GroupVersionKind(),
			scheduler: sched,
		}); err != nil {
		return err
	}

	// MariaDB controller
	mariaObj := &unstructured.Unstructured{}
	mariaObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.mariadb.com", Version: "v1alpha1", Kind: "MariaDB",
	})
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("galera").
		For(mariaObj).
		Complete(&DatabaseReconciler{
			client:    mgr.GetClient(),
			engine:    "galera",
			gvk:       mariaObj.GroupVersionKind(),
			scheduler: sched,
		}); err != nil {
		return err
	}

	return nil
}

func (r *DatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	dbKey := r.engine + "/" + req.Namespace + "/" + req.Name

	// Fetch the database CR
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.gvk)
	if err := r.client.Get(ctx, req.NamespacedName, obj); err != nil {
		if errors.IsNotFound(err) {
			r.scheduler.Deregister(dbKey)
			metrics.RecordReconcile(r.engine, "success")
			return ctrl.Result{}, nil
		}
		metrics.RecordReconcile(r.engine, "error")
		return ctrl.Result{}, err
	}

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	// Check for policy annotation (opt-in)
	policyName := annotations[v1alpha1.AnnotationPolicy]
	if policyName == "" {
		r.scheduler.Deregister(dbKey)
		return ctrl.Result{}, nil
	}

	// Check for exclude
	if annotations[v1alpha1.AnnotationExclude] == "true" {
		r.scheduler.Deregister(dbKey)
		return ctrl.Result{}, nil
	}

	// Fetch BackupPolicy
	policy := &v1alpha1.BackupPolicy{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: policyName}, policy); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("BackupPolicy not found, skipping", "policy", policyName, "database", dbKey)
		} else {
			common.ErrorLog("Failed to fetch BackupPolicy %q for %s: %v", policyName, dbKey, err)
		}
		return ctrl.Result{}, nil
	}

	// Resolve effective config
	effectiveCfg := v1alpha1.ParseAnnotations(annotations, &policy.Spec)

	// Register with scheduler
	r.scheduler.Register(dbKey, &ManagedDB{
		Namespace:   req.Namespace,
		ClusterName: req.Name,
		Engine:      r.engine,
		Config:      effectiveCfg,
	})

	// Set managed annotation if not already set
	if annotations[v1alpha1.AnnotationManaged] != "true" {
		patch := []byte(`{"metadata":{"annotations":{"` + v1alpha1.AnnotationManaged + `":"true"}}}`)
		if err := r.client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch)); err != nil {
			common.WarnLog("Failed to set managed annotation on %s: %v", dbKey, err)
		}
	}

	// Update status annotation on the database CR using unstructured + dynamic client
	r.updateTriageAnnotation(ctx, obj, annotations, dbKey)

	metrics.RecordReconcile(r.engine, "success")
	return ctrl.Result{}, nil
}

// updateTriageAnnotation is a placeholder for writing last-triage status back.
// Full implementation in triage_scheduler.go when triage results are available.
func (r *DatabaseReconciler) updateTriageAnnotation(ctx context.Context, obj *unstructured.Unstructured, annotations map[string]string, dbKey string) {
	// The GVR for patching unstructured objects via the dynamic client
	var gvr schema.GroupVersionResource
	switch r.engine {
	case "cnpg":
		gvr = k8s.CNPGClusterGVR
	case "galera":
		gvr = k8s.MariaDBGVR
	}
	_ = gvr // Used by scheduler when writing status annotations back
}
