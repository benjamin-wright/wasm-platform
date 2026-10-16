package provisioning

import (
	"context"
	"testing"

	dboperator "github.com/benjamin-wright/db-operator/pkg/api/v1alpha1"
	platform "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func applicationCleanupFixture(kind string) (*platform.Application, platform.ResourceBinding, client.Object) {
	app := &platform.Application{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "orders", UID: "application-uid"}}
	entry := platform.ResourceBinding{
		Kind: kind, Namespace: app.Namespace, UID: "resource-uid", LogicalName: "reader",
		DatabaseRef: "wasm-apps-postgres", Database: PGDatabaseName(app.Namespace, app.Name),
	}
	if kind == "PostgresMigrationSet" {
		entry.LogicalName = "migrations"
	}
	entry.Name = ApplicationResourceName(app, kind, entry.LogicalName)
	metadata := applicationMetadata(app, entry)
	metadata.UID = "resource-uid"
	metadata.ResourceVersion = "1"
	if kind == "PostgresMigrationSet" {
		return app, entry, &dboperator.PostgresMigrationSet{
			ObjectMeta: metadata,
			Spec:       dboperator.PostgresMigrationSetSpec{DatabaseRef: entry.DatabaseRef, Database: entry.Database},
		}
	}
	entry.Username = PGUsername(app.Namespace, app.Name, entry.LogicalName)
	entry.SecretName = entry.Name + "-creds"
	return app, entry, &dboperator.PostgresCredential{
		ObjectMeta: metadata,
		Spec: dboperator.PostgresCredentialSpec{
			DatabaseRef: entry.DatabaseRef, Username: entry.Username, SecretName: entry.SecretName,
			Permissions: []dboperator.DatabasePermissionEntry{{Databases: []string{entry.Database}}},
		},
	}
}

type staleApplicationCache struct {
	client.Client
	hide func(client.Object) bool
}

func (c *staleApplicationCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if c.hide(obj) {
		return apierrors.NewNotFound(schema.GroupResource{Group: dboperator.GroupVersion.Group, Resource: "resources"}, key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestApplicationCleanupChecksDirectReaderAfterCachedChildMiss(t *testing.T) {
	for _, kind := range []string{"PostgresCredential", "PostgresMigrationSet"} {
		t.Run(kind, func(t *testing.T) {
			app, entry, child := applicationCleanupFixture(kind)
			child.SetFinalizers([]string{"tests.wasm-platform.io/hold-cleanup"})
			kubernetes := infrastructureTestClient(t, child)
			cached := &staleApplicationCache{Client: kubernetes, hide: func(obj client.Object) bool {
				switch obj.(type) {
				case *dboperator.PostgresCredential, *dboperator.PostgresMigrationSet:
					return true
				default:
					return false
				}
			}}
			done, uid, err := NewWithReader(cached, kubernetes).WithInventory(cleanupInventory(app)).RemoveApplicationResource(t.Context(), app, entry)
			if err != nil {
				t.Fatal(err)
			}
			if done || uid != entry.UID {
				t.Fatalf("cleanup after cache miss = (%v, %q), want pending for %q", done, uid, entry.UID)
			}
			observed := child.DeepCopyObject().(client.Object)
			if err := kubernetes.Get(t.Context(), client.ObjectKeyFromObject(child), observed); err != nil {
				t.Fatal(err)
			}
			if observed.GetDeletionTimestamp().IsZero() {
				t.Fatal("existing child was not found and scheduled for deletion")
			}
		})
	}
}

func TestApplicationCleanupWaitsForSecretMissingFromCache(t *testing.T) {
	app, entry, child := applicationCleanupFixture("PostgresCredential")
	controller := true
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: entry.Namespace, Name: entry.SecretName, UID: "secret-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: dboperator.GroupVersion.String(), Kind: entry.Kind,
			Name: child.GetName(), UID: child.GetUID(), Controller: &controller,
		}},
	}}
	kubernetes := infrastructureTestClient(t, secret)
	cached := &staleApplicationCache{Client: kubernetes, hide: func(obj client.Object) bool {
		_, isSecret := obj.(*corev1.Secret)
		return isSecret
	}}
	resources := NewWithReader(cached, kubernetes).WithInventory(cleanupInventory(app))
	done, uid, err := resources.RemoveApplicationResource(t.Context(), app, entry)
	if err != nil {
		t.Fatal(err)
	}
	if done || uid != entry.UID {
		t.Fatalf("cleanup with remaining Secret = (%v, %q), want pending for %q", done, uid, entry.UID)
	}
	if err := kubernetes.Delete(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	done, uid, err = resources.RemoveApplicationResource(t.Context(), app, entry)
	if err != nil {
		t.Fatal(err)
	}
	if !done || uid != entry.UID {
		t.Fatalf("cleanup after verified Secret absence = (%v, %q), want complete for %q", done, uid, entry.UID)
	}
}

type mutationBeforeDeleteClient struct {
	client.Client
	mutate func(client.Object)
}

func (c *mutationBeforeDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	updated := obj.DeepCopyObject().(client.Object)
	if err := c.Client.Get(ctx, client.ObjectKeyFromObject(obj), updated); err != nil {
		return err
	}
	c.mutate(updated)
	if err := c.Client.Update(ctx, updated); err != nil {
		return err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestApplicationCleanupRejectsConcurrentBindingChanges(t *testing.T) {
	for _, kind := range []string{"PostgresCredential", "PostgresMigrationSet"} {
		for _, mutation := range []struct {
			name   string
			mutate func(client.Object)
		}{
			{"owner", func(obj client.Object) {
				owners := obj.GetOwnerReferences()
				owners[0].UID = "another-application"
				obj.SetOwnerReferences(owners)
			}},
			{"database", func(obj client.Object) {
				switch resource := obj.(type) {
				case *dboperator.PostgresCredential:
					resource.Spec.DatabaseRef = "wasm-foreign-postgres"
				case *dboperator.PostgresMigrationSet:
					resource.Spec.DatabaseRef = "wasm-foreign-postgres"
				}
			}},
		} {
			t.Run(kind+"/"+mutation.name, func(t *testing.T) {
				app, entry, child := applicationCleanupFixture(kind)
				kubernetes := infrastructureTestClient(t, child)
				racing := &mutationBeforeDeleteClient{Client: kubernetes, mutate: mutation.mutate}
				done, _, err := New(racing).WithInventory(cleanupInventory(app)).RemoveApplicationResource(t.Context(), app, entry)
				if !apierrors.IsConflict(err) {
					t.Fatalf("delete after concurrent binding change error = %v, want conflict", err)
				}
				if done {
					t.Fatal("cleanup completed despite concurrent binding change")
				}
				observed := child.DeepCopyObject().(client.Object)
				if err := kubernetes.Get(t.Context(), client.ObjectKeyFromObject(child), observed); err != nil {
					t.Fatal(err)
				}
				if observed.GetUID() != child.GetUID() || !observed.GetDeletionTimestamp().IsZero() {
					t.Fatal("concurrently changed child was replaced or deleted")
				}
				if observed.GetResourceVersion() == child.GetResourceVersion() {
					t.Fatal("test did not persist the concurrent change")
				}
			})
		}
	}
}

func cleanupInventory(app *platform.Application) *platform.ResourceInventory {
	return &platform.ResourceInventory{ObjectMeta: metav1.ObjectMeta{Namespace: app.Namespace, Name: InventoryName(app.UID), UID: "inventory-uid"}, Spec: platform.ResourceInventorySpec{ApplicationRef: platform.InventoryApplicationReference{Namespace: app.Namespace, Name: app.Name, UID: string(app.UID)}}}
}
