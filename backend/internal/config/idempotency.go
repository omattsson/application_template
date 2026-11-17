package config

import (
	"errors"
	"time"
)

// Defaults of the idempotency module.
const (
	// defaultIdempotencyTTL is how long a stored response is replayed.
	defaultIdempotencyTTL = 24 * time.Hour
	// defaultIdempotencyLockTimeout is how long an in-progress key blocks a
	// retry before the retry may take it over.
	defaultIdempotencyLockTimeout = 60 * time.Second
)

// IdempotencyConfig holds the settings of the optional idempotency module
// (a POST with an Idempotency-Key header runs once; a repeat replays the
// stored response).
type IdempotencyConfig struct {
	// TTL is IDEMPOTENCY_TTL (default 24h): how long a key and its stored
	// response live. The janitor deletes the rows after it.
	TTL time.Duration
	// LockTimeout is IDEMPOTENCY_LOCK_TIMEOUT (default 60s): how long a key
	// whose first request is still running answers 409 to a retry. After it
	// the next request with the same key and request takes the key over.
	LockTimeout time.Duration
	// Enabled is IDEMPOTENCY_ENABLED (default false, strict boolean). With
	// false no middleware, repository or worker exists.
	Enabled bool
}

// loadIdempotencyConfig reads the idempotency variables. The switch is parsed
// strictly (a typo must not silently run POST without the protection that the
// operator asked for). The durations are read leniently while the module is
// off, because nothing uses them, and strictly while it is on.
func loadIdempotencyConfig() (IdempotencyConfig, error) {
	enabled, err := parseEnvBoolStrict("IDEMPOTENCY_ENABLED", false)
	if err != nil {
		return IdempotencyConfig{}, err
	}

	c := IdempotencyConfig{Enabled: enabled}

	durations := []struct {
		dst      *time.Duration
		key      string
		fallback time.Duration
	}{
		{&c.TTL, "IDEMPOTENCY_TTL", defaultIdempotencyTTL},
		{&c.LockTimeout, "IDEMPOTENCY_LOCK_TIMEOUT", defaultIdempotencyLockTimeout},
	}

	for _, d := range durations {
		if !enabled {
			*d.dst = getEnvDuration(d.key, d.fallback)
			continue
		}

		v, err := parseEnvDurationStrict(d.key, d.fallback)
		if err != nil {
			return IdempotencyConfig{}, err
		}

		*d.dst = v
	}

	return c, nil
}

// Validate checks the settings that the module uses. It is called only with
// the module on.
func (c *IdempotencyConfig) Validate() error {
	if c.TTL <= 0 {
		return errors.New("IDEMPOTENCY_TTL must be positive")
	}

	if c.LockTimeout <= 0 {
		return errors.New("IDEMPOTENCY_LOCK_TIMEOUT must be positive")
	}

	// A row in progress expires with the TTL: a shorter TTL would let a retry
	// take over (and the janitor delete) a key whose request still runs.
	if c.TTL < c.LockTimeout {
		return errors.New("IDEMPOTENCY_TTL must not be shorter than IDEMPOTENCY_LOCK_TIMEOUT")
	}

	return nil
}
