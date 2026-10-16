package controller

import (
	"context"
	"fmt"
	"reflect"

	dboperator "github.com/benjamin-wright/db-operator/pkg/api/v1alpha1"
	platform "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/provisioning"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (r *ApplicationReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ApplicationReconciler) resources() *provisioning.Resources {
	return provisioning.NewWithReader(r.Client, r.reader())
}

func inventoryKey(entry platform.ResourceBinding) string {
	return entry.Kind + "/" + entry.Namespace + "/" + entry.Name
}

func (r *ApplicationReconciler) readInventory(ctx context.Context, app *platform.Application) (*platform.ResourceInventory, error) {
	var inventory platform.ResourceInventory
	key := client.ObjectKey{Namespace: app.Namespace, Name: provisioning.InventoryName(app.UID)}
	if err := r.reader().Get(ctx, key, &inventory); err != nil {
		return nil, err
	}
	if err := validateInventory(&inventory); err != nil {
		return nil, err
	}
	if inventory.Spec.ApplicationRef.Name != app.Name || inventory.Spec.ApplicationRef.UID != string(app.UID) {
		return nil, fmt.Errorf("resource inventory belongs to another Application")
	}
	return &inventory, nil
}

func (r *ApplicationReconciler) applicationResources(ctx context.Context, app *platform.Application) (*provisioning.Resources, error) {
	inventory, err := r.readInventory(ctx, app)
	if err != nil {
		return nil, err
	}
	if inventory.Spec.Releasing || !inventory.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("resource inventory is being released")
	}
	return r.resources().WithInventory(inventory), nil
}

func (r *ApplicationReconciler) persistResource(ctx context.Context, app *platform.Application, entry platform.ResourceBinding) error {
	inventory, err := r.readInventory(ctx, app)
	if err != nil {
		return err
	}
	if inventory.Spec.Releasing || !inventory.DeletionTimestamp.IsZero() {
		return fmt.Errorf("resource inventory is being released")
	}
	before := inventory.Spec.DeepCopy()
	if err := persistInventoryEntry(inventory, entry); err != nil {
		return err
	}
	if !reflect.DeepEqual(*before, inventory.Spec) {
		if err := r.Update(ctx, inventory); err != nil {
			return err
		}
	}
	app.Status.ResourceInventory = append([]platform.ResourceBinding(nil), inventory.Spec.Resources...)
	return nil
}

func (r *ApplicationReconciler) recordResourceUID(ctx context.Context, app *platform.Application, entry platform.ResourceBinding, obj client.Object) error {
	if obj.GetUID() == "" {
		return fmt.Errorf("resource %s has no persisted UID", entry.Name)
	}
	entry.UID = string(obj.GetUID())
	return r.persistResource(ctx, app, entry)
}

func (r *ApplicationReconciler) applicationResource(ctx context.Context, app *platform.Application, kind, logicalName string) (platform.ResourceBinding, error) {
	inventory, err := r.readInventory(ctx, app)
	if err != nil {
		return platform.ResourceBinding{}, err
	}
	for _, entry := range inventory.Spec.Resources {
		if entry.Kind == kind && entry.LogicalName == logicalName && !entry.Retained {
			if entry.Disposition == platform.ResourceRelease {
				return entry, fmt.Errorf("resource %s is being released", entry.Name)
			}
			return entry, nil
		}
	}
	entry := platform.ResourceBinding{
		Kind: kind, Namespace: app.Namespace, LogicalName: logicalName, Disposition: platform.ResourceActive,
		Name:        provisioning.ApplicationResourceName(app, kind, logicalName),
		DatabaseRef: postgresDatabaseCRName(app.Namespace), Database: PGDatabaseName(app.Namespace, app.Name),
	}
	if kind == "PostgresCredential" {
		entry.Username = PGUsername(app.Namespace, app.Name, logicalName)
		entry.SecretName = entry.Name + "-creds"
	}
	return entry, r.persistResource(ctx, app, entry)
}

func (r *ApplicationReconciler) sharedResourceIntent(ctx context.Context, app *platform.Application, kind, namespace, name string) (platform.ResourceBinding, error) {
	entry := platform.ResourceBinding{Kind: kind, Namespace: namespace, Name: name, LogicalName: "infrastructure", Retained: true, Disposition: platform.ResourceActive}
	inventory, err := r.readInventory(ctx, app)
	if err != nil {
		return entry, err
	}
	for _, previous := range inventory.Spec.Resources {
		if inventoryKey(entry) == inventoryKey(previous) {
			return previous, nil
		}
	}
	return entry, r.persistResource(ctx, app, entry)
}

func (r *ApplicationReconciler) ensureInventory(ctx context.Context, app *platform.Application) (*platform.ResourceInventory, error) {
	inventory, err := r.readInventory(ctx, app)
	if err == nil {
		return inventory, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	inventory = &platform.ResourceInventory{
		ObjectMeta: metav1.ObjectMeta{Namespace: app.Namespace, Name: provisioning.InventoryName(app.UID),
			Labels:     map[string]string{"app.kubernetes.io/managed-by": "wp-operator", provisioning.ApplicationUIDLabel: string(app.UID)},
			Finalizers: []string{inventoryFinalizer}},
		Spec: platform.ResourceInventorySpec{ApplicationRef: platform.InventoryApplicationReference{Namespace: app.Namespace, Name: app.Name, UID: string(app.UID)}},
	}
	// Import the previous status journal before changing any child ownership.
	for _, entry := range app.Status.ResourceInventory {
		if err := persistInventoryEntry(inventory, entry); err != nil {
			return nil, err
		}
	}
	if r.Client.Scheme().Recognizes(dboperator.GroupVersion.WithKind("PostgresCredential")) {
		discovered, err := r.resources().DiscoverApplicationResources(ctx, app)
		if err != nil {
			return nil, err
		}
		for _, entry := range discovered {
			if err := persistInventoryEntry(inventory, entry); err != nil {
				return nil, err
			}
		}
	}
	if len(inventory.Spec.Resources) == 0 && (!app.DeletionTimestamp.IsZero() || (app.Spec.SQL == nil && !app.Spec.KV)) {
		return nil, nil
	}
	if app.UID == "" {
		return nil, fmt.Errorf("Application has no persisted identity")
	}
	if err := r.Create(ctx, inventory); err != nil {
		return nil, err
	}
	if inventory.UID == "" {
		return nil, fmt.Errorf("ResourceInventory has no persisted identity")
	}
	return inventory, nil
}

// reconcileResourceInventory records desired obligations; the inventory controller releases them.
func (r *ApplicationReconciler) reconcileResourceInventory(ctx context.Context, app *platform.Application, deleting bool) (bool, error) {
	if !deleting && app.Spec.SQL != nil {
		var inventories platform.ResourceInventoryList
		if err := r.reader().List(ctx, &inventories, client.InNamespace(app.Namespace)); err != nil {
			return false, err
		}
		for _, previous := range inventories.Items {
			if previous.Spec.ApplicationRef.Name == app.Name && previous.Spec.ApplicationRef.UID != string(app.UID) && inventoryHasObligations(&previous) {
				return true, nil
			}
		}
	}
	inventory, err := r.ensureInventory(ctx, app)
	if err != nil {
		return false, err
	}
	if inventory == nil {
		app.Status.ResourceInventory = nil
		return false, nil
	}
	if !inventory.DeletionTimestamp.IsZero() && !deleting {
		return false, fmt.Errorf("resource inventory is being deleted")
	}
	resources := r.resources().WithInventory(inventory)
	before := inventory.Spec.DeepCopy()
	discovered, err := resources.DiscoverApplicationResources(ctx, app)
	if err != nil {
		return false, err
	}
	for _, entry := range discovered {
		if err := persistInventoryEntry(inventory, entry); err != nil {
			return false, err
		}
	}
	wanted := map[string]bool{}
	if !deleting && app.Spec.SQL != nil {
		usernames := map[string]string{}
		for _, user := range sqlUsersForApp(app.Spec.SQL) {
			username := PGUsername(app.Namespace, app.Name, user)
			if previous, exists := usernames[username]; exists {
				return false, fmt.Errorf("SQL users %s and %s resolve to the same role", previous, user)
			}
			usernames[username] = user
			wanted["PostgresCredential/"+user] = true
		}
		if app.Spec.SQL.Migrations != nil {
			wanted["PostgresMigrationSet/migrations"] = true
		}
	}
	for i, entry := range inventory.Spec.Resources {
		if !entry.Retained && !wanted[entry.Kind+"/"+entry.LogicalName] {
			inventory.Spec.Resources[i].Disposition = platform.ResourceRelease
		}
	}
	if deleting {
		releaseInventory(inventory)
	}
	if !reflect.DeepEqual(*before, inventory.Spec) {
		if err := r.Update(ctx, inventory); err != nil {
			return false, err
		}
	}
	// Complete verified transfers before an orphan handoff can remove the Application.
	for _, entry := range inventory.Spec.Resources {
		if entry.Retained || entry.Disposition != platform.ResourceRelease {
			continue
		}
		if err := resources.ClaimApplicationResource(ctx, app, entry); err != nil {
			return false, err
		}
	}
	app.Status.ResourceInventory = append([]platform.ResourceBinding(nil), inventory.Spec.Resources...)
	pending := false
	for _, entry := range inventory.Spec.Resources {
		pending = pending || (!entry.Retained && entry.Disposition == platform.ResourceRelease)
	}
	if deleting {
		if err := r.Status().Update(ctx, app); err != nil {
			return false, err
		}
		if app.Spec.ResourceCleanupPolicy == platform.CleanupOrphanInventory {
			return false, nil
		}
	}
	return pending, nil
}

// Child status and credential rotation must reach the serving controller through the inventory owner.
func (r *ApplicationReconciler) applicationsForInventoryResource(ctx context.Context, obj client.Object) []reconcile.Request {
	owner := metav1.GetControllerOf(obj)
	if owner == nil || owner.APIVersion != platform.GroupVersion.String() || owner.Kind != "ResourceInventory" {
		return nil
	}
	var inventory platform.ResourceInventory
	if err := r.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: owner.Name}, &inventory); err != nil || inventory.UID != owner.UID {
		return nil
	}
	if err := validateInventory(&inventory); err != nil {
		return nil
	}
	ref := inventory.Spec.ApplicationRef
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}}}
}
