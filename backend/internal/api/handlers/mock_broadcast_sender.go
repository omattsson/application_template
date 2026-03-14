package handlers

import "sync"

// MockBroadcastSender is a test double for websocket.BroadcastSender that
// records all messages passed to Broadcast for assertion in unit tests.
type MockBroadcastSender struct {
	mu       sync.Mutex
	messages [][]byte
}

// Broadcast records the message for later inspection.
func (m *MockBroadcastSender) Broadcast(message []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(message))
	copy(cp, message)
	m.messages = append(m.messages, cp)
}

// Messages returns a deep copy of all recorded messages.
func (m *MockBroadcastSender) Messages() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([][]byte, len(m.messages))
	for i, msg := range m.messages {
		cp := make([]byte, len(msg))
		copy(cp, msg)
		result[i] = cp
	}
	return result
}

// Reset clears all recorded messages.
func (m *MockBroadcastSender) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = nil
}
