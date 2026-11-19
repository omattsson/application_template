package outbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"backend/internal/websocket"

	gorilla "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A backlog bigger than the hub queue (256) must reach a real socket in full
// and in order: the poller feeds the hub at the pace of its Run loop.
func TestPollerDeliversABacklogLargerThanTheHubQueue(t *testing.T) {
	t.Parallel()
	const backlog = 700 // well over the 256 slots of the broadcast queue

	hub := websocket.NewHub()
	go hub.Run()
	t.Cleanup(hub.Shutdown)

	upgrader := gorilla.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if _, err := websocket.NewClient(hub, conn); err != nil {
			t.Errorf("register the client: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	conn, resp, err := gorilla.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	require.Eventually(t, func() bool { return hub.ClientCount() == 1 }, 2*time.Second, 5*time.Millisecond)

	store := newFakeStore()
	p := NewPoller(Config{Repo: store, Sender: hub, Interval: time.Hour})
	require.NoError(t, p.Poll(context.Background())) // the cursor starts at 0
	ids := make([]uint64, backlog)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	store.commit(ids...)

	received := make(chan []string, 1)
	go func() {
		var got []string
		_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
		for len(got) < backlog {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			got = append(got, string(msg))
		}
		received <- got
	}()

	require.NoError(t, p.Poll(context.Background()))

	got := <-received
	require.Len(t, got, backlog, "an event was lost between the poller and the socket")
	for i, msg := range got {
		assert.Equal(t, fmt.Sprintf(`{"type":"item.created","payload":{"id":%d}}`, i+1), msg, "in id order")
	}
}
