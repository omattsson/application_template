package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Defaults of the diagnostics servers.
const (
	// DefaultMetricsAddr is METRICS_ADDR: the Prometheus metrics port. It is
	// not the API port, and it has no authentication: keep it on a private
	// network.
	DefaultMetricsAddr = ":9090"
	// DefaultPprofAddr is PPROF_ADDR: loopback only, because heap and
	// goroutine dumps expose secrets.
	DefaultPprofAddr = "127.0.0.1:6060"
)

// IsLoopbackAddr reports whether a host:port listen address binds the
// loopback interface only: "localhost" or a loopback IP. An empty host (all
// interfaces), an unspecified IP (0.0.0.0, ::), any other name or address and
// a string that is not host:port are not loopback.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}

	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// validateListenAddr checks that addr is host:port with a numeric port from 0
// to 65535. name is the variable, for the error.
func validateListenAddr(name, addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s must be host:port (for example 127.0.0.1:6060 or :9090)", name)
	}

	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%s: the port must be a number from 0 to 65535", name)
	}

	return nil
}

// loadPprofConfig reads the PPROF_* variables into c. pprof is off by default
// and nothing else is read while it is off. With it on, an invalid value stops
// the start, and so does an address that is not loopback unless
// PPROF_ALLOW_NON_LOOPBACK=true: a heap or goroutine dump exposes secrets
// (tokens, keys, request bodies), so the safe default has no way to open it by
// a typo.
func loadPprofConfig(c *ServerConfig) error {
	enabled, err := parseEnvBoolStrict("PPROF_ENABLED", false)
	if err != nil {
		return err
	}

	c.PprofEnabled = enabled
	c.PprofAddr = strings.TrimSpace(getEnv("PPROF_ADDR", DefaultPprofAddr))

	if !enabled {
		return nil
	}

	if err := validateListenAddr("PPROF_ADDR", c.PprofAddr); err != nil {
		return err
	}

	if c.PprofAllowNonLoopback, err = parseEnvBoolStrict("PPROF_ALLOW_NON_LOOPBACK", false); err != nil {
		return err
	}

	if !IsLoopbackAddr(c.PprofAddr) && !c.PprofAllowNonLoopback {
		return fmt.Errorf("PPROF_ADDR %q is not a loopback address: pprof exposes heap and goroutine dumps; "+
			"bind 127.0.0.1 (the default), or set PPROF_ALLOW_NON_LOOPBACK=true if the port is on a private network only", c.PprofAddr)
	}

	if c.PprofMutexFraction, err = parseNonNegativeEnvInt("PPROF_MUTEX_FRACTION"); err != nil {
		return err
	}

	if c.PprofBlockRate, err = parseNonNegativeEnvInt("PPROF_BLOCK_RATE"); err != nil {
		return err
	}

	return nil
}

// parseNonNegativeEnvInt reads an integer of 0 or more from the environment.
// An unset or empty variable gives 0 (off). The error names the variable and
// does not repeat the value.
func parseNonNegativeEnvInt(key string) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return 0, nil
	}

	v, err := strconv.Atoi(value)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%s must be a whole number of 0 or more", key)
	}

	return v, nil
}
