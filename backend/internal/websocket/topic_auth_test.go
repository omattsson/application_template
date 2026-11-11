package websocket

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHub_TopicAuthorizer(t *testing.T) {
	t.Parallel()

	alice := ClientIdentity{UserID: "u1", Role: "user"}
	root := ClientIdentity{UserID: "u2", Role: "admin"}
	onlyAdminsOnAdminTopics := func(id ClientIdentity, topic string) bool {
		return topic != "admin" || id.Role == "admin"
	}

	tests := []struct {
		name      string
		authorize TopicAuthorizer
		id        ClientIdentity
		topic     string
		want      int
	}{
		{name: "no authorizer allows every topic", id: alice, topic: "admin", want: 1},
		{name: "allowed topic", authorize: onlyAdminsOnAdminTopics, id: alice, topic: "items", want: 1},
		{name: "refused topic", authorize: onlyAdminsOnAdminTopics, id: alice, topic: "admin", want: 0},
		{name: "admin topic for an admin", authorize: onlyAdminsOnAdminTopics, id: root, topic: "admin", want: 1},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := NewHub()
			hub.SetTopicAuthorizer(tt.authorize)
			c := newRegistered(t, hub, tt.id, 1)

			hub.Subscribe(c, tt.topic)

			assert.Equal(t, tt.want, hub.TopicSubscriberCount(tt.topic))
			assert.Equal(t, tt.want, topicSubscriberCount(hub, tt.topic))
		})
	}
}

func TestHub_TopicAuthorizerGetsTheIdentityAndCanBeRemoved(t *testing.T) {
	t.Parallel()
	hub := NewHub()
	id := ClientIdentity{UserID: "u1", Role: "devops", TokenID: "jti"}
	c := newRegistered(t, hub, id, 1)

	var mu sync.Mutex
	var seen []ClientIdentity
	hub.SetTopicAuthorizer(func(got ClientIdentity, topic string) bool {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, got)
		return false
	})
	hub.Subscribe(c, "t1")
	assert.Zero(t, hub.TopicSubscriberCount("t1"))
	mu.Lock()
	require.Len(t, seen, 1)
	assert.Equal(t, id, seen[0])
	mu.Unlock()

	hub.SetTopicAuthorizer(nil)
	hub.Subscribe(c, "t1")
	assert.Equal(t, 1, hub.TopicSubscriberCount("t1"))
}

func TestHub_TopicSubscriberCountNilHub(t *testing.T) {
	t.Parallel()
	var hub *Hub
	assert.Zero(t, hub.TopicSubscriberCount("x"))
}
