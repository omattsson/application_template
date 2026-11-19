// Package mail sends plain text mail. Mailer is the interface that the
// optional password reset module uses; SMTPMailer is the implementation on
// go-mail and FakeMailer records messages for tests.
package mail

import (
	"context"
	"errors"
	"fmt"
	stdmail "net/mail"
	"strings"
	"unicode/utf8"
)

// Limits of a message. They keep a header inside the line limit of RFC 5322
// and the body small.
const (
	// MaxAddressLen is the longest recipient address (RFC 5321 path limit).
	MaxAddressLen = 254
	// MaxSubjectLen is the longest subject, in bytes.
	MaxSubjectLen = 255
	// MaxBodyLen is the longest body, in bytes.
	MaxBodyLen = 64 << 10
)

// ErrInvalidMessage is returned (wrapped) by Send for a message that cannot
// be sent. No message is built from it: nothing leaves the process.
var ErrInvalidMessage = errors.New("invalid mail message")

// Message is one plain text mail to one recipient.
type Message struct {
	// To is one bare address (no display name, no list).
	To string
	// Subject is one line.
	Subject string
	// Body is plain text.
	Body string
}

// Mailer sends one message. An implementation honours ctx (a cancelled or
// expired context ends the call with an error that matches it), is safe for
// concurrent use and never logs a credential.
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// Validate checks the message. A header value must not hold a line break or
// any other control character: that would let a value add a header (header
// injection). The body may have line breaks but no NUL.
func (m Message) Validate() error {
	to := strings.TrimSpace(m.To)

	switch {
	case to == "" || len(to) > MaxAddressLen:
		return fmt.Errorf("%w: recipient", ErrInvalidMessage)
	case hasControl(to) || hasControl(m.Subject):
		return fmt.Errorf("%w: control character in a header value", ErrInvalidMessage)
	case m.Subject == "" || len(m.Subject) > MaxSubjectLen || !utf8.ValidString(m.Subject):
		return fmt.Errorf("%w: subject", ErrInvalidMessage)
	case m.Body == "" || len(m.Body) > MaxBodyLen || !utf8.ValidString(m.Body) || strings.ContainsRune(m.Body, 0):
		return fmt.Errorf("%w: body", ErrInvalidMessage)
	}

	addr, err := stdmail.ParseAddress(to)
	if err != nil || addr.Address != to || addr.Name != "" {
		return fmt.Errorf("%w: recipient", ErrInvalidMessage)
	}

	return nil
}

// hasControl reports whether s holds a control character (C0, DEL or C1, which
// includes CR, LF and NUL) or a Unicode line separator.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) || r == ' ' || r == ' ' {
			return true
		}
	}

	return false
}
