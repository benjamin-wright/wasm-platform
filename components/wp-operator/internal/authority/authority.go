package authority

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrUnavailable = errors.New("configuration authority is not initialized")

type applicationState struct {
	uid        types.UID
	generation int64
}

// Authority fences publication until the elected leader has processed its startup inventory.
type Authority struct {
	mu       sync.RWMutex
	ctx      context.Context
	cancel   context.CancelFunc
	pending  map[types.NamespacedName]applicationState
	observed map[types.NamespacedName]applicationState
	ready    bool
}

func New() *Authority {
	return &Authority{observed: make(map[types.NamespacedName]applicationState)}
}

// Initialize records the leader's startup inventory. Previously completed reconciliations
// count only when they cover the same Application identity and desired generation.
func (a *Authority) Initialize(ctx context.Context, applications []wasmplatformv1alpha1.Application) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ctx, a.cancel = context.WithCancel(ctx)
	if a.observed == nil {
		a.observed = make(map[types.NamespacedName]applicationState)
	}
	a.refreshLocked(applications)
}

func (a *Authority) refreshLocked(applications []wasmplatformv1alpha1.Application) {
	a.pending = make(map[types.NamespacedName]applicationState)
	for i := range applications {
		app := &applications[i]
		key := client.ObjectKeyFromObject(app)
		state := applicationState{uid: app.UID, generation: app.Generation}
		completed, ok := a.observed[key]
		if !ok || completed.uid != state.uid || completed.generation < state.generation {
			a.pending[key] = state
		}
	}
	a.ready = len(a.pending) == 0
	if a.ready {
		a.observed = nil
	}
}

// Reconciled records an actual completed publication, deletion or explicit exclusion.
// Retrying or failed reconciliations must not call this method.
func (a *Authority) Reconciled(key types.NamespacedName, uid types.UID, generation int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx != nil && a.ctx.Err() != nil {
		return
	}
	if a.ctx == nil || !a.ready {
		a.observed[key] = applicationState{uid: uid, generation: generation}
	}
	if a.ctx == nil {
		return
	}
	if pending, ok := a.pending[key]; ok && (uid == "" || (uid == pending.uid && generation >= pending.generation)) {
		delete(a.pending, key)
	}
	a.ready = len(a.pending) == 0
	if a.ready {
		a.observed = nil
	}
}

// Context returns the live publication authority; its cancellation fences existing sessions.
func (a *Authority) Context() (context.Context, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.ready || a.ctx == nil || a.ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	return a.ctx, nil
}

func (a *Authority) Ready(_ *http.Request) error {
	_, err := a.Context()
	return err
}

func (a *Authority) Revoke() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ready = false
	if a.cancel != nil {
		a.cancel()
	}
}

// Initializer is run by the manager only after winning leader election.
type Initializer struct {
	Authority *Authority
	Reader    client.Reader
}

func (*Initializer) NeedLeaderElection() bool { return true }

func (i *Initializer) Start(ctx context.Context) error {
	defer i.Authority.Revoke()
	applications, err := i.inventory(ctx)
	if err != nil {
		return errors.New("reading startup Application inventory failed")
	}
	i.Authority.Initialize(ctx, applications)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for i.Authority.Ready(nil) != nil {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			applications, err := i.inventory(ctx)
			if err != nil {
				return errors.New("refreshing startup Application inventory failed")
			}
			i.Authority.mu.Lock()
			if !i.Authority.ready {
				i.Authority.refreshLocked(applications)
			}
			i.Authority.mu.Unlock()
		}
	}
	<-ctx.Done()
	return nil
}

func (i *Initializer) inventory(ctx context.Context) ([]wasmplatformv1alpha1.Application, error) {
	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var applications wasmplatformv1alpha1.ApplicationList
	err := i.Reader.List(listCtx, &applications)
	return applications.Items, err
}
