package provisioning

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	dboperator "github.com/benjamin-wright/db-operator/pkg/api/v1alpha1"
	platform "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ApplicationUIDLabel            = "wasm-platform.io/application-uid"
	LogicalNameAnnotation          = "wasm-platform.io/logical-name"
	LegacyApplicationUIDAnnotation = "wasm-platform.io/legacy-application-uid"
)

// ApplicationResourceName encodes the complete ephemeral owner identity within a DNS label.
func ApplicationResourceName(app *platform.Application, kind, logicalName string) string {
	tuple, _ := json.Marshal([]string{kind, app.Namespace, app.Name, string(app.UID), logicalName})
	digest := sha256.Sum256(tuple)
	prefix := "pg"
	if kind == "PostgresMigrationSet" {
		prefix = "migrations"
	}
	return fmt.Sprintf("wasm-%s-%x", prefix, digest[:16])
}

func InventoryName(uid types.UID) string {
	digest := sha256.Sum256([]byte(uid))
	return fmt.Sprintf("wasm-inventory-%x", digest[:16])
}

func (r *Resources) WithInventory(inventory *platform.ResourceInventory) *Resources {
	bound := *r
	bound.inventory = inventory
	return &bound
}

func (r *Resources) validateInventory(app *platform.Application) error {
	if r.inventory == nil || r.inventory.UID == "" || r.inventory.Namespace != app.Namespace || r.inventory.Spec.ApplicationRef.Name != app.Name || r.inventory.Spec.ApplicationRef.UID != string(app.UID) {
		return fmt.Errorf("application resource requires its persisted ResourceInventory")
	}
	return nil
}

func (r *Resources) resourceMetadata(app *platform.Application, entry platform.ResourceBinding) metav1.ObjectMeta {
	metadata := applicationMetadata(app, entry)
	metadata.OwnerReferences[0].Kind = "ResourceInventory"
	metadata.OwnerReferences[0].Name = r.inventory.Name
	metadata.OwnerReferences[0].UID = r.inventory.UID
	return metadata
}

func (r *Resources) verifyResource(app *platform.Application, entry platform.ResourceBinding, obj client.Object) error {
	if r.inventory == nil {
		return verifyApplicationResource(app, entry, obj)
	}
	if err := r.validateInventory(app); err != nil {
		return err
	}
	owner := metav1.GetControllerOf(obj)
	if owner == nil || owner.Kind != "ResourceInventory" || owner.Name != r.inventory.Name || owner.UID != r.inventory.UID {
		return verifyApplicationResource(app, entry, obj)
	}
	// Reuse the exact binding and metadata checks with the verified owner translated.
	copy := obj.DeepCopyObject().(client.Object)
	refs := copy.GetOwnerReferences()
	refs[0].Kind, refs[0].Name, refs[0].UID = "Application", app.Name, app.UID
	copy.SetOwnerReferences(refs)
	return verifyApplicationResource(app, entry, copy)
}

func (r *Resources) ClaimApplicationResource(ctx context.Context, app *platform.Application, entry platform.ResourceBinding) error {
	if err := r.validateInventory(app); err != nil {
		return err
	}
	var obj client.Object
	switch entry.Kind {
	case "PostgresCredential":
		obj = &dboperator.PostgresCredential{}
	case "PostgresMigrationSet":
		obj = &dboperator.PostgresMigrationSet{}
	default:
		return fmt.Errorf("unsupported application resource kind %s", entry.Kind)
	}
	err := r.getObserved(ctx, client.ObjectKey{Namespace: entry.Namespace, Name: entry.Name}, obj)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.adoptApplicationResource(ctx, app, entry, obj)
}

func applicationMetadata(app *platform.Application, entry platform.ResourceBinding) metav1.ObjectMeta {
	controller, block := true, true
	return metav1.ObjectMeta{
		Namespace: entry.Namespace, Name: entry.Name,
		Labels:          map[string]string{"app.kubernetes.io/managed-by": "wp-operator", ApplicationUIDLabel: string(app.UID)},
		Annotations:     map[string]string{LogicalNameAnnotation: entry.LogicalName},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: platform.GroupVersion.String(), Kind: "Application", Name: app.Name, UID: app.UID, Controller: &controller, BlockOwnerDeletion: &block}},
	}
}

func verifyApplicationResource(app *platform.Application, entry platform.ResourceBinding, obj client.Object) error {
	if err := verifyApplicationData(app, entry); err != nil {
		return err
	}
	if entry.Namespace != app.Namespace || obj.GetNamespace() != entry.Namespace || obj.GetName() != entry.Name || app.UID == "" {
		return fmt.Errorf("invalid application resource binding")
	}
	if entry.UID != "" && string(obj.GetUID()) != entry.UID {
		return fmt.Errorf("resource %s/%s UID differs from inventory", entry.Namespace, entry.Name)
	}
	owner := metav1.GetControllerOf(obj)
	owned := owner != nil && owner.APIVersion == platform.GroupVersion.String() && owner.Kind == "Application" && owner.Name == app.Name && owner.UID == app.UID
	if !owned || len(obj.GetOwnerReferences()) != 1 || obj.GetLabels()[ApplicationUIDLabel] != string(app.UID) || obj.GetLabels()["app.kubernetes.io/managed-by"] != "wp-operator" || obj.GetAnnotations()[LogicalNameAnnotation] != entry.LogicalName {
		return fmt.Errorf("resource %s/%s is not owned by this Application", entry.Namespace, entry.Name)
	}
	return verifyBinding(entry, obj)
}

func verifyBinding(entry platform.ResourceBinding, obj client.Object) error {
	valid := false
	switch resource := obj.(type) {
	case *dboperator.PostgresCredential:
		valid = resource.Spec.DatabaseRef == entry.DatabaseRef && resource.Spec.Username == entry.Username && resource.Spec.SecretName == entry.SecretName && len(resource.Spec.ClusterRoles) == 0 && len(resource.Spec.Permissions) > 0
		for _, permission := range resource.Spec.Permissions {
			valid = valid && len(permission.Databases) == 1 && permission.Databases[0] == entry.Database
		}
	case *dboperator.PostgresMigrationSet:
		valid = resource.Spec.DatabaseRef == entry.DatabaseRef && resource.Spec.Database == entry.Database
	}
	if !valid {
		return fmt.Errorf("resource %s/%s does not match its inventoried binding", entry.Namespace, entry.Name)
	}
	return nil
}

func verifyApplicationData(app *platform.Application, entry platform.ResourceBinding) error {
	if app.UID == "" || entry.Namespace != app.Namespace || entry.LogicalName == "" || (entry.Kind != "PostgresCredential" && entry.Kind != "PostgresMigrationSet") {
		return fmt.Errorf("invalid application resource binding")
	}
	if entry.DatabaseRef != "wasm-"+app.Namespace+"-postgres" || entry.Database != PGDatabaseName(app.Namespace, app.Name) {
		return fmt.Errorf("resource binding targets another Application's database")
	}
	if entry.Kind == "PostgresCredential" && entry.Username != PGUsername(app.Namespace, app.Name, entry.LogicalName) {
		return fmt.Errorf("resource binding targets another Application's SQL user")
	}
	return nil
}

// DiscoverApplicationResources recovers obligations after a crash or a spec edit.
// Unowned legacy credentials require an administrator's explicit UID attestation.
func (r *Resources) DiscoverApplicationResources(ctx context.Context, app *platform.Application) ([]platform.ResourceBinding, error) {
	var credentials dboperator.PostgresCredentialList
	if err := r.apiReader.List(ctx, &credentials, client.InNamespace(app.Namespace)); err != nil {
		return nil, err
	}
	var migrations dboperator.PostgresMigrationSetList
	if err := r.apiReader.List(ctx, &migrations, client.InNamespace(app.Namespace)); err != nil {
		return nil, err
	}
	var result []platform.ResourceBinding
	appendResource := func(kind string, obj client.Object, entry platform.ResourceBinding) error {
		owner := metav1.GetControllerOf(obj)
		claimed := obj.GetLabels()[ApplicationUIDLabel] == string(app.UID) || (owner != nil && owner.UID == app.UID)
		attested := obj.GetAnnotations()[LegacyApplicationUIDAnnotation] == string(app.UID)
		legacyMigration := kind == "PostgresMigrationSet" && obj.GetName() == MigrationSetName(app.Namespace, app.Name) && obj.GetLabels()["wasm-platform.io/app-namespace"] == app.Namespace && obj.GetLabels()["wasm-platform.io/app-name"] == app.Name
		if !claimed && !attested && !legacyMigration {
			return nil
		}
		entry.Kind, entry.Namespace, entry.Name, entry.UID = kind, obj.GetNamespace(), obj.GetName(), string(obj.GetUID())
		if err := verifyApplicationData(app, entry); err != nil {
			return err
		}
		if claimed {
			entry.LogicalName = obj.GetAnnotations()[LogicalNameAnnotation]
			if err := r.verifyResource(app, entry, obj); err != nil {
				return err
			}
		} else {
			if len(obj.GetOwnerReferences()) != 0 {
				return fmt.Errorf("legacy resource %s has a foreign owner", obj.GetName())
			}
			if err := verifyBinding(entry, obj); err != nil {
				return err
			}
		}
		result = append(result, entry)
		if len(result) > 128 {
			return fmt.Errorf("application resource inventory exceeds 128 entries")
		}
		return nil
	}
	for i := range credentials.Items {
		cred := &credentials.Items[i]
		logical := cred.Annotations[LogicalNameAnnotation]
		if logical == "" && app.Spec.SQL != nil {
			users := []platform.SQLUserSpec{{Name: "app"}}
			if len(app.Spec.SQL.Users) > 0 {
				users = app.Spec.SQL.Users
			}
			for _, user := range users {
				if PGUsername(app.Namespace, app.Name, user.Name) == cred.Spec.Username {
					logical = user.Name
					break
				}
			}
		}
		entry := platform.ResourceBinding{LogicalName: logical, DatabaseRef: cred.Spec.DatabaseRef, Username: cred.Spec.Username, SecretName: cred.Spec.SecretName}
		entry.Database = app.Status.SQLDatabaseName
		if len(cred.Spec.Permissions) > 0 && len(cred.Spec.Permissions[0].Databases) == 1 {
			entry.Database = cred.Spec.Permissions[0].Databases[0]
		}
		targetsApplicationData := slices.ContainsFunc(cred.Spec.Permissions, func(permission dboperator.DatabasePermissionEntry) bool {
			return slices.Contains(permission.Databases, PGDatabaseName(app.Namespace, app.Name))
		})
		if strings.HasPrefix(cred.Name, "wasm-") && cred.Spec.DatabaseRef == "wasm-"+app.Namespace+"-postgres" && targetsApplicationData && cred.Labels[ApplicationUIDLabel] == "" && cred.Annotations[LegacyApplicationUIDAnnotation] != string(app.UID) {
			return nil, fmt.Errorf("legacy credential %s requires Application UID attestation", cred.Name)
		}
		if err := appendResource("PostgresCredential", cred, entry); err != nil {
			return nil, err
		}
	}
	for i := range migrations.Items {
		ms := &migrations.Items[i]
		entry := platform.ResourceBinding{LogicalName: "migrations", DatabaseRef: ms.Spec.DatabaseRef, Database: ms.Spec.Database}
		if err := appendResource("PostgresMigrationSet", ms, entry); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (r *Resources) adoptApplicationResource(ctx context.Context, app *platform.Application, entry platform.ResourceBinding, obj client.Object) error {
	if err := verifyApplicationData(app, entry); err != nil {
		return err
	}
	if err := r.validateInventory(app); err != nil {
		return err
	}
	if obj.GetLabels()[ApplicationUIDLabel] != "" || len(obj.GetOwnerReferences()) != 0 {
		if err := r.verifyResource(app, entry, obj); err != nil {
			return err
		}
		if owner := metav1.GetControllerOf(obj); owner != nil && owner.Kind == "ResourceInventory" {
			return nil
		}
	} else {
		attested := obj.GetAnnotations()[LegacyApplicationUIDAnnotation] == string(app.UID)
		legacyMigration := entry.Kind == "PostgresMigrationSet" && obj.GetName() == MigrationSetName(app.Namespace, app.Name) && obj.GetLabels()["wasm-platform.io/app-namespace"] == app.Namespace && obj.GetLabels()["wasm-platform.io/app-name"] == app.Name
		if !attested && !legacyMigration {
			return fmt.Errorf("legacy resource %s requires Application UID attestation", entry.Name)
		}
		if err := verifyBinding(entry, obj); err != nil {
			return err
		}
	}
	if entry.UID == "" || entry.UID != string(obj.GetUID()) {
		return fmt.Errorf("resource %s lacks a verified inventory UID", entry.Name)
	}
	before := obj.DeepCopyObject().(client.Object)
	metadata := r.resourceMetadata(app, entry)
	labels, annotations := obj.GetLabels(), obj.GetAnnotations()
	if labels == nil {
		labels = map[string]string{}
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	for k, v := range metadata.Labels {
		labels[k] = v
	}
	for k, v := range metadata.Annotations {
		annotations[k] = v
	}
	obj.SetLabels(labels)
	obj.SetAnnotations(annotations)
	obj.SetOwnerReferences(metadata.OwnerReferences)
	return r.client.Patch(ctx, obj, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *Resources) EnsureCredential(ctx context.Context, app *platform.Application, entry platform.ResourceBinding, spec dboperator.PostgresCredentialSpec) (*dboperator.PostgresCredential, error) {
	if err := r.validateInventory(app); err != nil {
		return nil, err
	}
	if r.inventory.Spec.Releasing || !r.inventory.DeletionTimestamp.IsZero() || entry.Disposition == platform.ResourceRelease {
		return nil, fmt.Errorf("inventory resource is being released")
	}
	if err := verifyApplicationData(app, entry); err != nil {
		return nil, err
	}
	if entry.Kind != "PostgresCredential" {
		return nil, fmt.Errorf("invalid credential resource kind")
	}
	if err := verifyBinding(entry, &dboperator.PostgresCredential{Spec: spec}); err != nil {
		return nil, err
	}
	var existing dboperator.PostgresCredential
	err := r.getObserved(ctx, types.NamespacedName{Namespace: entry.Namespace, Name: entry.Name}, &existing)
	if apierrors.IsNotFound(err) {
		if entry.UID != "" {
			return nil, fmt.Errorf("inventoried credential %s disappeared", entry.Name)
		}
		if entry.Name != ApplicationResourceName(app, entry.Kind, entry.LogicalName) || entry.SecretName != entry.Name+"-creds" {
			return nil, fmt.Errorf("invalid credential creation identity")
		}
		var secret corev1.Secret
		secretErr := r.getObserved(ctx, types.NamespacedName{Namespace: entry.Namespace, Name: entry.SecretName}, &secret)
		if secretErr == nil {
			return nil, fmt.Errorf("credential Secret %s exists without its expected credential", entry.SecretName)
		}
		if !apierrors.IsNotFound(secretErr) {
			return nil, secretErr
		}
		existing = dboperator.PostgresCredential{ObjectMeta: r.resourceMetadata(app, entry), Spec: spec}
		if err := r.client.Create(ctx, &existing); err != nil {
			return nil, err
		}
		return &existing, nil
	}
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(existing.Spec, spec) {
		return nil, fmt.Errorf("credential %s policy differs from desired configuration", entry.Name)
	}
	if err := r.adoptApplicationResource(ctx, app, entry, &existing); err != nil {
		return nil, err
	}
	if err := r.verifyResource(app, entry, &existing); err != nil {
		return nil, err
	}
	if !existing.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("credential %s is being deleted", entry.Name)
	}
	return &existing, nil
}

func (r *Resources) EnsureMigration(ctx context.Context, app *platform.Application, entry platform.ResourceBinding, spec dboperator.PostgresMigrationSetSpec) (*dboperator.PostgresMigrationSet, error) {
	if err := r.validateInventory(app); err != nil {
		return nil, err
	}
	if r.inventory.Spec.Releasing || !r.inventory.DeletionTimestamp.IsZero() || entry.Disposition == platform.ResourceRelease {
		return nil, fmt.Errorf("inventory resource is being released")
	}
	if err := verifyApplicationData(app, entry); err != nil {
		return nil, err
	}
	if entry.Kind != "PostgresMigrationSet" {
		return nil, fmt.Errorf("invalid migration resource kind")
	}
	if err := verifyBinding(entry, &dboperator.PostgresMigrationSet{Spec: spec}); err != nil {
		return nil, err
	}
	var existing dboperator.PostgresMigrationSet
	err := r.getObserved(ctx, types.NamespacedName{Namespace: entry.Namespace, Name: entry.Name}, &existing)
	if apierrors.IsNotFound(err) {
		if entry.UID != "" {
			return nil, fmt.Errorf("inventoried migration %s disappeared", entry.Name)
		}
		if entry.Name != ApplicationResourceName(app, entry.Kind, entry.LogicalName) {
			return nil, fmt.Errorf("invalid migration creation identity")
		}
		existing = dboperator.PostgresMigrationSet{ObjectMeta: r.resourceMetadata(app, entry), Spec: spec}
		if err := r.client.Create(ctx, &existing); err != nil {
			return nil, err
		}
		return &existing, nil
	}
	if err != nil {
		return nil, err
	}
	if err := r.adoptApplicationResource(ctx, app, entry, &existing); err != nil {
		return nil, err
	}
	if err := r.verifyResource(app, entry, &existing); err != nil {
		return nil, err
	}
	if !existing.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("migration %s is being deleted", entry.Name)
	}
	if !reflect.DeepEqual(existing.Spec, spec) {
		before := existing.DeepCopy()
		existing.Spec = spec
		if err := r.client.Patch(ctx, &existing, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, err
		}
		// Never treat a prior run's status as the observation of a new request.
		existing.Status.Phase = dboperator.MigrationSetPhasePending
	}
	return &existing, nil
}

func (r *Resources) CredentialSecret(ctx context.Context, cred *dboperator.PostgresCredential) (*corev1.Secret, error) {
	if !cred.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("credential is being deleted")
	}
	var secret corev1.Secret
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: cred.Namespace, Name: cred.Spec.SecretName}, &secret); err != nil {
		return nil, err
	}
	if err := verifySecret(&secret, cred, "PostgresCredential"); err != nil {
		return nil, err
	}
	if string(secret.Data["PGUSER"]) != cred.Spec.Username {
		return nil, fmt.Errorf("credential Secret username differs from binding")
	}
	return &secret, nil
}

func verifySecret(secret *corev1.Secret, resource client.Object, kind string) error {
	owner := metav1.GetControllerOf(secret)
	if resource.GetUID() == "" || owner == nil || owner.UID != resource.GetUID() || owner.Name != resource.GetName() || owner.Kind != kind || owner.APIVersion != dboperator.GroupVersion.String() || !secret.DeletionTimestamp.IsZero() {
		return fmt.Errorf("Secret %s/%s lacks verified credential ownership", secret.Namespace, secret.Name)
	}
	return nil
}

// RemoveApplicationResource fences deletion by inventoried UID and waits for foreground cleanup.
func (r *Resources) RemoveApplicationResource(ctx context.Context, app *platform.Application, entry platform.ResourceBinding) (done bool, uid string, err error) {
	var obj client.Object
	switch entry.Kind {
	case "PostgresCredential":
		obj = &dboperator.PostgresCredential{}
	case "PostgresMigrationSet":
		obj = &dboperator.PostgresMigrationSet{}
	default:
		return false, "", fmt.Errorf("resource kind %s is not application-owned", entry.Kind)
	}
	err = r.getObserved(ctx, types.NamespacedName{Namespace: entry.Namespace, Name: entry.Name}, obj)
	if apierrors.IsNotFound(err) {
		// A deleted credential's Secret can outlive its parent while GC converges.
		if entry.SecretName != "" {
			var secret corev1.Secret
			secretErr := r.getObserved(ctx, types.NamespacedName{Namespace: entry.Namespace, Name: entry.SecretName}, &secret)
			if secretErr == nil {
				owner := metav1.GetControllerOf(&secret)
				if entry.UID == "" || owner == nil || string(owner.UID) != entry.UID || owner.Name != entry.Name || owner.Kind != entry.Kind || owner.APIVersion != dboperator.GroupVersion.String() {
					return false, "", fmt.Errorf("remaining credential Secret has conflicting ownership")
				}
				return false, entry.UID, nil
			}
			if !apierrors.IsNotFound(secretErr) {
				return false, "", secretErr
			}
		}
		return true, entry.UID, nil
	}
	if err != nil {
		return false, "", err
	}
	if entry.UID == "" {
		if err := r.verifyResource(app, entry, obj); err != nil {
			return false, "", err
		}
		return false, string(obj.GetUID()), nil
	}
	if err := r.adoptApplicationResource(ctx, app, entry, obj); err != nil {
		return false, "", err
	}
	if err := r.verifyResource(app, entry, obj); err != nil {
		return false, "", err
	}
	if obj.GetDeletionTimestamp().IsZero() {
		uid := types.UID(entry.UID)
		resourceVersion := obj.GetResourceVersion()
		err := r.client.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}, client.PropagationPolicy(metav1.DeletePropagationForeground))
		if err != nil && !apierrors.IsNotFound(err) {
			return false, "", err
		}
	}
	return false, entry.UID, nil
}
