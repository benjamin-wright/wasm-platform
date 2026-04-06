package routestore



















































































































import (
	"reflect"
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/types"
)

// RouteConfig is the gateway-facing description of a single route entry.
type RouteConfig struct {
	Path        string
	Methods     []string
	NatsSubject string
}

// RouteUpdate is a single route add/update/delete operation.
type RouteUpdate struct {
	Config *RouteConfig
	Delete bool
}

// RouteUpdateBatch is a versioned set of route updates broadcast to connected gateways.
type RouteUpdateBatch struct {
	Version   string
	Updates   []*RouteUpdate
	Timestamp int64
}

// gatewayEntry holds the channel used to deliver route updates to a connected gateway stream.
type gatewayEntry struct {
	ch chan *RouteUpdateBatch
}

// Store is a thread-safe in-memory registry of RouteConfig values for HTTP-type Applications.
// It also maintains a registry of connected gateway streams so that the reconciler can
// broadcast incremental route updates.
type Store struct {
	mu       sync.RWMutex
	routes   map[types.NamespacedName]*RouteConfig
	gateways map[string]*gatewayEntry
	version  uint64 // accessed atomically
}

// New returns an initialised Store.
func New() *Store {
	return &Store{
		routes:   make(map[types.NamespacedName]*RouteConfig),
		gateways: make(map[string]*gatewayEntry),
	}
}

// Version returns the current monotonic version counter.
func (s *Store) Version() uint64 {
	return atomic.LoadUint64(&s.version)
}

// Set stores or replaces the route config for key. It returns true if the config
// materially changed (i.e. the new value differs from the existing one).
// The version counter is only incremented on a real change.
func (s *Store) Set(key types.NamespacedName, cfg *RouteConfig) bool {
	s.mu.Lock()
	existing := s.routes[key]
	if reflect.DeepEqual(existing, cfg) {
		s.mu.Unlock()
		return false
	}
	s.routes[key] = cfg
	s.mu.Unlock()
	atomic.AddUint64(&s.version, 1)
	return true
}

// Delete removes the route config for key and increments the version.
func (s *Store) Delete(key types.NamespacedName) {
	s.mu.Lock()
	delete(s.routes, key)
	s.mu.Unlock()
	atomic.AddUint64(&s.version, 1)
}

// Snapshot returns a shallow copy of all stored RouteConfig pointers.
func (s *Store) Snapshot() []*RouteConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*RouteConfig, 0, len(s.routes))
	for _, v := range s.routes {
		out = append(out, v)
	}
	return out
}

// RegisterGateway adds a connected gateway to the registry and returns the channel
// on which route updates will be delivered.
func (s *Store) RegisterGateway(gatewayID string) chan *RouteUpdateBatch {
	ch := make(chan *RouteUpdateBatch, 16)
	s.mu.Lock()
	s.gateways[gatewayID] = &gatewayEntry{ch: ch}
	s.mu.Unlock()
	return ch
}

// DeregisterGateway removes a gateway from the registry and closes its channel.
func (s *Store) DeregisterGateway(gatewayID string) {
	s.mu.Lock()
	if entry, ok := s.gateways[gatewayID]; ok {
		delete(s.gateways, gatewayID)
		close(entry.ch)
	}
	s.mu.Unlock()
}

// BroadcastUpdate fans a RouteUpdateBatch out to every registered gateway.
// If a gateway's channel buffer is full the gateway is deregistered (it will
// reconnect and request a full route snapshot).
func (s *Store) BroadcastUpdate(update *RouteUpdateBatch) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var slow []string
	for id, entry := range s.gateways {
		select {
		case entry.ch <- update:
		default:
			slow = append(slow, id)
		}
	}

	for _, id := range slow {
		entry := s.gateways[id]
		delete(s.gateways, id)
		close(entry.ch)
	}
}