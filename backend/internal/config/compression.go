package config

import (
	"errors"
	"fmt"
)

// Defaults and limits of the response compression.
const (
	// defaultCompressionMinSize is the smallest body (bytes) that is gzipped.
	defaultCompressionMinSize = 1024
	// maxCompressionMinSize caps COMPRESSION_MIN_SIZE: the middleware holds up
	// to that many bytes of a response back per request to decide.
	maxCompressionMinSize = 1 << 20
)

// CompressionConfig holds the settings of the response compression
// (middleware.Compression).
type CompressionConfig struct {
	// MinSize is COMPRESSION_MIN_SIZE (default 1024): the smallest
	// uncompressed body, in bytes, that is compressed. Read leniently while
	// the module is off, and a positive integer of at most 1 MiB while it is
	// on.
	MinSize int
	// Enabled is COMPRESSION_ENABLED (default true, strict boolean). With
	// false no middleware is mounted and responses are unchanged.
	Enabled bool
}

// loadCompressionConfig reads the compression variables. The switch is
// parsed strictly (a typo must not silently change what the server sends).
// The minimum is read leniently while the module is off, because nothing uses
// it, and strictly while it is on.
func loadCompressionConfig() (CompressionConfig, error) {
	enabled, err := parseEnvBoolStrict("COMPRESSION_ENABLED", true)
	if err != nil {
		return CompressionConfig{}, err
	}

	c := CompressionConfig{Enabled: enabled}

	if !enabled {
		c.MinSize = int(getEnvInt32("COMPRESSION_MIN_SIZE", defaultCompressionMinSize))

		return c, nil
	}

	v, err := parseEnvInt32Strict("COMPRESSION_MIN_SIZE", defaultCompressionMinSize)
	if err != nil {
		return CompressionConfig{}, err
	}

	c.MinSize = int(v)

	return c, nil
}

// Validate checks the settings that the module uses. It is called only with
// the module on.
func (c *CompressionConfig) Validate() error {
	if c.MinSize <= 0 {
		return errors.New("COMPRESSION_MIN_SIZE must be positive")
	}

	if c.MinSize > maxCompressionMinSize {
		return fmt.Errorf("COMPRESSION_MIN_SIZE must not be larger than %d", maxCompressionMinSize)
	}

	return nil
}
