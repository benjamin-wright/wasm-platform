package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	dboperator "github.com/benjamin-wright/db-operator/pkg/api/v1alpha1"
	platform "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/provisioning"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const inventoryFinalizer = "wasm-platform.io/inventory-protection"

// ResourceInventoryReconciler owns cleanup independently of Application existence or status.
type ResourceInventoryReconciler struct {
	client.Client
	APIReader client.Reader
}

func (r *ResourceInventoryReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func inventoryApplication(inventory *platform.ResourceInventory) *platform.Application {
	ref := inventory.Spec.ApplicationRef
	return &platform.Application{ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name, UID: types.UID(ref.UID)}}
}

func validateInventory(inventory *platform.ResourceInventory) error {
	ref := inventory.Spec.ApplicationRef
	if ref.UID == "" || ref.Name == "" || ref.Namespace != inventory.Namespace || inventory.Name != provisioning.InventoryName(types.UID(ref.UID)) || inventory.Labels["app.kubernetes.io/managed-by"] != "wp-operator" || len(inventory.OwnerReferences) != 0 {
		return fmt.Errorf("inventory %s/%s has an invalid ownership identity", inventory.Namespace, inventory.Name)
	}
	return nil
}

func inventoryHasObligations(inventory *platform.ResourceInventory) bool {
	for _, entry := range inventory.Spec.Resources {
		if !entry.Retained {
			return true
		}
	}
	return false
}

func releaseInventory(inventory *platform.ResourceInventory) {
	inventory.Spec.Releasing = true
	for i := range inventory.Spec.Resources {
		if !inventory.Spec.Resources[i].Retained {
			inventory.Spec.Resources[i].Disposition = platform.ResourceRelease
		}
	}
}

func persistInventoryEntry(inventory *platform.ResourceInventory, entry platform.ResourceBinding) error {
	if entry.Disposition == "" {
		entry.Disposition = platform.ResourceActive
	}
	for i, previous := range inventory.Spec.Resources {
		if inventoryKey(previous) != inventoryKey(entry) {
			continue
		}
		if previous.UID != "" && previous.UID != entry.UID {
			return fmt.Errorf("resource %s UID differs from inventory", entry.Name)
		}
		before, after := previous, entry
		before.UID, after.UID = "", ""
		before.Disposition, after.Disposition = "", ""
		if !reflect.DeepEqual(before, after) {
			return fmt.Errorf("resource %s binding differs from inventory", entry.Name)
		}
		if previous.Disposition == platform.ResourceRelease {
			entry.Disposition = platform.ResourceRelease
		}
		inventory.Spec.Resources[i] = entry
		return nil
	}
	if len(inventory.Spec.Resources) >= 128 {
		return fmt.Errorf("resource inventory exceeds 128 entries")
	}
	if inventory.Spec.Releasing && !entry.Retained {
		entry.Disposition = platform.ResourceRelease
	}
	inventory.Spec.Resources = append(inventory.Spec.Resources, entry)
	return nil
}

func (r *ResourceInventoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var inventory platform.ResourceInventory
	if err := r.reader().Get(ctx, req.NamespacedName, &inventory); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if err := validateInventory(&inventory); err != nil {
		return ctrl.Result{}, err
	}
	if !controllerutil.ContainsFinalizer(&inventory, inventoryFinalizer) {
		if !inventory.DeletionTimestamp.IsZero() {
			return ctrl.Result{}, fmt.Errorf("deleting inventory lacks cleanup protection")
		}
		controllerutil.AddFinalizer(&inventory, inventoryFinalizer)
		return ctrl.Result{Requeue: true}, r.Update(ctx, &inventory)
	}

	app := inventoryApplication(&inventory)
	var current platform.Application
	err := r.reader().Get(ctx, client.ObjectKeyFromObject(app), &current)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	absent := apierrors.IsNotFound(err) || current.UID != app.UID
	if absent && !inventory.Spec.Releasing {
		releaseInventory(&inventory)
		return ctrl.Result{Requeue: true}, r.Update(ctx, &inventory)
	}
	// The Application controller must withdraw serving and finish any provisioning
	// before handing over its obligations. A live owner prevents premature cleanup.
	if !inventory.DeletionTimestamp.IsZero() && !inventory.Spec.Releasing {
		return r.report(ctx, &inventory, "ApplicationActive", "Waiting for the Application to release its inventory.", nil)
	}

	resources := provisioning.NewWithReader(r.Client, r.reader()).WithInventory(&inventory)
	for _, entry := range inventory.Spec.Resources {
		if entry.Retained || entry.Disposition != platform.ResourceRelease {
			continue
		}
		done, uid, err := resources.RemoveApplicationResource(ctx, app, entry)
		if err != nil {
			return r.report(ctx, &inventory, "CleanupBlocked", "Resource ownership or cleanup could not be verified.", err)
		}
		if !done {
			if entry.UID == "" && uid != "" {
				entry.UID = uid
				if err := persistInventoryEntry(&inventory, entry); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{Requeue: true}, r.Update(ctx, &inventory)
			}
			continue
		}
		remaining := make([]platform.ResourceBinding, 0, len(inventory.Spec.Resources)-1)
		for _, previous := range inventory.Spec.Resources {
			if inventoryKey(previous) != inventoryKey(entry) {
				remaining = append(remaining, previous)
			}
		}
		inventory.Spec.Resources = remaining
		return ctrl.Result{Requeue: true}, r.Update(ctx, &inventory)
	}
	if absent && inventory.Spec.Releasing && !inventoryHasObligations(&inventory) {
		if inventory.DeletionTimestamp.IsZero() {
			uid, version := inventory.UID, inventory.ResourceVersion
			return ctrl.Result{Requeue: true}, r.Delete(ctx, &inventory, client.Preconditions{UID: &uid, ResourceVersion: &version})
		}
		controllerutil.RemoveFinalizer(&inventory, inventoryFinalizer)
		return ctrl.Result{}, r.Update(ctx, &inventory)
	}
	return r.report(ctx, &inventory, "Observed", "Inventory obligations observed.", nil)
}

func (r *ResourceInventoryReconciler) report(ctx context.Context, inventory *platform.ResourceInventory, reason, message string, cause error) (ctrl.Result, error) {
	before := inventory.Status.DeepCopy()
	inventory.Status.ObservedGeneration = inventory.Generation
	inventory.Status.PendingReleases = 0
	for _, entry := range inventory.Spec.Resources {
		if !entry.Retained && entry.Disposition == platform.ResourceRelease {
			inventory.Status.PendingReleases++
		}
	}
	status := metav1.ConditionTrue
	if cause != nil || inventory.Status.PendingReleases > 0 || reason == "ApplicationActive" {
		status = metav1.ConditionFalse
	}
	apimeta.SetStatusCondition(&inventory.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: reason, Message: message, ObservedGeneration: inventory.Generation})
	if !reflect.DeepEqual(*before, inventory.Status) {
		if err := r.Status().Update(ctx, inventory); err != nil {
			return ctrl.Result{}, err
		}
	}
	if cause != nil {
		return ctrl.Result{}, cause
	}
	// Periodic observation also covers an absent Application after a missed event.
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *ResourceInventoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platform.ResourceInventory{}).
		Owns(&dboperator.PostgresCredential{}).
		Owns(&dboperator.PostgresMigrationSet{}).
		Watches(&platform.Application{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: provisioning.InventoryName(obj.GetUID())}}}
		})).Complete(r)
}
