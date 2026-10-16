//go:build integration

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

type lifecycleApplication struct {
	Metadata struct {
		UID        string `json:"uid"`
		Generation int64  `json:"generation"`
	} `json:"metadata"`
	Spec   lifecycleSpec `json:"spec"`
	Status struct {
		Conditions []struct {
			Type, Status, Reason string
			ObservedGeneration   int64 `json:"observedGeneration"`
		} `json:"conditions"`
		ResolvedModules   []resolvedModule    `json:"resolvedModules"`
		ResourceInventory []lifecycleResource `json:"resourceInventory"`
	} `json:"status"`
}

type lifecycleSpec struct {
	ResourceCleanupPolicy string              `json:"resourceCleanupPolicy,omitempty"`
	Functions             []lifecycleFunction `json:"functions"`
	SQL                   *lifecycleSQL       `json:"sql,omitempty"`
	KV                    bool                `json:"kv,omitempty"`
}

type lifecycleFunction struct {
	Name    string `json:"name"`
	Module  string `json:"module"`
	SQLUser string `json:"sqlUser,omitempty"`
	Trigger struct {
		HTTP *struct {
			Path    string   `json:"path"`
			Methods []string `json:"methods,omitempty"`
		} `json:"http,omitempty"`
	} `json:"trigger"`
}

type lifecycleSQL struct {
	Migrations *struct {
		Artifact       string `json:"artifact"`
		TargetRevision int64  `json:"targetRevision"`
	} `json:"migrations,omitempty"`
	Users []struct {
		Name        string          `json:"name"`
		Permissions json.RawMessage `json:"permissions,omitempty"`
	} `json:"users,omitempty"`
}

type lifecycleResource struct {
	Kind        string `json:"kind"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	UID         string `json:"uid"`
	LogicalName string `json:"logicalName"`
	SecretName  string `json:"secretName"`
	Retained    bool   `json:"retained"`
}

type lifecycleResourceObject struct {
	Metadata struct {
		UID               string                             `json:"uid"`
		ResourceVersion   string                             `json:"resourceVersion"`
		DeletionTimestamp string                             `json:"deletionTimestamp"`
		Finalizers        []string                           `json:"finalizers"`
		OwnerReferences   []struct{ Kind, Name, UID string } `json:"ownerReferences"`
	} `json:"metadata"`
}

func TestApplicationDataRetentionAndOwnedResourceCleanup(t *testing.T) {
	g := NewWithT(t)
	const namespace = "default"
	const name = "lifecycle-retained-data"
	const path = "/lifecycle-retained-data"
	endpoint := "http://localhost" + path

	sqlFixture, err := lifecycleReadApplication(t.Context(), "default", "sql-hello")
	g.Expect(err).NotTo(HaveOccurred())
	counterFixture, err := lifecycleReadApplication(t.Context(), "examples", "counter-app")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sqlFixture.Spec.SQL).NotTo(BeNil())
	g.Expect(sqlFixture.Spec.SQL.Migrations).NotTo(BeNil())
	g.Expect(counterFixture.Spec.Functions).To(HaveLen(1))

	spec := sqlFixture.Spec
	spec.KV = true
	for i := range spec.Functions {
		function := &spec.Functions[i]
		function.Module = lifecycleModule(t, sqlFixture, function.Name)
		g.Expect(function.Trigger.HTTP).NotTo(BeNil())
		function.Trigger.HTTP.Path = path + strings.TrimPrefix(function.Trigger.HTTP.Path, "/sql-hello")
	}
	counter := counterFixture.Spec.Functions[0]
	counter.Name = "counter"
	counter.Module = lifecycleModule(t, counterFixture, counterFixture.Spec.Functions[0].Name)
	counter.Trigger.HTTP.Path = path + "/counter"
	spec.Functions = append(spec.Functions, counter)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), migrationFailedTimeout)
		defer cancel()
		if err := lifecycleDeleteApplication(ctx, namespace, name); err != nil {
			t.Errorf("lifecycle Application cleanup failed: %v", err)
		}
	})
	g.Expect(lifecycleDeleteApplication(pollContext(t, migrationFailedTimeout), namespace, name)).To(Succeed())
	lifecycleApplyApplication(t, namespace, name, spec)
	initial := lifecycleWaitReady(t, namespace, name)
	initialInventory := lifecycleReadInventory(t, namespace, initial.Metadata.UID)
	g.Expect(initialInventory.Metadata.OwnerReferences).To(BeEmpty())
	g.Expect(initialInventory.Spec.ApplicationRef.UID).To(Equal(initial.Metadata.UID))
	reader := lifecycleInventoryResource(t, initial, "PostgresCredential", "reader")
	writer := lifecycleInventoryResource(t, initial, "PostgresCredential", "writer")
	migrations := lifecycleInventoryResource(t, initial, "PostgresMigrationSet", "migrations")
	postgres := lifecycleInventoryResource(t, initial, "PostgresDatabase", "infrastructure")
	redis := lifecycleInventoryResource(t, initial, "RedisDatabase", "infrastructure")
	redisCredential := lifecycleInventoryResource(t, initial, "RedisCredential", "infrastructure")
	for _, resource := range []lifecycleResource{reader, writer, migrations} {
		g.Expect(resource.Retained).To(BeFalse(), resource.Kind+" must follow the current Application's lifetime")
		child, err := lifecycleReadResource(t.Context(), resource)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(child.Metadata.OwnerReferences).To(HaveLen(1))
		g.Expect(child.Metadata.OwnerReferences[0].Kind).To(Equal("ResourceInventory"))
		g.Expect(child.Metadata.OwnerReferences[0].UID).To(Equal(initialInventory.Metadata.UID))
	}
	retained := []lifecycleResource{postgres, redis, redisCredential}
	for _, resource := range retained {
		g.Expect(resource.Retained).To(BeTrue(), resource.Kind+" must survive Application deletion")
	}
	lifecycleWaitHTTP(t, http.MethodPost, endpoint+"/setup", nil)
	lifecycleWaitSQLRows(t, endpoint)
	var previousCounter int
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (int, error) {
		value, err := fetchCounter(ctx, endpoint+"/counter")
		previousCounter = value
		return value, err
	}, routeTimeout, pollInterval).Should(BeNumerically(">", 0))

	edited := lifecycleCopySpec(t, spec)
	edited.SQL.Users = nil
	for _, user := range spec.SQL.Users {
		if user.Name != "reader" {
			edited.SQL.Users = append(edited.SQL.Users, user)
		}
	}
	edited.Functions = nil
	for _, function := range spec.Functions {
		if function.SQLUser != "reader" {
			edited.Functions = append(edited.Functions, function)
		}
	}
	lifecycleApplyApplication(t, namespace, name, edited)
	lifecycleWaitReady(t, namespace, name)
	lifecycleAssertResourcesRemoved(t, reader)

	g.Expect(lifecycleSetDeletionBarrier(t.Context(), migrations, true)).To(Succeed())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), kubectlTimeout)
		defer cancel()
		if err := lifecycleSetDeletionBarrier(ctx, migrations, false); err != nil {
			t.Errorf("removing status-loss barrier: %v", err)
		}
	})
	edited.SQL = nil
	edited.Functions = []lifecycleFunction{counter}
	lifecycleApplyApplication(t, namespace, name, edited)
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (string, error) {
		child, err := lifecycleReadResource(ctx, migrations)
		return child.Metadata.DeletionTimestamp, err
	}, routeTimeout, pollInterval).ShouldNot(BeEmpty())
	lifecycleClearStatus(t, "application", namespace, name)
	lifecycleClearStatus(t, "resourceinventory", namespace, initialInventory.Metadata.Name)
	afterReset := lifecycleReadInventory(t, namespace, initial.Metadata.UID)
	g.Expect(afterReset.Spec.Resources).To(ContainElement(HaveField("UID", migrations.UID)))
	g.Expect(lifecycleSetDeletionBarrier(t.Context(), migrations, false)).To(Succeed())
	lifecycleWaitReady(t, namespace, name)
	lifecycleAssertResourcesRemoved(t, writer, migrations)
	lifecycleAssertRetained(t, retained)

	g.Expect(lifecycleDeleteApplication(pollContext(t, migrationFailedTimeout), namespace, name)).To(Succeed())
	lifecycleAssertRetained(t, retained)
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (int, error) {
		status, _, err := requestHTTP(ctx, testHTTPClient, http.MethodGet, endpoint+"/counter", "", nil)
		return status, err
	}, routeTimeout, pollInterval).Should(Equal(http.StatusNotFound), "deleted Application routes must stop serving")

	lifecycleApplyApplication(t, namespace, name, spec)
	recreated := lifecycleWaitReady(t, namespace, name)
	g.Expect(recreated.Metadata.UID).NotTo(Equal(initial.Metadata.UID), "recreation must establish a new authorization owner")
	for _, previous := range []lifecycleResource{reader, writer, migrations} {
		current := lifecycleInventoryResource(t, recreated, previous.Kind, previous.LogicalName)
		g.Expect(current.UID).NotTo(Equal(previous.UID), "recreated Application must provision fresh owned "+previous.Kind)
	}
	lifecycleAssertRetained(t, retained)
	lifecycleWaitSQLRows(t, endpoint)
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (int, error) {
		return fetchCounter(ctx, endpoint+"/counter")
	}, routeTimeout, pollInterval).Should(BeNumerically(">", previousCounter), "same namespace/name must retain its KV data across recreation")

	var recreatedOwned []lifecycleResource
	for _, resource := range recreated.Status.ResourceInventory {
		if !resource.Retained {
			recreatedOwned = append(recreatedOwned, resource)
		}
	}
	heldMigration := lifecycleInventoryResource(t, recreated, "PostgresMigrationSet", "migrations")
	g.Expect(lifecycleSetDeletionBarrier(t.Context(), heldMigration, true)).To(Succeed())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), kubectlTimeout)
		defer cancel()
		if err := lifecycleSetDeletionBarrier(ctx, heldMigration, false); err != nil {
			t.Errorf("removing lifecycle deletion barrier: %v", err)
		}
	})
	g.Expect(lifecycleStartDeleteApplication(t.Context(), namespace, name)).To(Succeed())
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (bool, error) {
		child, err := lifecycleReadResource(ctx, heldMigration)
		if err != nil {
			return false, err
		}
		if child.Metadata.UID != heldMigration.UID {
			return false, fmt.Errorf("held migration identity changed before cleanup completed")
		}
		return child.Metadata.DeletionTimestamp != "", nil
	}, routeTimeout, pollInterval).Should(BeTrue(), "Application deletion must request owned child cleanup")
	g.Consistently(pollContext(t, 5*time.Second+kubectlTimeout), func(ctx context.Context) (string, error) {
		app, err := lifecycleReadApplication(ctx, namespace, name)
		return app.Metadata.UID, err
	}, 5*time.Second, pollInterval).Should(Equal(recreated.Metadata.UID), "Application finalization must wait for downstream deletion completion")
	// Transfer a blocked deletion to the independent inventory.
	output, err := kubectlOutput(t.Context(), kubectlTimeout, nil, true, "patch", "application", name, "-n", namespace, "--type=merge", "--patch", `{"spec":{"resourceCleanupPolicy":"OrphanInventory"}}`)
	g.Expect(err).NotTo(HaveOccurred(), string(output))
	g.Expect(lifecycleDeleteApplication(pollContext(t, migrationFailedTimeout), namespace, name)).To(Succeed())
	orphan := lifecycleReadInventory(t, namespace, recreated.Metadata.UID)
	g.Expect(orphan.Spec.Releasing).To(BeTrue())
	for _, patch := range []string{`{"spec":{"releasing":false}}`, `{"spec":{"releasing":null}}`} {
		_, err := kubectlOutput(t.Context(), kubectlTimeout, nil, true, "patch", "resourceinventory", orphan.Metadata.Name, "-n", namespace, "--type=merge", "--patch", patch)
		g.Expect(err).To(HaveOccurred(), "a released inventory cannot be reactivated")
	}

	g.Expect(orphan.Metadata.Finalizers).To(ContainElement("wasm-platform.io/inventory-protection"))
	g.Expect(orphan.Spec.Resources).To(ContainElement(HaveField("UID", heldMigration.UID)))
	lifecycleClearStatus(t, "resourceinventory", namespace, orphan.Metadata.Name)
	lifecycleApplyApplication(t, namespace, name, spec)
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (string, error) {
		app, err := lifecycleReadApplication(ctx, namespace, name)
		for _, condition := range app.Status.Conditions {
			if condition.Type == "Ready" {
				return condition.Reason, err
			}
		}
		return "", err
	}, routeTimeout, pollInterval).Should(Equal("ResourceCleanupPending"))
	g.Consistently(pollContext(t, 5*time.Second+kubectlTimeout), func(ctx context.Context) (int, error) {
		app, err := lifecycleReadApplication(ctx, namespace, name)
		return len(app.Status.ResourceInventory), err
	}, 5*time.Second, pollInterval).Should(BeZero(), "replacement must not provision while old cleanup is unresolved")
	g.Expect(lifecycleSetDeletionBarrier(t.Context(), heldMigration, false)).To(Succeed())
	lifecycleWaitReady(t, namespace, name)
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (string, error) {
		return lifecycleResourceUID(ctx, lifecycleResource{Kind: "ResourceInventory", Namespace: namespace, Name: orphan.Metadata.Name})
	}, routeTimeout, pollInterval).Should(BeEmpty())
	lifecycleAssertResourcesRemoved(t, recreatedOwned...)
	g.Expect(lifecycleDeleteApplication(pollContext(t, migrationFailedTimeout), namespace, name)).To(Succeed())
	lifecycleAssertRetained(t, retained)
}

func lifecycleReadApplication(ctx context.Context, namespace, name string) (lifecycleApplication, error) {
	var app lifecycleApplication
	data, err := kubectlOutput(ctx, kubectlTimeout, nil, false, "get", "application", name, "-n", namespace, "-o", "json")
	if err != nil {
		return app, err
	}
	err = json.Unmarshal(data, &app)
	return app, err
}

func lifecycleWaitReady(t *testing.T, namespace, name string) lifecycleApplication {
	t.Helper()
	var app lifecycleApplication
	NewWithT(t).Eventually(pollContext(t, migrationFailedTimeout), func(ctx context.Context) (bool, error) {
		var err error
		app, err = lifecycleReadApplication(ctx, namespace, name)
		if err != nil {
			return false, err
		}
		for _, condition := range app.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" && condition.ObservedGeneration == app.Metadata.Generation {
				return true, nil
			}
		}
		return false, fmt.Errorf("Application %s/%s has not reconciled generation %d", namespace, name, app.Metadata.Generation)
	}, migrationFailedTimeout, pollInterval).Should(BeTrue())
	return app
}

func lifecycleModule(t *testing.T, app lifecycleApplication, name string) string {
	t.Helper()
	for _, module := range app.Status.ResolvedModules {
		if module.Name == name && module.Generation == app.Metadata.Generation && module.ModuleRef != "" {
			return module.ModuleRef
		}
	}
	t.Fatalf("fixture has no current immutable module for function %s", name)
	return ""
}

func lifecycleCopySpec(t *testing.T, spec lifecycleSpec) lifecycleSpec {
	t.Helper()
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var copied lifecycleSpec
	if err := json.Unmarshal(data, &copied); err != nil {
		t.Fatal(err)
	}
	return copied
}

func lifecycleApplyApplication(t *testing.T, namespace, name string, spec lifecycleSpec) {
	t.Helper()
	manifest := map[string]any{
		"apiVersion": "wasm-platform.io/v1alpha1", "kind": "Application",
		"metadata": map[string]string{"name": name, "namespace": namespace},
		"spec":     spec,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := kubectlOutput(t.Context(), kubectlTimeout, bytes.NewReader(data), true, "apply", "-f", "-"); err != nil {
		t.Fatalf("applying lifecycle Application: %v: %s", err, output)
	}
}

func lifecycleInventoryResource(t *testing.T, app lifecycleApplication, kind, logicalName string) lifecycleResource {
	t.Helper()
	for _, resource := range app.Status.ResourceInventory {
		if resource.Kind == kind && resource.LogicalName == logicalName {
			if resource.Namespace == "" || resource.Name == "" || resource.UID == "" {
				t.Fatalf("incomplete inventory entry for %s %s", kind, logicalName)
			}
			return resource
		}
	}
	t.Fatalf("inventory has no %s %s", kind, logicalName)
	return lifecycleResource{}
}

func lifecycleReadResource(ctx context.Context, resource lifecycleResource) (lifecycleResourceObject, error) {
	var object lifecycleResourceObject
	data, err := kubectlOutput(ctx, kubectlTimeout, nil, false, "get", resource.Kind, resource.Name,
		"-n", resource.Namespace, "--ignore-not-found", "-o", "json")
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return object, err
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return object, err
	}
	if object.Metadata.UID == "" {
		return object, fmt.Errorf("%s %s/%s has no UID", resource.Kind, resource.Namespace, resource.Name)
	}
	return object, nil
}

func lifecycleResourceUID(ctx context.Context, resource lifecycleResource) (string, error) {
	object, err := lifecycleReadResource(ctx, resource)
	return object.Metadata.UID, err
}

func lifecycleAssertResourcesRemoved(t *testing.T, resources ...lifecycleResource) {
	t.Helper()
	for _, resource := range resources {
		uid, err := lifecycleResourceUID(t.Context(), resource)
		if err != nil {
			t.Fatal(err)
		}
		if uid != "" {
			t.Fatalf("owned %s %s/%s still exists after lifecycle cleanup completed", resource.Kind, resource.Namespace, resource.Name)
		}
	}
}

func lifecycleAssertRetained(t *testing.T, resources []lifecycleResource) {
	t.Helper()
	for _, resource := range resources {
		uid, err := lifecycleResourceUID(t.Context(), resource)
		if err != nil {
			t.Fatal(err)
		}
		if uid != resource.UID {
			t.Fatalf("retained %s %s/%s changed UID: got %q, want %q", resource.Kind, resource.Namespace, resource.Name, uid, resource.UID)
		}
	}
}

func lifecycleStartDeleteApplication(ctx context.Context, namespace, name string) error {
	if output, err := kubectlOutput(ctx, kubectlTimeout, nil, true, "delete", "application", name,
		"-n", namespace, "--ignore-not-found", "--wait=false"); err != nil {
		return fmt.Errorf("deleting lifecycle Application: %w: %s", err, output)
	}
	return nil
}

func lifecycleDeleteApplication(ctx context.Context, namespace, name string) error {
	if err := lifecycleStartDeleteApplication(ctx, namespace, name); err != nil {
		return err
	}
	for {
		data, err := kubectlOutput(ctx, kubectlTimeout, nil, false, "get", "application", name,
			"-n", namespace, "--ignore-not-found", "-o", "json")
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func lifecycleSetDeletionBarrier(ctx context.Context, resource lifecycleResource, enabled bool) error {
	const barrier = "wasm-platform.io/e2e-deletion-barrier"
	object, err := lifecycleReadResource(ctx, resource)
	if err != nil {
		return err
	}
	if object.Metadata.UID == "" && !enabled {
		return nil
	}
	if object.Metadata.UID != resource.UID {
		return fmt.Errorf("refusing to change deletion barrier for replaced %s %s/%s", resource.Kind, resource.Namespace, resource.Name)
	}
	finalizers := make([]string, 0, len(object.Metadata.Finalizers)+1)
	for _, finalizer := range object.Metadata.Finalizers {
		if finalizer != barrier {
			finalizers = append(finalizers, finalizer)
		}
	}
	if enabled {
		finalizers = append(finalizers, barrier)
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": object.Metadata.ResourceVersion,
		"finalizers":      finalizers,
	}})
	if err != nil {
		return err
	}
	if output, err := kubectlOutput(ctx, kubectlTimeout, nil, true, "patch", resource.Kind, resource.Name,
		"-n", resource.Namespace, "--type=merge", "--patch", string(patch)); err != nil {
		return fmt.Errorf("changing lifecycle deletion barrier: %w: %s", err, output)
	}
	return nil
}

func lifecycleWaitHTTP(t *testing.T, method, endpoint string, check func([]byte) bool) {
	t.Helper()
	NewWithT(t).Eventually(pollContext(t, routeTimeout), func(ctx context.Context) error {
		status, body, err := requestHTTP(ctx, testHTTPClient, method, endpoint, "", nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("%s %s returned HTTP %d", method, endpoint, status)
		}
		if check != nil && !check(body) {
			return fmt.Errorf("%s %s returned unexpected rows", method, endpoint)
		}
		return nil
	}, routeTimeout, pollInterval).Should(Succeed())
}

func lifecycleWaitSQLRows(t *testing.T, endpoint string) {
	t.Helper()
	lifecycleWaitHTTP(t, http.MethodGet, endpoint, func(body []byte) bool {
		return bytes.Contains(body, []byte(`"name":"Alice"`)) && bytes.Contains(body, []byte(`"name":"Bob"`)) && !bytes.Contains(body, []byte(`"name":"Carol"`))
	})
}

type lifecycleInventory struct {
	Metadata struct {
		Name, UID       string
		OwnerReferences []json.RawMessage `json:"ownerReferences"`
		Finalizers      []string          `json:"finalizers"`
	} `json:"metadata"`
	Spec struct {
		ApplicationRef struct{ Namespace, Name, UID string } `json:"applicationRef"`
		Releasing      bool                                  `json:"releasing"`
		Resources      []lifecycleResource                   `json:"resources"`
	} `json:"spec"`
}

func lifecycleReadInventory(t *testing.T, namespace, appUID string) lifecycleInventory {
	t.Helper()
	data, err := kubectlOutput(t.Context(), kubectlTimeout, nil, false, "get", "resourceinventories", "-n", namespace, "-l", "wasm-platform.io/application-uid="+appUID, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []lifecycleInventory `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected one inventory for Application %s; got %d", appUID, len(list.Items))
	}
	return list.Items[0]
}

func lifecycleClearStatus(t *testing.T, kind, namespace, name string) {
	t.Helper()
	output, err := kubectlOutput(t.Context(), kubectlTimeout, nil, true, "patch", kind, name, "-n", namespace, "--subresource=status", "--type=merge", "--patch", `{"status":null}`)
	if err != nil {
		t.Fatalf("clearing %s status: %v: %s", kind, err, output)
	}
}
