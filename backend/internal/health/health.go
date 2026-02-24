package health

import (
	"context"
	"sync"
	"time"
)

type HealthChecker struct {
	mu           sync.RWMutex
	isReady      bool
	startTime    time.Time
	dependencies map[string]HealthCheck
}

// HealthCheck is a function that checks a dependency's health.
// It receives a context for cancellation and timeout support.
type HealthCheck func(ctx context.Context) error

type HealthStatus struct {
	Status string                 `json:"status"`
	Uptime string                 `json:"uptime,omitempty"`
	Checks map[string]CheckStatus `json:"checks,omitempty"`
}

type CheckStatus struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// New creates a new HealthChecker instance
func New() *HealthChecker {
	return &HealthChecker{
		startTime:    time.Now(),
		dependencies: make(map[string]HealthCheck),
		isReady:      false,
	}
}

func (h *HealthChecker) AddCheck(name string, check HealthCheck) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dependencies[name] = check
}

func (h *HealthChecker) SetReady(ready bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.isReady = ready
}

func (h *HealthChecker) CheckLiveness(_ context.Context) HealthStatus {
	uptime := time.Since(h.startTime).String()
	return HealthStatus{
		Status: "UP",
		Uptime: uptime,
	}
}

func (h *HealthChecker) CheckReadiness(ctx context.Context) HealthStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if !h.isReady {
		return HealthStatus{
			Status: "DOWN",
			Checks: map[string]CheckStatus{
				"ready": {Status: "DOWN", Message: "Service is not ready"},
			},
		}
	}

	status := HealthStatus{
		Status: "UP",
		Checks: make(map[string]CheckStatus),
	}

	for name, check := range h.dependencies {
		if err := check(ctx); err != nil {
			status.Status = "DOWN"
			status.Checks[name] = CheckStatus{
				Status:  "DOWN",
				Message: err.Error(),
			}
		} else {
			status.Checks[name] = CheckStatus{
				Status: "UP",
			}
		}
	}

	return status
}
