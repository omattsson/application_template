package health

import (
	"context"
	"errors"
	"testing"
)

func TestNew(t *testing.T) {
	t.Parallel()
	h := New()
	if h == nil {
		t.Error("Expected non-nil HealthChecker")
	}
}

func TestLivenessCheck(t *testing.T) {
	t.Parallel()
	h := New()
	status := h.CheckLiveness(context.Background())

	if status.Status != "UP" {
		t.Errorf("Expected status UP, got %s", status.Status)
	}

	if status.Uptime == "" {
		t.Error("Expected non-empty uptime")
	}
}

func TestReadinessCheck(t *testing.T) {
	t.Parallel()

	t.Run("Service not ready", func(t *testing.T) {
		t.Parallel()
		h := New() // Create a fresh instance for this subtest
		status := h.CheckReadiness(context.Background())
		if status.Status != "DOWN" {
			t.Errorf("Expected status DOWN, got %s", status.Status)
		}
	})

	t.Run("Service ready", func(t *testing.T) {
		t.Parallel()
		h := New() // Create a fresh instance for this subtest
		h.SetReady(true)
		status := h.CheckReadiness(context.Background())
		if status.Status != "UP" {
			t.Errorf("Expected status UP, got %s", status.Status)
		}
	})
}

func TestHealthChecks(t *testing.T) {
	t.Parallel()

	t.Run("All checks passing", func(t *testing.T) {
		t.Parallel()
		h := New() // Create a fresh instance for this subtest
		h.SetReady(true)
		h.AddCheck("test", func(_ context.Context) error { return nil })
		status := h.CheckReadiness(context.Background())
		if status.Status != "UP" {
			t.Errorf("Expected status UP, got %s", status.Status)
		}
		if status.Checks["test"].Status != "UP" {
			t.Errorf("Expected check status UP, got %s", status.Checks["test"].Status)
		}
	})

	t.Run("Failed check", func(t *testing.T) {
		t.Parallel()
		h := New() // Create a fresh instance for this subtest
		h.SetReady(true)
		h.AddCheck("failing", func(_ context.Context) error { return errors.New("test error") })
		status := h.CheckReadiness(context.Background())
		if status.Status != "DOWN" {
			t.Errorf("Expected status DOWN, got %s", status.Status)
		}
		if status.Checks["failing"].Status != "DOWN" {
			t.Errorf("Expected check status DOWN, got %s", status.Checks["failing"].Status)
		}
	})
}

func TestInformationalChecks(t *testing.T) {
	t.Parallel()

	t.Run("a failing informational check does not turn readiness DOWN", func(t *testing.T) {
		t.Parallel()
		h := New()
		h.SetReady(true)
		h.AddCheck("database", func(_ context.Context) error { return nil })
		h.AddInformationalCheck("mail", func(_ context.Context) error { return errors.New("mail server unavailable") })

		status := h.CheckReadiness(context.Background())
		if status.Status != "UP" {
			t.Errorf("Expected status UP, got %s", status.Status)
		}

		if got := status.Checks["mail"]; got.Status != StatusDegraded || got.Message != "mail server unavailable" {
			t.Errorf("Expected a degraded mail check with its message, got %+v", got)
		}

		if status.Checks["database"].Status != "UP" {
			t.Errorf("Expected the database check UP, got %s", status.Checks["database"].Status)
		}
	})

	t.Run("a passing informational check is UP", func(t *testing.T) {
		t.Parallel()
		h := New()
		h.SetReady(true)
		h.AddInformationalCheck("mail", func(_ context.Context) error { return nil })

		status := h.CheckReadiness(context.Background())
		if status.Status != "UP" || status.Checks["mail"].Status != "UP" {
			t.Errorf("Expected UP and UP, got %+v", status)
		}
	})

	t.Run("a failing gating check still wins next to an informational one", func(t *testing.T) {
		t.Parallel()
		h := New()
		h.SetReady(true)
		h.AddCheck("database", func(_ context.Context) error { return errors.New("down") })
		h.AddInformationalCheck("mail", func(_ context.Context) error { return errors.New("down") })

		status := h.CheckReadiness(context.Background())
		if status.Status != "DOWN" {
			t.Errorf("Expected DOWN, got %s", status.Status)
		}

		if status.Checks["database"].Status != "DOWN" || status.Checks["mail"].Status != StatusDegraded {
			t.Errorf("Unexpected checks %+v", status.Checks)
		}
	})

	t.Run("not ready is DOWN whatever the informational checks say", func(t *testing.T) {
		t.Parallel()
		h := New()
		h.AddInformationalCheck("mail", func(_ context.Context) error { return nil })

		if status := h.CheckReadiness(context.Background()); status.Status != "DOWN" {
			t.Errorf("Expected DOWN, got %s", status.Status)
		}
	})

	t.Run("a name registered twice keeps the last kind", func(t *testing.T) {
		t.Parallel()
		h := New()
		h.SetReady(true)
		h.AddCheck("x", func(_ context.Context) error { return errors.New("down") })
		h.AddInformationalCheck("x", func(_ context.Context) error { return errors.New("down") })

		if status := h.CheckReadiness(context.Background()); status.Status != "UP" {
			t.Errorf("Expected UP, got %s", status.Status)
		}

		h.AddCheck("x", func(_ context.Context) error { return errors.New("down") })

		if status := h.CheckReadiness(context.Background()); status.Status != "DOWN" {
			t.Errorf("Expected DOWN after re-registering as gating, got %s", status.Status)
		}
	})
}
