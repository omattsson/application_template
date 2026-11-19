package config

import (
	"errors"
	"time"
)

// Defaults and limits of the outbox module.
const (
	// defaultOutboxPollInterval is how often each replica reads the outbox.
	defaultOutboxPollInterval = 500 * time.Millisecond
	// defaultOutboxRetention is how long a delivered event row stays.
	defaultOutboxRetention = time.Hour
	// minOutboxRetention is the shortest retention: a row must outlive a
	// replica that is paused, restarted or slow for a while.
	minOutboxRetention = time.Minute
	// outboxRetentionPollFactor is how many poll intervals a row must live.
	outboxRetentionPollFactor = 10
)

// OutboxConfig holds the settings of the optional outbox module (the Items
// WebSocket events are written to the outbox_events table in the transaction
// of the change, and every replica polls the table and delivers the events
// to its own sockets).
type OutboxConfig struct {
	// PollInterval is OUTBOX_POLL_INTERVAL (default 500ms): how often each
	// replica reads the new rows.
	PollInterval time.Duration
	// Retention is OUTBOX_RETENTION (default 1h): the janitor deletes the
	// rows older than this.
	Retention time.Duration
	// Enabled is OUTBOX_ENABLED (default false, strict boolean). With false
	// the Items handler broadcasts directly and no table, poller or worker
	// is used.
	Enabled bool
}

// loadOutboxConfig reads the outbox variables. The switch is parsed strictly
// (a typo must not silently drop the delivery guarantee that the operator
// asked for). The durations are read leniently while the module is off,
// because nothing uses them, and strictly while it is on.
func loadOutboxConfig() (OutboxConfig, error) {
	enabled, err := parseEnvBoolStrict("OUTBOX_ENABLED", false)
	if err != nil {
		return OutboxConfig{}, err
	}

	c := OutboxConfig{Enabled: enabled}

	durations := []struct {
		dst      *time.Duration
		key      string
		fallback time.Duration
	}{
		{&c.PollInterval, "OUTBOX_POLL_INTERVAL", defaultOutboxPollInterval},
		{&c.Retention, "OUTBOX_RETENTION", defaultOutboxRetention},
	}

	for _, d := range durations {
		if !enabled {
			*d.dst = getEnvDuration(d.key, d.fallback)
			continue
		}

		v, err := parseEnvDurationStrict(d.key, d.fallback)
		if err != nil {
			return OutboxConfig{}, err
		}

		*d.dst = v
	}

	return c, nil
}

// Validate checks the settings that the module uses. It is called only with
// the module on.
func (c *OutboxConfig) Validate() error {
	if c.PollInterval <= 0 {
		return errors.New("OUTBOX_POLL_INTERVAL must be positive")
	}

	if c.Retention <= 0 {
		return errors.New("OUTBOX_RETENTION must be positive")
	}

	if c.Retention < minOutboxRetention {
		return errors.New("OUTBOX_RETENTION must be at least 1m")
	}

	// A row that the janitor deletes before a slow poller reads it is a lost
	// event.
	if c.Retention < outboxRetentionPollFactor*c.PollInterval {
		return errors.New("OUTBOX_RETENTION must be at least 10 times OUTBOX_POLL_INTERVAL")
	}

	return nil
}
