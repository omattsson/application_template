package health

import (
	"sync"
	"time"
)

type HealthChecker struct {
	mu           sync.RWMutex
	isReady      bool
	startTime    time.Time
	dependencies map[string]HealthCheck
}

type HealthCheck func() error

type HealthStatus struct {
	Status string                 `json:"status"`
	Uptime string                 `json:"uptime,omitempty"`
	Checks map[string]CheckStatus `json:"checks,omitempty"`
}

type CheckStatus struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

func NewHealthChecker() *HealthChecker {
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

func (h *HealthChecker) CheckLiveness() HealthStatus {
	uptime := time.Since(h.startTime).String()
	return HealthStatus{
		Status: "UP",
		Uptime: uptime,
	}
}

func (h *HealthChecker) CheckReadiness() HealthStatus {
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
		if err := check(); err != nil {
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
