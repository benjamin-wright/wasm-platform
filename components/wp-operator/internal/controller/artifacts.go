package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/artifacts"
)

func (r *ApplicationReconciler) resolveModules(ctx context.Context, app *wasmplatformv1alpha1.Application) (map[string]artifacts.Identity, error) {
	if r.Artifacts == nil {
		return nil, errors.New("artifact resolver is not initialized")
	}
	identities := make(map[string]artifacts.Identity, len(app.Spec.Functions))
	requested := make(map[string]string, len(app.Spec.Functions))
	for _, function := range app.Spec.Functions {
		if _, exists := requested[function.Name]; exists {
			return nil, errors.New("duplicate function name")
		}
		requested[function.Name] = function.Module
	}
	records := make(map[string]wasmplatformv1alpha1.ResolvedModuleStatus, len(app.Spec.Functions))
	for _, previous := range app.Status.ResolvedModules {
		if moduleRef, exists := requested[previous.Name]; exists && previous.Generation == app.Generation && previous.RequestedRef == moduleRef {
			records[previous.Name] = previous
		}
	}
	persist := func() error {
		var resolved []wasmplatformv1alpha1.ResolvedModuleStatus
		for _, function := range app.Spec.Functions {
			if record, exists := records[function.Name]; exists {
				resolved = append(resolved, record)
			}
		}
		if reflect.DeepEqual(resolved, app.Status.ResolvedModules) {
			return nil
		}
		app.Status.ResolvedModules = resolved
		if err := r.Status().Update(ctx, app); err != nil {
			return errors.New("persisting resolved module identities failed")
		}
		return nil
	}
	fail := func(err error) (map[string]artifacts.Identity, error) {
		if persistErr := persist(); persistErr != nil {
			return nil, persistErr
		}
		return nil, err
	}
	for _, function := range app.Spec.Functions {
		if err := r.Artifacts.Validate(function.Module); err != nil {
			return fail(fmt.Errorf("module policy validation failed: %w", err))
		}
		var identity artifacts.Identity
		if previous, exists := records[function.Name]; exists {
			identity = artifacts.Identity{ModuleRef: previous.ModuleRef, ManifestDigest: previous.ManifestDigest, WasmDigest: previous.WasmDigest}
			if artifacts.ValidateIdentity(identity) != nil || r.Artifacts.Validate(identity.ModuleRef) != nil {
				return fail(errors.New("persisted module identity is invalid"))
			}
		}
		if identity.ModuleRef == "" {
			var err error
			identity, err = r.Artifacts.Resolve(ctx, function.Module)
			if err != nil {
				return fail(fmt.Errorf("module resolution failed: %w", err))
			}
			if artifacts.ValidateIdentity(identity) != nil || r.Artifacts.Validate(identity.ModuleRef) != nil {
				return fail(errors.New("resolved module identity is invalid"))
			}
		}
		identities[function.Name] = identity
		records[function.Name] = wasmplatformv1alpha1.ResolvedModuleStatus{
			Name: function.Name, Generation: app.Generation, RequestedRef: function.Module,
			ModuleRef: identity.ModuleRef, ManifestDigest: identity.ManifestDigest, WasmDigest: identity.WasmDigest,
		}
	}
	if err := persist(); err != nil {
		return nil, err
	}
	return identities, nil
}
