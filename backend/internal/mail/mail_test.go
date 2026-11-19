package mail

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageValidate(t *testing.T) {
	t.Parallel()

	ok := Message{To: "alice@example.com", Subject: "Hello", Body: "Line one\r\nLine two\r\n"}

	tests := []struct {
		mutate  func(*Message)
		name    string
		wantErr bool
	}{
		{name: "valid", mutate: func(*Message) {}},
		{name: "body with line breaks and tabs", mutate: func(m *Message) { m.Body = "a\r\n\tb\n" }},
		{name: "non-ascii subject and body", mutate: func(m *Message) { m.Subject, m.Body = "Återställ", "Hej Åsa" }},
		{name: "empty recipient", mutate: func(m *Message) { m.To = "" }, wantErr: true},
		{name: "blank recipient", mutate: func(m *Message) { m.To = "   " }, wantErr: true},
		{name: "recipient that is not an address", mutate: func(m *Message) { m.To = "alice" }, wantErr: true},
		{name: "recipient too long", mutate: func(m *Message) { m.To = strings.Repeat("a", 250) + "@example.com" }, wantErr: true},
		{name: "empty subject", mutate: func(m *Message) { m.Subject = "" }, wantErr: true},
		{name: "subject too long", mutate: func(m *Message) { m.Subject = strings.Repeat("s", MaxSubjectLen+1) }, wantErr: true},
		{name: "subject with a tab", mutate: func(m *Message) { m.Subject = "a\tb" }, wantErr: true},
		{name: "subject with NUL", mutate: func(m *Message) { m.Subject = "a\x00b" }, wantErr: true},
		{name: "subject with DEL", mutate: func(m *Message) { m.Subject = "a\x7fb" }, wantErr: true},
		{name: "subject with a C1 control", mutate: func(m *Message) { m.Subject = "a\u0085b" }, wantErr: true},
		{name: "subject that is not utf-8", mutate: func(m *Message) { m.Subject = "a\xffb" }, wantErr: true},
		{name: "empty body", mutate: func(m *Message) { m.Body = "" }, wantErr: true},
		{name: "body with NUL", mutate: func(m *Message) { m.Body = "a\x00b" }, wantErr: true},
		{name: "body too long", mutate: func(m *Message) { m.Body = strings.Repeat("b", MaxBodyLen+1) }, wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			msg := ok
			tt.mutate(&msg)

			err := msg.Validate()
			if tt.wantErr {
				assert.ErrorIs(t, err, ErrInvalidMessage)

				return
			}

			assert.NoError(t, err)
		})
	}
}

func TestFakeMailerRecordsAndFails(t *testing.T) {
	t.Parallel()

	f := &FakeMailer{}
	msg := Message{To: "a@example.com", Subject: "s", Body: "b"}

	require.NoError(t, f.Send(context.Background(), msg))
	assert.Equal(t, []Message{msg}, f.Messages())
	assert.Equal(t, 1, f.Calls())

	boom := errors.New("smtp down")
	f.SetError(boom)
	require.ErrorIs(t, f.Send(context.Background(), msg), boom)
	assert.Len(t, f.Messages(), 1, "a failed send is not recorded")
	assert.Equal(t, 2, f.Calls(), "but it is counted")

	f.SetError(nil)
	require.ErrorIs(t, f.Send(context.Background(), Message{To: "a@example.com\nBcc: x", Subject: "s", Body: "b"}), ErrInvalidMessage,
		"the fake validates like the real mailer")

	f.Reset()
	assert.Empty(t, f.Messages())
	assert.Zero(t, f.Calls())
}

func TestFakeMailerDelayHonoursTheContext(t *testing.T) {
	t.Parallel()

	f := &FakeMailer{}
	f.SetDelay(5 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	require.ErrorIs(t, f.Send(ctx, Message{To: "a@example.com", Subject: "s", Body: "b"}), context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second)

	f.SetDelay(10 * time.Millisecond)
	require.NoError(t, f.Send(context.Background(), Message{To: "a@example.com", Subject: "s", Body: "b"}))
}

func TestFakeMailerIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	f := &FakeMailer{}

	var wg sync.WaitGroup

	for range 20 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_ = f.Send(context.Background(), Message{To: "a@example.com", Subject: "s", Body: "b"})
			_ = f.Messages()
		}()
	}

	wg.Wait()
	assert.Len(t, f.Messages(), 20)
}
