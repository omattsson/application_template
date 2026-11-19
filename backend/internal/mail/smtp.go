package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"backend/internal/config"

	gomail "github.com/wneessen/go-mail"
)

// SMTPMailer sends mail through an SMTP server with go-mail. It opens one
// connection per message (a reset mail is rare, and a pooled connection would
// outlive a server that closed it) and is safe for concurrent use.
//
// TLS follows SMTP_TLS: STARTTLS is required (the send fails when the server
// does not offer it, so there is no silent downgrade), implicit TLS wraps the
// connection from the first byte, and none sends in clear text. Credentials are
// used only over TLS: the constructor refuses "none" with a username, and the
// automatic mechanism choice of go-mail does not use PLAIN or LOGIN on a
// connection that is not encrypted.
type SMTPMailer struct {
	tlsConfig *tls.Config
	host      string
	tlsMode   string
	username  string
	password  string
	from      string
	port      int
	timeout   time.Duration
}

var _ Mailer = (*SMTPMailer)(nil)

// SMTPOption changes an SMTPMailer (tests).
type SMTPOption func(*SMTPMailer)

// WithTLSConfig sets the TLS configuration of the connection (a test trusts
// its own certificate with it). The default verifies the server certificate
// against the system roots for the SMTP host, TLS 1.2 or later.
func WithTLSConfig(cfg *tls.Config) SMTPOption {
	return func(m *SMTPMailer) { m.tlsConfig = cfg }
}

// NewSMTP builds the mailer of cfg. It checks what it relies on again (the
// config package validated it at load): a host, a sender address, a known TLS
// mode and no credentials in clear text.
func NewSMTP(cfg config.MailConfig, opts ...SMTPOption) (*SMTPMailer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("mail: %w", err)
	}

	m := &SMTPMailer{
		host:     cfg.Host,
		port:     cfg.Port,
		tlsMode:  cfg.TLS,
		username: cfg.Username,
		password: cfg.Password,
		from:     cfg.From,
		timeout:  cfg.Timeout,
	}

	for _, o := range opts {
		o(m)
	}

	if m.tlsConfig == nil {
		m.tlsConfig = &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	}

	// Build one client now: a bad option combination fails at the start, not at
	// the first mail.
	if _, err := m.newClient(); err != nil {
		return nil, fmt.Errorf("mail: %w", err)
	}

	return m, nil
}

// newClient builds a go-mail client for one connection.
func (m *SMTPMailer) newClient() (*gomail.Client, error) {
	opts := []gomail.Option{
		gomail.WithTLSConfig(m.tlsConfig.Clone()),
		gomail.WithTimeout(m.timeout),
		gomail.WithPort(m.port),
	}

	switch m.tlsMode {
	case config.SMTPTLSImplicit:
		opts = append(opts, gomail.WithSSL())
	case config.SMTPTLSNone:
		opts = append(opts, gomail.WithTLSPolicy(gomail.NoTLS))
	default:
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	}

	if m.username != "" {
		opts = append(opts,
			gomail.WithSMTPAuth(gomail.SMTPAuthAutoDiscover),
			gomail.WithUsername(m.username),
			gomail.WithPassword(m.password),
		)
	}

	return gomail.NewClient(m.host, opts...)
}

// Send builds the message and sends it. Validate runs first, so a header
// value with a line break never reaches the library. A cancelled or expired
// ctx ends the call at once with an error that matches ctx.Err(); the
// connection is closed when the library next looks at it, at the latest after
// the SMTP timeout. A failure after validation is a *DeliveryError: its text
// holds the stage, the reason and the SMTP code, never the recipient or the
// server's answer to a refused delivery, and never the credentials. Log it
// with Describe.
func (m *SMTPMailer) Send(ctx context.Context, msg Message) error {
	if err := msg.Validate(); err != nil {
		return err
	}

	body := gomail.NewMsg()
	if err := body.From(m.from); err != nil {
		return fmt.Errorf("mail: sender address: %w", err)
	}

	if err := body.To(msg.To); err != nil {
		return fmt.Errorf("%w: recipient", ErrInvalidMessage)
	}

	body.Subject(msg.Subject)
	body.SetBodyString(gomail.TypeTextPlain, msg.Body)
	// RFC 3834: an automatic reply and a vacation notice must not answer it.
	body.SetGenHeader("Auto-Submitted", "auto-generated")

	return m.run(ctx, func(ctx context.Context, c *gomail.Client) error {
		return deliver(ctx, c, body)
	})
}

// deliver dials, sends and closes. It closes the connection when ctx ends, so
// a server that stops answering cannot hold the send past the deadline.
func deliver(ctx context.Context, c *gomail.Client, body *gomail.Msg) (err error) {
	client, err := c.DialToSMTPClientWithContext(ctx)
	if err != nil {
		return newDeliveryError("connect", err)
	}

	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()

	defer func() {
		if cerr := c.CloseWithSMTPClient(client); cerr != nil && err == nil && ctx.Err() == nil {
			err = newDeliveryError("close", cerr)
		}
	}()

	if err := c.SendWithSMTPClient(client, body); err != nil {
		return newDeliveryError("send", err)
	}

	return nil
}

// Ping connects to the server, completes the greeting, EHLO, STARTTLS and
// authentication, and quits. It sends no message. It is the probe of the health
// check.
func (m *SMTPMailer) Ping(ctx context.Context) error {
	return m.run(ctx, func(ctx context.Context, c *gomail.Client) error {
		client, err := c.DialToSMTPClientWithContext(ctx)
		if err != nil {
			return newDeliveryError("connect", err)
		}

		stop := context.AfterFunc(ctx, func() { _ = client.Close() })
		defer stop()

		_ = c.CloseWithSMTPClient(client)

		return nil
	})
}

// run builds a client, limits the call to the SMTP timeout and runs fn in a
// goroutine, so that the caller returns when ctx ends even while the library
// waits for the network. The goroutine ends at the latest when the connection
// deadlines of the client pass.
func (m *SMTPMailer) run(ctx context.Context, fn func(context.Context, *gomail.Client) error) error {
	client, err := m.newClient()
	if err != nil {
		return fmt.Errorf("mail: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- fn(ctx, client) }()

	select {
	case err := <-done:
		if err != nil && ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
			return errors.Join(ctx.Err(), err)
		}

		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Address returns host:port of the server (for logs; no credentials).
func (m *SMTPMailer) Address() string {
	return net.JoinHostPort(m.host, strconv.Itoa(m.port))
}
