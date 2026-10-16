package controller

import (
	"testing"

	dboperator "github.com/benjamin-wright/db-operator/pkg/api/v1alpha1"
	platform "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func reconcileInventory(t *testing.T, c client.Client, key client.ObjectKey) {
	t.Helper()
	r := &ResourceInventoryReconciler{Client: c}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
}

func TestInventorySurvivesStatusLossAndCapabilityRemoval(t *testing.T) {
	app := lifecycleApp("status-loss")
	app.Spec.SQL = &platform.SQLSpec{}
	r, c := lifecycleReconciler(t, app)
	key := client.ObjectKeyFromObject(app)
	app = settleLifecycle(t, r, key)
	record := inventoryResource(t, app, "PostgresCredential", "app")
	inventory, err := r.readInventory(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	app.Spec.SQL = nil
	app.Spec.Functions = nil
	if err := c.Update(t.Context(), app); err != nil {
		t.Fatal(err)
	}
	app.Status = platform.ApplicationStatus{}
	if err := c.Status().Update(t.Context(), app); err != nil {
		t.Fatal(err)
	}
	inventory.Status = platform.ResourceInventoryStatus{}
	if err := c.Status().Update(t.Context(), inventory); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := reconcileLifecycle(t, r, key); err != nil {
			t.Fatal(err)
		}
	}
	var credential dboperator.PostgresCredential
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: record.Namespace, Name: record.Name}, &credential); !apierrors.IsNotFound(err) {
		t.Fatalf("credential survived status loss and capability removal: %v", err)
	}
	inventory, err = r.readInventory(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	if inventoryHasObligations(inventory) {
		t.Fatal("completed obligation remains in inventory spec")
	}
}

func TestOrphanInventoryFencesReplacementUntilIndependentCleanup(t *testing.T) {
	app := lifecycleApp("orphan-inventory")
	app.Spec.SQL = &platform.SQLSpec{}
	app.Spec.ResourceCleanupPolicy = platform.CleanupOrphanInventory
	r, c := lifecycleReconciler(t, app)
	key := client.ObjectKeyFromObject(app)
	app = settleLifecycle(t, r, key)
	record := inventoryResource(t, app, "PostgresCredential", "app")
	var credential dboperator.PostgresCredential
	childKey := client.ObjectKey{Namespace: record.Namespace, Name: record.Name}
	if err := c.Get(t.Context(), childKey, &credential); err != nil {
		t.Fatal(err)
	}
	credential.Finalizers = []string{"tests.wasm-platform.io/hold"}
	if err := c.Update(t.Context(), &credential); err != nil {
		t.Fatal(err)
	}
	inventory, err := r.readInventory(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	inventoryKey := client.ObjectKeyFromObject(inventory)
	if len(inventory.OwnerReferences) != 0 {
		t.Fatal("inventory must outlive Application garbage collection")
	}
	if owner := metav1.GetControllerOf(&credential); owner == nil || owner.UID != inventory.UID {
		t.Fatal("inventory does not own credential")
	}
	if err := c.Delete(t.Context(), app); err != nil {
		t.Fatal(err)
	}
	finishLifecycleDeletion(t, r, key)
	if err := c.Get(t.Context(), inventoryKey, inventory); err != nil {
		t.Fatal(err)
	}
	if !inventory.Spec.Releasing || !inventoryHasObligations(inventory) || len(inventory.Finalizers) == 0 {
		t.Fatal("orphan handoff lost cleanup obligations")
	}
	inventory.Status = platform.ResourceInventoryStatus{}
	if err := c.Status().Update(t.Context(), inventory); err != nil {
		t.Fatal(err)
	}
	replacement := lifecycleApp(app.Name)
	replacement.UID = "replacement-owner"
	replacement.Spec.SQL = &platform.SQLSpec{}
	if err := c.Create(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileLifecycle(t, r, key); err != nil {
		t.Fatal(err)
	}
	var credentials dboperator.PostgresCredentialList
	if err := c.List(t.Context(), &credentials); err != nil {
		t.Fatal(err)
	}
	if len(credentials.Items) != 1 || len(r.Store.Snapshot()) != 0 {
		t.Fatal("replacement provisioned or served before old obligations retired")
	}
	if err := c.Get(t.Context(), childKey, &credential); err != nil {
		t.Fatal(err)
	}
	credential.Finalizers = nil
	if err := c.Update(t.Context(), &credential); err != nil {
		t.Fatal(err)
	}
	// No Application reconcile is required to finish orphan cleanup.
	for range 5 {
		reconcileInventory(t, c, inventoryKey)
	}
	if err := c.Get(t.Context(), inventoryKey, inventory); !apierrors.IsNotFound(err) {
		t.Fatalf("completed orphan inventory still exists: %v", err)
	}
	settled := settleLifecycle(t, r, key)
	if inventoryResource(t, settled, "PostgresCredential", "app").UID == record.UID {
		t.Fatal("replacement reused old credential")
	}
}

func TestInventoryKeepsObligationAfterChildDisappears(t *testing.T) {
	app := lifecycleApp("remaining-secret")
	app.Spec.SQL = &platform.SQLSpec{}
	r, c := lifecycleReconciler(t, app)
	app = settleLifecycle(t, r, client.ObjectKeyFromObject(app))
	record := inventoryResource(t, app, "PostgresCredential", "app")
	inventory, err := r.readInventory(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(inventory)
	var credential dboperator.PostgresCredential
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: record.Namespace, Name: record.Name}, &credential); err != nil {
		t.Fatal(err)
	}
	// Bypass the test garbage collector to model an independently surviving Secret.
	if err := c.Client.Delete(t.Context(), &credential); err != nil {
		t.Fatal(err)
	}
	app.Finalizers = nil
	if err := c.Update(t.Context(), app); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), app); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		reconcileInventory(t, c, key)
	}
	if err := c.Get(t.Context(), key, inventory); err != nil {
		t.Fatal(err)
	}
	if !inventoryHasObligations(inventory) {
		t.Fatal("child absence incorrectly retired its Secret obligation")
	}
	inventory.Status = platform.ResourceInventoryStatus{}
	if err := c.Status().Update(t.Context(), inventory); err != nil {
		t.Fatal(err)
	}
	reconcileInventory(t, c, key)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: record.Namespace, Name: record.SecretName}}
	if err := c.Delete(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		reconcileInventory(t, c, key)
	}
	if err := c.Get(t.Context(), key, inventory); !apierrors.IsNotFound(err) {
		t.Fatalf("orphan did not finish after Secret cleanup: %v", err)
	}
}

func TestInventoryTransfersExistingApplicationChildWithoutRecreation(t *testing.T) {
	app := lifecycleApp("transfer-owner")
	app.Spec.SQL = &platform.SQLSpec{}
	r, c := lifecycleReconciler(t, app)
	app = settleLifecycle(t, r, client.ObjectKeyFromObject(app))
	record := inventoryResource(t, app, "PostgresCredential", "app")
	inventory, err := r.readInventory(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	var credential dboperator.PostgresCredential
	key := client.ObjectKey{Namespace: record.Namespace, Name: record.Name}
	if err := c.Get(t.Context(), key, &credential); err != nil {
		t.Fatal(err)
	}
	controller := true
	credential.OwnerReferences = []metav1.OwnerReference{{APIVersion: platform.GroupVersion.String(), Kind: "Application", Name: app.Name, UID: app.UID, Controller: &controller}}
	if err := c.Update(t.Context(), &credential); err != nil {
		t.Fatal(err)
	}
	// Recover the old status journal and Application-owned child into a new inventory.
	inventory.Finalizers = nil
	if err := c.Update(t.Context(), inventory); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), inventory); err != nil {
		t.Fatal(err)
	}
	app = settleLifecycle(t, r, client.ObjectKeyFromObject(app))
	inventory, err = r.readInventory(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Get(t.Context(), key, &credential); err != nil {
		t.Fatal(err)
	}
	if string(credential.UID) != record.UID || metav1.GetControllerOf(&credential).UID != inventory.UID {
		t.Fatal("transfer replaced the child or retained the old owner")
	}
}

func TestInventoryChildChangesWakeApplication(t *testing.T) {
	app := lifecycleApp("inventory-events")
	app.Spec.SQL = &platform.SQLSpec{}
	r, c := lifecycleReconciler(t, app)
	app = settleLifecycle(t, r, client.ObjectKeyFromObject(app))
	entry := inventoryResource(t, app, "PostgresCredential", "app")
	var credential dboperator.PostgresCredential
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: entry.Namespace, Name: entry.Name}, &credential); err != nil {
		t.Fatal(err)
	}
	requests := r.applicationsForInventoryResource(t.Context(), &credential)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(app) {
		t.Fatal("inventory-owned child change did not enqueue Application")
	}
	credential.OwnerReferences[0].UID = "replaced-inventory"
	if len(r.applicationsForInventoryResource(t.Context(), &credential)) != 0 {
		t.Fatal("foreign inventory owner enqueued Application")
	}
}
