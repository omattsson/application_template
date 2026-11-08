package main

import (
	"bytes"
	"log"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSetupLoggingTo(t *testing.T) {
	// Not parallel: it replaces the process-wide default slog logger.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	tests := []struct {
		name      string
		level     slog.Level
		wantDebug bool
		wantInfo  bool
		wantWarn  bool
	}{
		{name: "debug level logs everything", level: slog.LevelDebug, wantDebug: true, wantInfo: true, wantWarn: true},
		{name: "info level drops debug", level: slog.LevelInfo, wantInfo: true, wantWarn: true},
		{name: "warn level drops info", level: slog.LevelWarn, wantWarn: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			setupLoggingTo(&buf, tt.level)

			slog.Debug("debug-line")
			slog.Info("info-line")
			slog.Warn("warn-line")

			out := buf.String()
			assert.Equal(t, tt.wantDebug, bytes.Contains([]byte(out), []byte("debug-line")))
			assert.Equal(t, tt.wantInfo, bytes.Contains([]byte(out), []byte("info-line")))
			assert.Equal(t, tt.wantWarn, bytes.Contains([]byte(out), []byte("warn-line")))
		})
	}
}

// TestSetupLoggingToStdLogDoesNotDeadlock checks that a log.Print of a
// library goes through the new default logger without recursing.
func TestSetupLoggingToStdLogDoesNotDeadlock(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	setupLoggingTo(&buf, slog.LevelInfo)

	log.Print("std-log-line")

	assert.Contains(t, buf.String(), "std-log-line")
}
