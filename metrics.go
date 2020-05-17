package metricslite

import (
	"fmt"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// A Counter is a function which increments a metric's value by 1 when invoked.
// Labels enable optional partitioning of a Counter into multiple dimensions.
//
// Counters must be safe for concurrent use. The number of label values passed
// when the Counter is invoked must match the number of label names defined
// when the Counter was created, or the Counter will panic.
type Counter func(labels ...string)

// A Gauge is a function which sets a metric's value when invoked.
// Labels enable optional partitioning of a Gauge into multiple dimensions.
//
// Gauges must be safe for concurrent use. The number of label values passed
// when the Gauge is invoked must match the number of label names defined
// when the Gauge was created, or the Gauge will panic.
type Gauge func(value float64, labels ...string)

// An Interface is a type which can produce metrics functions. An Interface
// implementation must be safe for concurrent use.
//
// If any Interface methods are used to create a metric with the same name as
// a previously created metric, those methods should panic. Callers are expected
// to create metrics once and pass them through their program as needed.
type Interface interface {
	Counter(name, help string, labelNames ...string) Counter
	Gauge(name, help string, labelNames ...string) Gauge
}

// prom implements Interface by wrapping the Prometheus client library.
type prom struct {
	reg *prometheus.Registry
}

var _ Interface = &prom{}

// NewPrometheus creates an Interface which will register all of its metrics
// to the specified Prometheus registry. The registry must not be nil.
func NewPrometheus(reg *prometheus.Registry) Interface {
	return &prom{reg: reg}
}

// Counter implements Interface.
func (p *prom) Counter(name, help string, labelNames ...string) Counter {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: name,
		Help: help,
	}, labelNames)

	p.reg.MustRegister(c)

	return func(labels ...string) {
		c.WithLabelValues(labels...).Inc()
	}
}

// Gauge implements Interface.
func (p *prom) Gauge(name, help string, labelNames ...string) Gauge {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: name,
		Help: help,
	}, labelNames)

	p.reg.MustRegister(g)

	return func(value float64, labels ...string) {
		g.WithLabelValues(labels...).Set(value)
	}
}

// discard implements Interface by discarding all metrics.
type discard struct{}

var _ Interface = discard{}

// Discard creates an Interface which creates no-op metrics that discard their
// data.
func Discard() Interface { return discard{} }

// Counter implements Interface.
func (discard) Counter(_, _ string, _ ...string) Counter { return func(_ ...string) {} }

// Gauge implements Interface.
func (discard) Gauge(_, _ string, _ ...string) Gauge { return func(_ float64, _ ...string) {} }

// Memory implements Interface by storing timeseries and samples in memory.
// This type is primarily useful for tests.
type Memory struct {
	mu     sync.Mutex
	series map[string]*series
}

// Series produces a copy of all of the timeseries and samples stored by
// the Memory.
func (m *Memory) Series() map[string]Series {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]Series, len(m.series))
	for k, v := range m.series {
		out[k] = Series{
			Name:    v.Name,
			Help:    v.Help,
			Samples: v.Samples.Clone(),
		}
	}

	return out
}

// NewMemory creates an initialized Memory.
func NewMemory() *Memory { return &Memory{series: make(map[string]*series)} }

// A Series is a timeseries with metadata and samples partitioned by labels.
type Series struct {
	Name, Help string
	Samples    map[string]float64
}

// A series is a concurrency safe representation of Series for internal use.
type series struct {
	Name, Help string
	Samples    *sampleMap
}

// Counter implements Interface.
func (m *Memory) Counter(name, help string, labelNames ...string) Counter {
	m.mu.Lock()
	defer m.mu.Unlock()

	samples := m.register(name, help, labelNames...)

	return func(labels ...string) {
		m.mu.Lock()
		defer m.mu.Unlock()

		// Counter always increment.
		samples.Inc(sampleKVs(name, labelNames, labels))
	}
}

// Gauge implements Interface.
func (m *Memory) Gauge(name, help string, labelNames ...string) Gauge {
	m.mu.Lock()
	defer m.mu.Unlock()

	samples := m.register(name, help, labelNames...)

	return func(value float64, labels ...string) {
		m.mu.Lock()
		defer m.mu.Unlock()

		// Gauges set an arbitrary value.
		samples.Set(sampleKVs(name, labelNames, labels), value)
	}
}

// register registers a timeseries and produces a samples map for that series.
func (m *Memory) register(name, help string, labelNames ...string) *sampleMap {
	if _, ok := m.series[name]; ok {
		panicf("metricslite: timeseries %q already registered", name)
	}

	samples := &sampleMap{m: make(map[string]float64)}

	m.series[name] = &series{
		Name:    name,
		Help:    help,
		Samples: samples,
	}

	return samples
}

// A sampleMap is a concurrency-safe map of label combinations to samples.
type sampleMap struct {
	mu sync.Mutex
	m  map[string]float64
}

// Clone creates a copy of the underlying map from the sampleMap.
func (sm *sampleMap) Clone() map[string]float64 {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Create an output map that contains all of the data from sm.m.
	samples := make(map[string]float64, len(sm.m))
	for k, v := range sm.m {
		samples[k] = v
	}

	return samples
}

// Inc increments the value of k by 1.
func (sm *sampleMap) Inc(k string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.m[k]++
}

// Set stores k=v in the map.
func (sm *sampleMap) Set(k string, v float64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.m[k] = v
}

// sampleKVs produces a map key for Memory series output.
func sampleKVs(name string, labelNames, labels []string) string {
	// Must have the same argument cardinality.
	if len(labels) != len(labelNames) {
		panicf("metricslite: mismatched label cardinality for timeseries %q", name)
	}

	// Join key/values as "key=value,foo=bar".
	kvs := make([]string, 0, len(labels))
	for i := 0; i < len(labels); i++ {
		kvs = append(kvs, strings.Join([]string{labelNames[i], labels[i]}, "="))
	}

	return strings.Join(kvs, ",")
}

func panicf(format string, a ...interface{}) {
	panic(fmt.Sprintf(format, a...))
}
