package configstore

import (
	"sync"
	"sync/atomic"

	configsync "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/configsync"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/types"
)

// hostEntry holds the channel used to deliver updates to a connected execution-host stream.
type hostEntry struct {
	ch chan *configsync.IncrementalConfig
}

// Store is a thread-safe in-memory registry of ApplicationConfig values and
// infrastructure connection configuration.  It also maintains a registry of
// connected execution-host streams so that the reconciler can broadcast
// incremental updates.
type Store struct {
	mu      sync.RWMutex
	configs map[types.NamespacedName]*configsync.ApplicationConfig
	hosts   map[string]*hostEntry
	version uint64 // accessed atomically

	// Infrastructure connection config pushed to all execution hosts.
	// nil means the resource is not yet provisioned.
	natsConfig  *configsync.NatsConnectionConfig
	redisConfig *configsync.RedisConnectionConfig
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

// Set stores or replaces the config for key. It returns true if the config
// materially changed (i.e. the new value differs from the existing one).
// The version counter is only incremented on a real change.
func (s *Store) Set(key types.NamespacedName, cfg *configsync.ApplicationConfig) bool {
	s.mu.Lock()
	existing := s.configs[key]
	if proto.Equal(existing, cfg) {
		s.mu.Unlock()
		return false
	}
	s.configs[key] = cfg
	s.mu.Unlock()
	atomic.AddUint64(&s.version, 1)
	return true
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

// SetNatsConfig updates the stored NATS connection config.  Returns true if
// the value materially changed (caller should broadcast an update).
// Pass nil to signal that NATS is no longer provisioned.
func (s *Store) SetNatsConfig(cfg *configsync.NatsConnectionConfig) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if proto.Equal(s.natsConfig, cfg) {
		return false
	}
	s.natsConfig = cfg
	atomic.AddUint64(&s.version, 1)
	return true
}

// SetRedisConfig updates the stored Redis connection config.  Returns true if
// the value materially changed (caller should broadcast an update).
// Pass nil to signal that Redis is no longer provisioned.
func (s *Store) SetRedisConfig(cfg *configsync.RedisConnectionConfig) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if proto.Equal(s.redisConfig, cfg) {
		return false
	}
	s.redisConfig = cfg
	atomic.AddUint64(&s.version, 1)
	return true
}

// NatsConfig returns the current NATS connection config (may be nil).
func (s *Store) NatsConfig() *configsync.NatsConnectionConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.natsConfig
}

// RedisConfig returns the current Redis connection config (may be nil).
func (s *Store) RedisConfig() *configsync.RedisConnectionConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.redisConfig
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
