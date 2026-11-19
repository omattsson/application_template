package mail

import (
	"context"
	"errors"
	"fmt"

	gomail "github.com/wneessen/go-mail"
)

// DeliveryError is what SMTPMailer returns for a failure after the message
// passed validation. go-mail's own SendError prints the recipient address
// ("affected recipient(s): ...") and the server's answer, which often echoes
// it, so a DeliveryError that wraps one prints only the classification: the
// stage, the reason, the SMTP code and whether the failure is temporary. The
// original error stays reachable through errors.As and Unwrap, for a caller
// that needs it and accepts the risk of printing it.
//
// A failure that is not a SendError (a connect, TLS or authentication error)
// carries no recipient and keeps its text, which tells the operator why.
type DeliveryError struct {
	cause error
	// Stage is where it failed: "connect", "send" or "close".
	Stage string
	// Reason is the go-mail reason of a refused delivery ("sending SMTP RCPT TO
	// command"); empty for a failure that is not a SendError.
	Reason string
	// Code is the SMTP code of the server's refusal (4xx temporary, 5xx
	// permanent); 0 when there is none.
	Code int
	// Temporary reports a 4xx refusal.
	Temporary bool
}

// Error implements error. It never holds the recipient or the server's text
// of a refused delivery.
func (e *DeliveryError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("mail: %s: %v", e.Stage, e.cause)
	}

	text := "mail: " + e.Stage + " refused: " + e.Reason
	if e.Code != 0 {
		text += fmt.Sprintf(" (smtp %d)", e.Code)
	}

	return text
}

// Unwrap returns the original error.
func (e *DeliveryError) Unwrap() error { return e.cause }

// newDeliveryError classifies err for stage. A go-mail SendError is reduced to
// its reason and codes; any other error keeps its text.
func newDeliveryError(stage string, err error) error {
	var se *gomail.SendError
	if errors.As(err, &se) {
		return &DeliveryError{
			Stage:     stage,
			Reason:    se.Reason.String(),
			Code:      se.ErrorCode(),
			Temporary: se.IsTemp(),
			cause:     err,
		}
	}

	return &DeliveryError{Stage: stage, cause: err}
}

// Describe returns slog attributes that classify a mail error and print none
// of its text: the kind ("timeout", "canceled", "invalid_message", "smtp" or
// "other"), and for a DeliveryError the stage, the reason, the SMTP code and
// whether it is temporary. A log line for a failed send uses it instead of the
// error, because the text of an error of an unknown Mailer may hold the
// recipient. It returns nil for a nil error.
func Describe(err error) []any {
	if err == nil {
		return nil
	}

	kind := "other"

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		kind = "timeout"
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	case errors.Is(err, ErrInvalidMessage):
		kind = "invalid_message"
	}

	var de *DeliveryError

	if errors.As(err, &de) {
		attrs := []any{}

		if de.Reason != "" && kind == "other" {
			kind = "smtp"
		}

		attrs = append(attrs, "kind", kind, "stage", de.Stage)

		if de.Reason != "" {
			attrs = append(attrs, "reason", de.Reason, "smtp_code", de.Code, "temporary", de.Temporary)
		}

		return attrs
	}

	return []any{"kind", kind}
}
