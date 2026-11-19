//go:build integration

package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
	gorilla "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// outboxIntegrationConfig is a config with the outbox on against the MySQL of
// make test-backend-all. A short poll interval keeps the tests fast.
func outboxIntegrationConfig() *config.Config {
	cfg := bootstrapTestConfig()
	cfg.Database = integrationDBConfig()
	cfg.Outbox = config.OutboxConfig{Enabled: true, PollInterval: 20 * time.Millisecond, Retention: time.Hour}
	return cfg
}

// replicaStack is one backend process: its own pool, hub, poller and server.
type replicaStack struct {
	repo models.Repository
	base string
}

// startReplica builds a full process (database.NewRepository with the hook,
// buildServices with the poller, an HTTP server on port 0).
func startReplica(t *testing.T) *replicaStack {
	t.Helper()
	cfg := outboxIntegrationConfig()
	repo, err := initDatabase(cfg) // MySQL, migrations, the change hook
	require.NoError(t, err)
	s, router, err := buildServices(cfg, repo)
	require.NoError(t, err)
	srv := startHTTPServer(cfg, router)
	s.servers = append(s.servers, srv)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		assert.NoError(t, shutdown(ctx, s))
	})
	return &replicaStack{repo: repo, base: "http://" + srv.Addr}
}

func dialHub(t *testing.T, base string) *gorilla.Conn {
	t.Helper()
	conn, resp, err := gorilla.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/ws", nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	time.Sleep(100 * time.Millisecond) // the hub registers the client asynchronously
	return conn
}

func readEvent(t *testing.T, conn *gorilla.Conn) string {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	return string(msg)
}

func expectSilence(t *testing.T, conn *gorilla.Conn) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(400*time.Millisecond)))
	_, msg, err := conn.ReadMessage()
	assert.Error(t, err, "a second copy arrived: %s", msg)
}

// TestDatabaseOutboxCrossReplicaDelivery: an item written through replica A
// reaches a real socket on replica B, and the socket on A gets one copy.
func TestDatabaseOutboxCrossReplicaDelivery(t *testing.T) {
	// Not parallel: buildRouter sets the process-wide Gin mode.
	t.Setenv("GIN_MODE", gin.TestMode)
	a := startReplica(t)
	b := startReplica(t)
	socketA := dialHub(t, a.base)
	socketB := dialHub(t, b.base)

	status, body := call(t, http.MethodPost, a.base+"/api/v1/items", `{"name":"it-cross-replica","price":2.5}`, nil)
	require.Equal(t, http.StatusCreated, status, body)

	for name, conn := range map[string]*gorilla.Conn{"A (the writer)": socketA, "B (another replica)": socketB} {
		msg := readEvent(t, conn)
		assert.Contains(t, msg, `"type":"item.created"`, name)
		assert.Contains(t, msg, `"name":"it-cross-replica"`, name)
		expectSilence(t, conn)
	}
}

// TestDatabaseOutboxCrashBetweenCommitAndDelivery: the writer commits the
// change and the event and never delivers (it has no poller, like a process
// that died right after the commit). Another replica's poller delivers it.
func TestDatabaseOutboxCrashBetweenCommitAndDelivery(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	b := startReplica(t)
	socketB := dialHub(t, b.base)

	// The writer: a repository with the hook and nothing else.
	writer, err := initDatabase(outboxIntegrationConfig())
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close() })

	item := &models.Item{Name: "it-crash", Price: 1, Version: 1}
	require.NoError(t, writer.Create(context.Background(), item))

	msg := readEvent(t, socketB)
	assert.Contains(t, msg, `"type":"item.created"`)
	assert.Contains(t, msg, `"name":"it-crash"`)
	expectSilence(t, socketB)
}
