package configstore

import (
	"sync"
	"sync/atomic"

	configsync "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/configsync"
	"k8s.io/apimachinery/pkg/types"
)

// hostEntry holds the channel used to deliver updates to a connected execution-host stream.
type hostEntry struct {
	ch chan *configsync.IncrementalConfig
}

// Store is a thread-safe in-memory registry of ApplicationConfig values.
// It also maintains a registry of connected execution-host streams so that
// the reconciler can broadcast incremental updates.
type Store struct {
	mu      sync.RWMutex
	configs map[types.NamespacedName]*configsync.ApplicationConfig
	hosts   map[string]*hostEntry
	version uint64 // accessed atomically
}

// New returns an initialised Store.
func New() *Store {
	return &Store{
		configs: make(map[types.NamespacedName]*configsync.ApplicationConfig),
		hosts:   make(map[string]*hostEntry),
	}
}

// Version returns the current monotonic version counter.
func (s *Store) Version() uint64 {
	return atomic.LoadUint64(&s.version)
}

// Set stores or replaces the config for key and increments the version.
func (s *Store) Set(key types.NamespacedName, cfg *configsync.ApplicationConfig) {
	s.mu.Lock()
	s.configs[key] = cfg
	s.mu.Unlock()
	atomic.AddUint64(&s.version, 1)
}

// Delete removes the config for key and increments the version.
func (s *Store) Delete(key types.NamespacedName) {
	s.mu.Lock()
	delete(s.configs, key)
	s.mu.Unlock()
	atomic.AddUint64(&s.version, 1)
}

// Snapshot returns a shallow copy of all stored ApplicationConfig pointers.
func (s *Store) Snapshot() []*configsync.ApplicationConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*configsync.ApplicationConfig, 0, len(s.configs))
	for _, v := range s.configs {
		out = append(out, v)
	}
	return out
}

// RegisterHost adds a connected host to the registry and returns the channel
// on which incremental updates will be delivered.
func (s *Store) RegisterHost(hostID string) chan *configsync.IncrementalConfig {
	ch := make(chan *configsync.IncrementalConfig, 16)
	s.mu.Lock()
	s.hosts[hostID] = &hostEntry{ch: ch}
	s.mu.Unlock()
	return ch
}

// DeregisterHost removes a host from the registry and closes its channel.
func (s *Store) DeregisterHost(hostID string) {
	s.mu.Lock()
	if entry, ok := s.hosts[hostID]; ok {
		delete(s.hosts, hostID)
		close(entry.ch)
	}
	s.mu.Unlock()
}

// BroadcastUpdate fans an IncrementalConfig update out to every registered host.
// If a host's channel buffer is full the host is deregistered (it will reconnect
// and request a full config snapshot).
func (s *Store) BroadcastUpdate(update *configsync.IncrementalConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var slow []string
	for id, entry := range s.hosts {
		select {
		case entry.ch <- update:
		default:
			slow = append(slow, id)
		}
	}

	for _, id := range slow {
		entry := s.hosts[id]
		delete(s.hosts, id)
		close(entry.ch)
	}
}
