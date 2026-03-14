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

// CheckLiveness returns a simple UP status. The context parameter is unused
// because liveness is intentionally trivial — it only reports uptime and
// does not call any external dependencies that would need cancellation.
func (h *HealthChecker) CheckLiveness(_ context.Context) HealthStatus {
	uptime := time.Since(h.startTime).String()
	return HealthStatus{
		Status: "UP",
		Uptime: uptime,
	}
}

func (h *HealthChecker) CheckReadiness(ctx context.Context) HealthStatus {
	// Copy state under lock, then release before running I/O-bound checks
	// to avoid holding the mutex during potentially slow dependency calls.
	h.mu.RLock()
	ready := h.isReady
	deps := make(map[string]HealthCheck, len(h.dependencies))
	for k, v := range h.dependencies {
		deps[k] = v
	}
	h.mu.RUnlock()

	if !ready {
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

	for name, check := range deps {
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
