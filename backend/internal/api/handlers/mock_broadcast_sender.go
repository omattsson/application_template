package handlers

import "sync"

// TargetedMessage is a message recorded by BroadcastToTopic or BroadcastToUser
// together with its topic or user ID.
type TargetedMessage struct {
	Target  string
	Message []byte
}

// MockBroadcastSender is a test double for websocket.BroadcastSender,
// websocket.TopicSender and websocket.UserSender. It records all messages for
// assertion in unit tests.
type MockBroadcastSender struct {
	mu       sync.Mutex
	messages [][]byte
	topic    []TargetedMessage
	user     []TargetedMessage
}

// Broadcast records the message for later inspection.
func (m *MockBroadcastSender) Broadcast(message []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, copyBytes(message))
}

// BroadcastToTopic records the message and its topic.
func (m *MockBroadcastSender) BroadcastToTopic(topic string, message []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.topic = append(m.topic, TargetedMessage{Target: topic, Message: copyBytes(message)})
}

// BroadcastToUser records the message and its user ID.
func (m *MockBroadcastSender) BroadcastToUser(userID string, message []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.user = append(m.user, TargetedMessage{Target: userID, Message: copyBytes(message)})
}

// Messages returns a deep copy of all recorded broadcast messages.
func (m *MockBroadcastSender) Messages() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([][]byte, len(m.messages))
	for i, msg := range m.messages {
		result[i] = copyBytes(msg)
	}
	return result
}

// TopicMessages returns a deep copy of all recorded topic messages.
func (m *MockBroadcastSender) TopicMessages() []TargetedMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return copyTargeted(m.topic)
}

// UserMessages returns a deep copy of all recorded user messages.
func (m *MockBroadcastSender) UserMessages() []TargetedMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return copyTargeted(m.user)
}

// Reset clears all recorded messages.
func (m *MockBroadcastSender) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = nil
	m.topic = nil
	m.user = nil
}

func copyBytes(b []byte) []byte {
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}

func copyTargeted(in []TargetedMessage) []TargetedMessage {
	out := make([]TargetedMessage, len(in))
	for i, tm := range in {
		out[i] = TargetedMessage{Target: tm.Target, Message: copyBytes(tm.Message)}
	}
	return out
}
