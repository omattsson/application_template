package mail

import (
	"context"
	"io"
	"mime"
	"mime/quotedprintable"
	stdmail "net/mail"
	"strings"
	"testing"
	"time"

	"backend/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testMailConfig(s *fakeServer, tlsMode string) config.MailConfig {
	return config.MailConfig{
		Enabled: true,
		Host:    "127.0.0.1",
		Port:    s.port(),
		TLS:     tlsMode,
		From:    "App <noreply@example.com>",
		Timeout: 3 * time.Second,
	}
}

func testMessage() Message {
	return Message{
		To:      "alice@example.com",
		Subject: "Reset your password",
		Body:    "Hello alice,\r\n\r\nhttps://app.example.com/reset-password#token=abcDEF-_123\r\n\r\nIgnore this if it was not you.\r\n",
	}
}

// parsed is a received message split into headers and decoded body.
func parsed(t *testing.T, r received) (stdmail.Header, string) {
	t.Helper()

	m, err := stdmail.ReadMessage(strings.NewReader(r.data))
	require.NoError(t, err)

	var body io.Reader = m.Body
	if strings.EqualFold(m.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
		body = quotedprintable.NewReader(m.Body)
	}

	text, err := io.ReadAll(body)
	require.NoError(t, err)

	return m.Header, string(text)
}

func TestSMTPSendModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mode      string // fake server mode
		tlsMode   string // SMTP_TLS
		username  string
		wantTLS   bool
		configure func(*fakeServer)
	}{
		{name: "none against a plain server", mode: "plain", tlsMode: config.SMTPTLSNone},
		{name: "starttls", mode: "starttls", tlsMode: config.SMTPTLSStartTLS, wantTLS: true},
		{name: "implicit tls", mode: "tls", tlsMode: config.SMTPTLSImplicit, wantTLS: true},
		{
			name: "starttls with credentials", mode: "starttls", tlsMode: config.SMTPTLSStartTLS, wantTLS: true, username: "mailer",
			configure: func(s *fakeServer) { s.authPw, s.authUser = "s3cret-pw", "mailer" },
		},
		{
			name: "implicit tls with credentials", mode: "tls", tlsMode: config.SMTPTLSImplicit, wantTLS: true, username: "mailer",
			configure: func(s *fakeServer) { s.authPw, s.authUser = "s3cret-pw", "mailer" },
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := startFakeServer(t, tt.mode, tt.configure)
			cfg := testMailConfig(srv, tt.tlsMode)
			cfg.Username = tt.username

			if tt.username != "" {
				cfg.Password = "s3cret-pw"
			}

			m, err := NewSMTP(cfg, WithTLSConfig(srv.clientTLS()))
			require.NoError(t, err)

			require.NoError(t, m.Send(context.Background(), testMessage()))

			got := srv.messages()
			require.Len(t, got, 1)
			assert.Equal(t, tt.wantTLS, got[0].tls, "the connection is encrypted as configured")
			assert.Contains(t, got[0].from, "noreply@example.com")
			assert.Equal(t, []string{"<alice@example.com>"}, got[0].to)
			assert.Equal(t, tt.username, got[0].authUser)

			header, body := parsed(t, got[0])
			assert.Equal(t, "Reset your password", header.Get("Subject"))
			assert.Contains(t, header.Get("From"), "noreply@example.com")
			assert.Contains(t, header.Get("To"), "alice@example.com")
			assert.Equal(t, "auto-generated", header.Get("Auto-Submitted"))

			mediaType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
			require.NoError(t, err)
			assert.Equal(t, "text/plain", mediaType, "plain text only")
			assert.Contains(t, body, "https://app.example.com/reset-password#token=abcDEF-_123", "the link survives the encoding")
		})
	}
}

func TestSMTPRefusesWhatItCannotSendSafely(t *testing.T) {
	t.Parallel()

	t.Run("starttls is required, there is no downgrade", func(t *testing.T) {
		t.Parallel()
		srv := startFakeServer(t, "starttls", func(s *fakeServer) { s.noStartTLS = true })
		m, err := NewSMTP(testMailConfig(srv, config.SMTPTLSStartTLS), WithTLSConfig(srv.clientTLS()))
		require.NoError(t, err)

		err = m.Send(context.Background(), testMessage())
		require.Error(t, err)
		assert.Empty(t, srv.messages(), "nothing is sent in clear text")
	})

	t.Run("a certificate that is not trusted", func(t *testing.T) {
		t.Parallel()
		srv := startFakeServer(t, "tls", nil)
		m, err := NewSMTP(testMailConfig(srv, config.SMTPTLSImplicit)) // system roots
		require.NoError(t, err)

		require.Error(t, m.Send(context.Background(), testMessage()))
		assert.Empty(t, srv.messages())
	})

	t.Run("credentials in clear text are refused when the mailer is built", func(t *testing.T) {
		t.Parallel()
		srv := startFakeServer(t, "plain", nil)
		cfg := testMailConfig(srv, config.SMTPTLSNone)
		cfg.Username, cfg.Password = "mailer", "s3cret-pw"

		_, err := NewSMTP(cfg)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "s3cret-pw")
	})

	t.Run("a rejected login never shows the password", func(t *testing.T) {
		t.Parallel()
		srv := startFakeServer(t, "tls", func(s *fakeServer) { s.rejectAuth = true })
		cfg := testMailConfig(srv, config.SMTPTLSImplicit)
		cfg.Username, cfg.Password = "mailer", "s3cret-pw"
		m, err := NewSMTP(cfg, WithTLSConfig(srv.clientTLS()))
		require.NoError(t, err)

		err = m.Send(context.Background(), testMessage())
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "s3cret-pw")
		assert.Empty(t, srv.messages())
	})

	t.Run("an unusable configuration", func(t *testing.T) {
		t.Parallel()

		_, err := NewSMTP(config.MailConfig{})
		require.Error(t, err)
	})
}

// A header value with a line break is refused before anything is sent.
func TestSMTPHeaderInjectionIsImpossible(t *testing.T) {
	t.Parallel()

	srv := startFakeServer(t, "plain", nil)
	m, err := NewSMTP(testMailConfig(srv, config.SMTPTLSNone))
	require.NoError(t, err)

	tests := []struct {
		name string
		msg  Message
	}{
		{name: "line feed in the subject", msg: Message{To: "a@example.com", Subject: "Hi\nBcc: evil@example.com", Body: "x"}},
		{name: "carriage return in the subject", msg: Message{To: "a@example.com", Subject: "Hi\rBcc: evil@example.com", Body: "x"}},
		{name: "crlf in the subject", msg: Message{To: "a@example.com", Subject: "Hi\r\nBcc: evil@example.com", Body: "x"}},
		{name: "line feed in the recipient", msg: Message{To: "a@example.com\nBcc: evil@example.com", Subject: "Hi", Body: "x"}},
		{name: "crlf in the recipient", msg: Message{To: "a@example.com\r\nBcc: evil@example.com", Subject: "Hi", Body: "x"}},
		{name: "unicode line separator in the subject", msg: Message{To: "a@example.com", Subject: "Hi Bcc: x", Body: "x"}},
		{name: "two recipients", msg: Message{To: "a@example.com, b@example.com", Subject: "Hi", Body: "x"}},
		{name: "recipient with a display name", msg: Message{To: "Alice <a@example.com>", Subject: "Hi", Body: "x"}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := m.Send(context.Background(), tt.msg)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidMessage)
		})
	}

	assert.Zero(t, srv.connections(), "no connection was opened for a refused message")
	assert.Empty(t, srv.messages())
}

func TestSMTPSubjectWithNonASCIIIsEncoded(t *testing.T) {
	t.Parallel()

	srv := startFakeServer(t, "plain", nil)
	m, err := NewSMTP(testMailConfig(srv, config.SMTPTLSNone))
	require.NoError(t, err)

	msg := testMessage()
	msg.Subject = "Återställ lösenord"
	msg.Body = "Hej Åsa, lösenordet är här"
	require.NoError(t, m.Send(context.Background(), msg))

	got := srv.messages()
	require.Len(t, got, 1)

	header, _ := parsed(t, got[0])
	subject, err := new(mime.WordDecoder).DecodeHeader(header.Get("Subject"))
	require.NoError(t, err)
	assert.Equal(t, msg.Subject, subject)
	assert.NotContains(t, header.Get("Subject"), "\n")
}

func TestSMTPTimeoutAndCancel(t *testing.T) {
	t.Parallel()

	t.Run("a server that never greets ends at the timeout", func(t *testing.T) {
		t.Parallel()
		srv := startFakeServer(t, "plain", func(s *fakeServer) { s.stall = true })
		cfg := testMailConfig(srv, config.SMTPTLSNone)
		cfg.Timeout = 300 * time.Millisecond
		m, err := NewSMTP(cfg)
		require.NoError(t, err)

		start := time.Now()
		err = m.Send(context.Background(), testMessage())
		require.Error(t, err)
		assert.Less(t, time.Since(start), 3*time.Second)
	})

	t.Run("a cancelled context ends the send at once", func(t *testing.T) {
		t.Parallel()
		srv := startFakeServer(t, "plain", func(s *fakeServer) { s.stall = true })
		m, err := NewSMTP(testMailConfig(srv, config.SMTPTLSNone)) // timeout 3s
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		err = m.Send(ctx, testMessage())
		require.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(start), 2*time.Second)
	})

	t.Run("an expired context", func(t *testing.T) {
		t.Parallel()
		srv := startFakeServer(t, "plain", func(s *fakeServer) { s.stall = true })
		m, err := NewSMTP(testMailConfig(srv, config.SMTPTLSNone))
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		err = m.Send(ctx, testMessage())
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("a server that is not there", func(t *testing.T) {
		t.Parallel()
		srv := startFakeServer(t, "plain", nil)
		cfg := testMailConfig(srv, config.SMTPTLSNone)
		_ = srv.ln.Close()
		m, err := NewSMTP(cfg)
		require.NoError(t, err)

		require.Error(t, m.Send(context.Background(), testMessage()))
	})
}

func TestSMTPPing(t *testing.T) {
	t.Parallel()

	t.Run("a healthy server", func(t *testing.T) {
		t.Parallel()
		srv := startFakeServer(t, "starttls", nil)
		m, err := NewSMTP(testMailConfig(srv, config.SMTPTLSStartTLS), WithTLSConfig(srv.clientTLS()))
		require.NoError(t, err)

		require.NoError(t, m.Ping(context.Background()))
		assert.Empty(t, srv.messages(), "a ping sends no message")
		assert.Equal(t, 1, srv.connections())
	})

	t.Run("a server without starttls", func(t *testing.T) {
		t.Parallel()
		srv := startFakeServer(t, "starttls", func(s *fakeServer) { s.noStartTLS = true })
		m, err := NewSMTP(testMailConfig(srv, config.SMTPTLSStartTLS), WithTLSConfig(srv.clientTLS()))
		require.NoError(t, err)

		require.Error(t, m.Ping(context.Background()))
	})

	t.Run("a cancelled context", func(t *testing.T) {
		t.Parallel()
		srv := startFakeServer(t, "plain", func(s *fakeServer) { s.stall = true })
		m, err := NewSMTP(testMailConfig(srv, config.SMTPTLSNone))
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		require.ErrorIs(t, m.Ping(ctx), context.DeadlineExceeded)
	})
}

func TestSMTPAddress(t *testing.T) {
	t.Parallel()

	srv := startFakeServer(t, "plain", nil)
	m, err := NewSMTP(testMailConfig(srv, config.SMTPTLSNone))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(m.Address(), "127.0.0.1:"))
}
