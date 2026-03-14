package websocket

import "encoding/json"

// Message is the envelope for all WebSocket messages sent to clients.
type Message struct {
	// Type identifies the event kind, e.g. "item.created", "item.updated".
	Type string `json:"type"`

	// Payload carries the event-specific data (typically the affected entity).
	Payload json.RawMessage `json:"payload"`
}

// NewMessage creates a Message with the given type and payload.
// The payload is JSON-marshalled; if marshalling fails the payload is set to null.
func NewMessage(msgType string, payload interface{}) Message {
	data, err := json.Marshal(payload)
	if err != nil {
		data = []byte("null")
	}
	return Message{
		Type:    msgType,
		Payload: data,
	}
}

// Bytes serialises the Message to JSON bytes suitable for broadcasting.
func (m Message) Bytes() []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte(`{"type":"error","payload":null}`)
	}
	return b
}
