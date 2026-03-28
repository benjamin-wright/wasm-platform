package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/configstore"
)

// ApplicationReconciler reconciles Application resources.
//
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=wasm-platform.io,resources=applications/finalizers,verbs=update
type ApplicationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Store  *configstore.Store
}

// Reconcile is the main reconciliation loop for Application resources.
func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	// TODO: implement reconciliation
	return ctrl.Result{}, nil
}

// SetupWithManager registers the ApplicationReconciler with the controller manager.
func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&wasmplatformv1alpha1.Application{}).
		Complete(r)
}
