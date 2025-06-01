package health

import (
	"errors"
	"testing"
)

func TestNewHealthChecker(t *testing.T) {
	h := NewHealthChecker()
	if h == nil {
		t.Error("Expected non-nil HealthChecker")
	}
}

func TestLivenessCheck(t *testing.T) {
	h := NewHealthChecker()
	status := h.CheckLiveness()

	if status.Status != "UP" {
		t.Errorf("Expected status UP, got %s", status.Status)
	}

	if status.Uptime == "" {
		t.Error("Expected non-empty uptime")
	}
}

func TestReadinessCheck(t *testing.T) {
	h := NewHealthChecker()

	t.Run("Service not ready", func(t *testing.T) {
		status := h.CheckReadiness()
		if status.Status != "DOWN" {
			t.Errorf("Expected status DOWN, got %s", status.Status)
		}
	})

	t.Run("Service ready", func(t *testing.T) {
		h.SetReady(true)
		status := h.CheckReadiness()
		if status.Status != "UP" {
			t.Errorf("Expected status UP, got %s", status.Status)
		}
	})
}

func TestHealthChecks(t *testing.T) {
	h := NewHealthChecker()
	h.SetReady(true)

	t.Run("All checks passing", func(t *testing.T) {
		h.AddCheck("test", func() error { return nil })
		status := h.CheckReadiness()
		if status.Status != "UP" {
			t.Errorf("Expected status UP, got %s", status.Status)
		}
		if status.Checks["test"].Status != "UP" {
			t.Errorf("Expected check status UP, got %s", status.Checks["test"].Status)
		}
	})

	t.Run("Failed check", func(t *testing.T) {
		h.AddCheck("failing", func() error { return errors.New("test error") })
		status := h.CheckReadiness()
		if status.Status != "DOWN" {
			t.Errorf("Expected status DOWN, got %s", status.Status)
		}
		if status.Checks["failing"].Status != "DOWN" {
			t.Errorf("Expected check status DOWN, got %s", status.Checks["failing"].Status)
		}
	})
}
