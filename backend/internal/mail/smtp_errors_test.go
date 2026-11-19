package mail

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"backend/internal/config"

	gomail "github.com/wneessen/go-mail"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A server that refuses the recipient and echoes the address in its answer:
// go-mail's SendError text then holds the address twice ("affected
// recipient(s)" and the server line). Neither may survive in an error that the
// mailer returns or in what Describe says about it.
func TestSMTPSendErrorHoldsNoRecipient(t *testing.T) {
	t.Parallel()

	srv := startFakeServer(t, "plain", func(s *fakeServer) {
		s.rcptReply = "550 5.1.1 <%s> no such mailbox for alice@example.com"
	})
	m, err := NewSMTP(testMailConfig(srv, config.SMTPTLSNone))
	require.NoError(t, err)

	err = m.Send(context.Background(), testMessage())
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "alice")
	assert.NotContains(t, err.Error(), "example.com")
	assert.NotContains(t, err.Error(), "no such mailbox", "not the server's text either")

	// The classification survives.
	var de *DeliveryError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "send", de.Stage)
	assert.Equal(t, 550, de.Code)
	assert.False(t, de.Temporary)
	assert.Contains(t, de.Reason, "RCPT TO")

	// The cause is still reachable for a caller that needs it.
	var se *gomail.SendError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, 550, se.ErrorCode())

	// What Describe gives a logger holds no address either.
	var buf bytes.Buffer

	lg := slog.New(slog.NewJSONHandler(&buf, nil))
	lg.Error("Failed to send a password reset mail", Describe(err)...)
	assert.NotContains(t, buf.String(), "alice")
	assert.NotContains(t, buf.String(), "example.com")
	assert.Contains(t, buf.String(), `"smtp_code":550`)
	assert.Contains(t, buf.String(), `"stage":"send"`)
	assert.Contains(t, buf.String(), "RCPT TO")
}

func TestSMTPTemporaryRefusalIsClassified(t *testing.T) {
	t.Parallel()

	srv := startFakeServer(t, "plain", func(s *fakeServer) { s.rcptReply = "451 4.3.0 try later %s" })
	m, err := NewSMTP(testMailConfig(srv, config.SMTPTLSNone))
	require.NoError(t, err)

	err = m.Send(context.Background(), testMessage())

	var de *DeliveryError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, 451, de.Code)
	assert.True(t, de.Temporary)
	assert.NotContains(t, err.Error(), "alice")
}

func TestDescribe(t *testing.T) {
	t.Parallel()

	attrs := func(err error) map[string]any {
		out := map[string]any{}
		a := Describe(err)
		require.Zero(t, len(a)%2)

		for i := 0; i < len(a); i += 2 {
			out[a[i].(string)] = a[i+1]
		}

		return out
	}

	tests := []struct {
		err  error
		want map[string]any
		name string
	}{
		{name: "nil", err: nil, want: map[string]any{}},
		{name: "timeout", err: context.DeadlineExceeded, want: map[string]any{"kind": "timeout"}},
		{name: "cancelled", err: context.Canceled, want: map[string]any{"kind": "canceled"}},
		{name: "invalid message", err: ErrInvalidMessage, want: map[string]any{"kind": "invalid_message"}},
		{
			name: "an unknown error says nothing of its text",
			err:  errors.New("550 alice@example.com refused"),
			want: map[string]any{"kind": "other"},
		},
		{
			name: "a delivery error of the connect stage",
			err:  &DeliveryError{Stage: "connect", cause: errors.New("dial tcp 127.0.0.1:1: connection refused")},
			want: map[string]any{"kind": "other", "stage": "connect"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, attrs(tt.err))
		})
	}
}
