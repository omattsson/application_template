//go:build integration

package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDatabaseIdempotencyConcurrentPosts is criterion 9 of the idempotency
// module on real MySQL with the real Items handler: requests that carry one
// Idempotency-Key and arrive at the same moment (a barrier releases them
// together) create one row in idempotency_keys and one item. Every answer is
// the one 201, a replay of it, or a 409 that asks the client to retry.
func TestDatabaseIdempotencyConcurrentPosts(t *testing.T) {
	// Not parallel: buildRouter sets the process-wide Gin mode.
	t.Setenv("GIN_MODE", gin.TestMode)

	cfg := bootstrapTestConfig()
	cfg.Database = integrationDBConfig()
	cfg.Idempotency = config.IdempotencyConfig{Enabled: true, TTL: time.Hour, LockTimeout: time.Minute}

	repo, err := initDatabase(cfg) // MySQL, with the migrations
	require.NoError(t, err)
	s, router, err := buildServices(cfg, repo)
	require.NoError(t, err)
	srv := startHTTPServer(cfg, router)
	s.servers = append(s.servers, srv)
	base := "http://" + srv.Addr
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		assert.NoError(t, shutdown(ctx, s))
	})
	db := repo.(gormRepository).DB()

	const rounds, clients = 5, 16
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: clients}}
	t.Cleanup(client.CloseIdleConnections)

	for round := 0; round < rounds; round++ {
		key := "it-" + uuid.NewString()
		name := "it-idem-" + uuid.NewString()[:8]
		body := `{"name":"` + name + `","price":1}`
		t.Cleanup(func() {
			db.Exec("DELETE FROM idempotency_keys WHERE idempotency_key = ?", key)
			db.Exec("DELETE FROM items WHERE name = ?", name)
		})

		type result struct {
			body     string
			replayed string
			status   int
		}
		results := make([]result, clients)
		var ready, done sync.WaitGroup
		barrier := make(chan struct{})
		for i := 0; i < clients; i++ {
			ready.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/api/v1/items", strings.NewReader(body))
				if err != nil {
					return
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Idempotency-Key", key)
				ready.Done()
				<-barrier
				resp, err := client.Do(req)
				if err != nil {
					return
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				results[i] = result{status: resp.StatusCode, body: string(b), replayed: resp.Header.Get("Idempotency-Replayed")}
			}()
		}
		ready.Wait()
		close(barrier)
		done.Wait()

		var created, replayed, conflict int
		var createdBody string
		for _, r := range results {
			switch {
			case r.status == http.StatusCreated && r.replayed == "":
				created++
				createdBody = r.body
			case r.status == http.StatusCreated && r.replayed == "true":
				replayed++
			case r.status == http.StatusConflict:
				conflict++
			default:
				t.Errorf("round %d: unexpected answer %d %q (replayed=%q)", round, r.status, r.body, r.replayed)
			}
		}
		t.Logf("round %d: created=%d replayed=%d conflict=%d", round, created, replayed, conflict)
		require.Equal(t, 1, created, "round %d: exactly one request ran the handler", round)
		assert.Equal(t, clients-1, replayed+conflict)
		for _, r := range results {
			if r.replayed == "true" {
				assert.Equal(t, createdBody, r.body, "a replay is the stored response")
			}
		}

		var rows, items int64
		require.NoError(t, db.Model(&models.IdempotencyKey{}).Where("idempotency_key = ?", key).Count(&rows).Error)
		require.NoError(t, db.Model(&models.Item{}).Where("name = ?", name).Count(&items).Error)
		assert.Equal(t, int64(1), rows, "round %d: one row for the key", round)
		assert.Equal(t, int64(1), items, "round %d: one item", round)
	}
}
