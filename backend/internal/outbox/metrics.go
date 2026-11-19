package outbox

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
)

// Metrics exposes the delivery of the poller (Observer): a counter of the
// delivered events and two gauges for the lag. No instrument has a label, so
// no user input can reach a series.
//
//   - outbox.events.delivered: events sent to the hub of this replica.
//   - outbox.delivery.age: seconds between the commit of the newest delivered
//     event and its delivery, by the database clock. It keeps the value of the
//     last delivery while the outbox is idle.
//   - outbox.cursor.lag: ids between the cursor and the table maximum,
//     measured at the end of every poll, also a failed one (a poll whose
//     query failed keeps the last value).
type Metrics struct {
	delivered metric.Int64Counter
	reg       metric.Registration
	// ageBits is the float64 bits of the age in seconds.
	ageBits atomic.Uint64
	lag     atomic.Int64
}

// NewMetrics binds the instruments to the global MeterProvider. With telemetry
// off (the no-op provider) it returns nil, so nothing is allocated per poll;
// pass the result as Config.Observer only when it is not nil (a nil *Metrics in
// the interface would not be nil).
func NewMetrics() *Metrics {
	if _, off := otel.GetMeterProvider().(noopmetric.MeterProvider); off {
		return nil
	}
	m, err := newMetrics(otel.Meter("outbox"))
	if err != nil {
		slog.Warn("Failed to register the outbox metrics", "error", err)
		return nil
	}
	return m
}

func newMetrics(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{}
	var err error
	m.delivered, err = meter.Int64Counter("outbox.events.delivered",
		metric.WithDescription("Outbox events sent to the WebSocket hub of this replica."),
		metric.WithUnit("{event}"))
	if err != nil {
		return nil, err
	}
	age, err := meter.Float64ObservableGauge("outbox.delivery.age",
		metric.WithDescription("Seconds between the commit of the newest delivered outbox event and its delivery, by the database clock."),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	lag, err := meter.Int64ObservableGauge("outbox.cursor.lag",
		metric.WithDescription("Outbox ids between the cursor of this replica and the table maximum."),
		metric.WithUnit("{id}"))
	if err != nil {
		return nil, err
	}
	m.reg, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveFloat64(age, math.Float64frombits(m.ageBits.Load()))
		o.ObserveInt64(lag, m.lag.Load())
		return nil
	}, age, lag)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Observe implements Observer.
func (m *Metrics) Observe(s Stats) {
	if m == nil {
		return
	}
	if s.Delivered > 0 {
		m.delivered.Add(context.Background(), int64(s.Delivered))
	}
	if s.HasAge {
		m.ageBits.Store(math.Float64bits(s.Age.Seconds()))
	}
	if s.HasLag {
		m.lag.Store(int64(min(s.IDLag, math.MaxInt64)))
	}
}

// Close unregisters the gauge callback.
func (m *Metrics) Close() {
	if m == nil || m.reg == nil {
		return
	}
	if err := m.reg.Unregister(); err != nil {
		slog.Debug("Failed to unregister the outbox metrics", "error", err)
	}
}
