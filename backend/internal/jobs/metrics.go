package jobs

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"backend/internal/models"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
)

// Outcomes of a finished attempt (the "outcome" attribute of jobs.processed).
const (
	// OutcomeSucceeded is a handler that returned nil.
	OutcomeSucceeded = "succeeded"
	// OutcomeRetried is a failure that was scheduled for another attempt.
	OutcomeRetried = "retried"
	// OutcomeDead is a job that will not run again.
	OutcomeDead = "dead"
	// OutcomeReleased is a job that shutdown interrupted and gave back.
	OutcomeReleased = "released"
)

// Metrics exposes the work of the pool. The only attributes are the job type,
// which is a name registered in code (the pool claims no other), and the
// outcome, a fixed set: no user input reaches a series. The queue depth is
// the snapshot that the pool samples every 30 s; nothing queries the database
// per scrape.
//
//   - jobs.processed: finished attempts, by type and outcome.
//   - jobs.duration: seconds that a handler ran, by type.
//   - jobs.depth: rows per status (the last snapshot).
//   - jobs.oldest_due.age: seconds that the oldest due pending job has
//     waited (the last snapshot, 0 when none is due).
//
// All methods are safe on a nil *Metrics (telemetry off).
type Metrics struct {
	processed metric.Int64Counter
	duration  metric.Float64Histogram
	reg       metric.Registration
	// depth holds the counts in the order of models.JobStatuses.
	depth [4]atomic.Int64
	// ageBits is the float64 bits of the oldest due age in seconds.
	ageBits atomic.Uint64
}

// NewMetrics binds the instruments to the global MeterProvider. With telemetry
// off (the no-op provider) it returns nil, so nothing is allocated; pass the
// result on only when it is not nil, or let the pool take the nil pointer (its
// methods do nothing).
func NewMetrics() *Metrics {
	if _, off := otel.GetMeterProvider().(noopmetric.MeterProvider); off {
		return nil
	}

	m, err := newMetrics(otel.Meter("jobs"))
	if err != nil {
		slog.Warn("Failed to register the job metrics", "error", err)

		return nil
	}

	return m
}

func newMetrics(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{}

	var err error

	m.processed, err = meter.Int64Counter("jobs.processed",
		metric.WithDescription("Finished job attempts by job type and outcome (succeeded, retried, dead, released)."),
		metric.WithUnit("{attempt}"))
	if err != nil {
		return nil, err
	}

	m.duration, err = meter.Float64Histogram("jobs.duration",
		metric.WithDescription("Seconds that a job handler ran, by job type."),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}

	depth, err := meter.Int64ObservableGauge("jobs.depth",
		metric.WithDescription("Jobs per status, from the last sample of the pool (every 30 s)."),
		metric.WithUnit("{job}"))
	if err != nil {
		return nil, err
	}

	age, err := meter.Float64ObservableGauge("jobs.oldest_due.age",
		metric.WithDescription("Seconds that the oldest due pending job has waited, from the last sample of the pool."),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}

	m.reg, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for i, status := range models.JobStatuses {
			o.ObserveInt64(depth, m.depth[i].Load(), metric.WithAttributes(attribute.String("status", status)))
		}

		o.ObserveFloat64(age, math.Float64frombits(m.ageBits.Load()))

		return nil
	}, depth, age)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// Processed counts one finished attempt of a job of type typ.
func (m *Metrics) Processed(typ, outcome string) {
	if m == nil {
		return
	}

	m.processed.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("type", typ), attribute.String("outcome", outcome)))
}

// Duration records how long a handler of type typ ran.
func (m *Metrics) Duration(typ string, d time.Duration) {
	if m == nil {
		return
	}

	m.duration.Record(context.Background(), d.Seconds(), metric.WithAttributes(attribute.String("type", typ)))
}

// ObserveStats stores the snapshot that the gauges report.
func (m *Metrics) ObserveStats(s models.JobStats) {
	if m == nil {
		return
	}

	m.depth[0].Store(s.Pending)
	m.depth[1].Store(s.Running)
	m.depth[2].Store(s.Succeeded)
	m.depth[3].Store(s.Dead)
	m.ageBits.Store(math.Float64bits(s.OldestDueAge.Seconds()))
}

// Close unregisters the gauge callback.
func (m *Metrics) Close() {
	if m == nil || m.reg == nil {
		return
	}

	if err := m.reg.Unregister(); err != nil {
		slog.Debug("Failed to unregister the job metrics", "error", err)
	}
}
