// Package replica identifies a backend process and writes its heartbeat.
//
// Every backend process (replica) has a process identity: the host name plus
// a random suffix (ProcessID). With REPLICA_HEARTBEAT_ENABLED=true the
// heartbeat writes it to the replica_heartbeats table every
// HeartbeatInterval and deletes the row on a clean shutdown, so the table
// lists the running processes. The database server clock sets and compares
// the heartbeat time, so replica clocks do not matter. A row older than
// StaleAfter belongs to a process that stopped without a clean shutdown; the
// replica-heartbeat-cleanup leader worker deletes such rows.
package replica

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"backend/internal/models"

	"github.com/google/uuid"
)

const (
	// HeartbeatInterval is how often a process writes its heartbeat.
	HeartbeatInterval = 30 * time.Second
	// StaleAfter is the age of the last heartbeat after which a process
	// counts as stopped (four missed heartbeats).
	StaleAfter = 2 * time.Minute
	// maxProcessIDLen is the size of replica_heartbeats.id and
	// leader_leases.holder.
	maxProcessIDLen = 253
	// beatTimeout limits one heartbeat write.
	beatTimeout = 10 * time.Second
	// fallbackIdentity is used when the process has no host name.
	fallbackIdentity = "backend"
)

// ProcessID returns a new process identity: identity (the host name) plus
// "-" and 8 random hex characters. Two processes with the same host name
// (Compose keeps the host name over a restart) get different IDs, so the
// row of a stopped process never becomes fresh again. The result fits 253
// characters and contains nothing secret.
func ProcessID(identity string) string {
	if identity == "" {
		identity = fallbackIdentity
	}
	const maxIdentity = maxProcessIDLen - 9 // "-" + 8 hex characters
	if len(identity) > maxIdentity {
		identity = identity[:maxIdentity]
	}
	return identity + "-" + uuid.New().String()[:8]
}

// Heartbeat writes the heartbeat row of one process.
type Heartbeat struct {
	repo     models.ReplicaHeartbeatRepository
	cancel   context.CancelFunc
	done     chan struct{}
	id       string
	interval time.Duration
	mu       sync.Mutex
}

// NewHeartbeat creates a heartbeat for the process id. An interval <= 0
// uses HeartbeatInterval. A nil repo or an empty id returns nil; the methods
// of a nil *Heartbeat do nothing.
func NewHeartbeat(repo models.ReplicaHeartbeatRepository, id string, interval time.Duration) *Heartbeat {
	if repo == nil || id == "" {
		return nil
	}
	if interval <= 0 {
		interval = HeartbeatInterval
	}
	return &Heartbeat{repo: repo, id: id, interval: interval}
}

// ID returns the process identity, or "" for a nil heartbeat.
func (h *Heartbeat) ID() string {
	if h == nil {
		return ""
	}
	return h.id
}

// Start writes the first heartbeat at once and then every interval, in a
// goroutine. A second Start does nothing; Start after Stop starts again.
func (h *Heartbeat) Start() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go h.run(ctx, h.done)
	slog.Info("Replica heartbeat started", "replica_id", h.id, "interval", h.interval)
}

// run writes the heartbeats until ctx ends. failing (local to this
// goroutine, so a restarted heartbeat never shares it) is true after a
// failed write.
func (h *Heartbeat) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	failing := h.beat(ctx, false)
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			failing = h.beat(ctx, failing)
		}
	}
}

// beat writes one heartbeat and returns whether it failed. A failure is
// logged once until the next success; the heartbeat goes on.
func (h *Heartbeat) beat(ctx context.Context, failing bool) bool {
	bctx, cancel := context.WithTimeout(ctx, beatTimeout)
	defer cancel()
	err := h.repo.Beat(bctx, h.id)
	if err != nil {
		if ctx.Err() != nil {
			return failing
		}
		if !failing {
			slog.Error("Replica heartbeat write failed", "replica_id", h.id, "error", err)
		}
		return true
	}
	if failing {
		slog.Info("Replica heartbeat write works again", "replica_id", h.id)
	}
	return false
}

// Stop ends the heartbeat and deletes the row of the process. The whole call
// (waiting for a write in progress and the delete) takes at most timeout. A
// second Stop does nothing.
func (h *Heartbeat) Stop(timeout time.Duration) {
	if h == nil {
		return
	}
	h.mu.Lock()
	cancel, done := h.cancel, h.done
	h.cancel, h.done = nil, nil
	h.mu.Unlock()
	if cancel == nil {
		return
	}
	ctx, cancelRemove := context.WithTimeout(context.Background(), timeout)
	defer cancelRemove()
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
		// The write in progress ignores its cancelled context; delete
		// anyway. If it lands later, the row is stale and cleaned up.
		slog.Warn("Replica heartbeat write did not stop in time", "replica_id", h.id)
	}
	if err := h.repo.Remove(ctx, h.id); err != nil {
		slog.Warn("Replica heartbeat row not deleted", "replica_id", h.id, "error", err)
	}
}
