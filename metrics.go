package metricslite

import (
	"fmt"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// A Counter is a function which increments a metric's value by value when
// invoked. Value must be zero or positive or the Counter will panic. Labels
// enable optional partitioning of a Counter into multiple dimensions.
//
// Counters must be safe for concurrent use. The number of label values passed
// when the Counter is invoked must match the number of label names defined when
// the Counter was created, or the Counter will panic.
type Counter func(value float64, labels ...string)

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
// to create non-const metrics once and pass them through their program as needed.
type Interface interface {
	// Const metrics are used primarily when implementing Prometheus exporters
	// or copying metrics from an external monitoring system. Most applications
	// should use the non-const Counter, Gauge, etc. instead.
	//
	// OnConstScrape invokes the input ScrapeFunc on-demand when the metrics are
	// gathered. OnConstScrape must be called with a ScrapeFunc if const metrics
	// are in use, or the Interface will panic.
	ConstCounter(name, help string, labelNames ...string)
	ConstGauge(name, help string, labelNames ...string)
	OnConstScrape(scrape ScrapeFunc)

	// Non-const (or direct instrumentation) metrics are used to instrument
	// normal Go applications. Their values are only updated when requested
	// by the caller: not on-demand when metrics are gathered.
	Counter(name, help string, labelNames ...string) Counter
	Gauge(name, help string, labelNames ...string) Gauge
}

// A ScrapeFunc is a function which is invoked on demand to collect metric
// labels and values for each const metric present in the map key for metrics.
// The user should invoke the collect function for any values they wish to
// export as metrics. If an error of type *ScrapeError is returned, the error
// will be reported to the metrics system.
//
// A ScrapeFunc must be safe for concurrent use. The number of label values
// passed when collect is invoked must match the number of label names defined
// when the const metric was created, or collect will panic.
type ScrapeFunc func(metrics map[string]func(value float64, labels ...string)) error

var _ error = &ScrapeError{}

// A ScrapeError allows a ScrapeFunc to report a specific metric as the cause
// of a failed metrics scrape.
type ScrapeError struct {
	Metric string
	Err    error
}

// Error implements error.
func (e *ScrapeError) Error() string {
	return fmt.Sprintf("%q: %v", e.Metric, e.Err)
}

// prom implements Interface by wrapping the Prometheus client library.
type prom struct {
	reg    *prometheus.Registry
	mu     sync.RWMutex
	scrape ScrapeFunc
	consts map[string]*promConst
}

var (
	_ Interface            = &prom{}
	_ prometheus.Collector = &prom{}
)

// NewPrometheus creates an Interface which will register all of its metrics
// to the specified Prometheus registry. The registry must not be nil.
func NewPrometheus(reg *prometheus.Registry) Interface {
	p := &prom{
		reg: reg,
		scrape: func(_ map[string]func(float64, ...string)) error {
			panic("metricslite: Interface collected const metrics invoked before calling OnConstScrape")
		},
		consts: make(map[string]*promConst),
	}

	reg.MustRegister(p)
	return p
}

// A promConst stores information about a Prometheus const metric.
type promConst struct {
	desc  *prometheus.Desc
	value prometheus.ValueType
}

// Describe implements prometheus.Collector.
func (p *prom) Describe(ch chan<- *prometheus.Desc) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, c := range p.consts {
		ch <- c.desc
	}
}

// Collect implements prometheus.Collector.
func (p *prom) Collect(ch chan<- prometheus.Metric) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.consts) == 0 {
		return
	}

	metrics := make(map[string]func(float64, ...string), len(p.consts))
	for name, c := range p.consts {
		// Shadow c for each closure.
		c := c
		metrics[name] = func(value float64, labels ...string) {
			ch <- prometheus.MustNewConstMetric(c.desc, c.value, value, labels...)
		}
	}

	err := p.scrape(metrics)
	if err == nil {
		return
	}

	// Scrape failed, try to report more information.
	serr, ok := err.(*ScrapeError)
	if !ok {
		// Cannot report on error!
		return
	}

	c, ok := p.consts[serr.Metric]
	if !ok {
		// Caller reported an invalid metric name. In theory this will trigger
		// an HTTP 500 which will cause Prometheus to fail the scrape, so it
		// is suitable for our purposes.
		panicf("metricslite: *ScrapeError contained non-existent metric %q", serr.Metric)
		return
	}

	ch <- prometheus.NewInvalidMetric(c.desc, serr.Err)
}

// OnConstScrape implements Interface.
func (p *prom) OnConstScrape(scrape ScrapeFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scrape = scrape
}

// ConstCounter implements Interface.
func (p *prom) ConstCounter(name, help string, labelNames ...string) {
	p.constBasic(prometheus.CounterValue, name, help, labelNames)
}

// ConstGauge implements Interface.
func (p *prom) ConstGauge(name, help string, labelNames ...string) {
	p.constBasic(prometheus.GaugeValue, name, help, labelNames)
}

// constBasic registers const metrics with prom for primitive metrics types like
// counters and gauges.
func (p *prom) constBasic(value prometheus.ValueType, name, help string, labelNames []string) {
	switch value {
	case prometheus.CounterValue, prometheus.GaugeValue:
	default:
		panicf("metricslite: invalid prom.constBasic metric type: %T", value)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.consts[name] = &promConst{
		desc:  prometheus.NewDesc(name, help, labelNames, nil),
		value: value,
	}
}

// Counter implements Interface.
func (p *prom) Counter(name, help string, labelNames ...string) Counter {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: name,
		Help: help,
	}, labelNames)

	p.reg.MustRegister(c)

	return func(value float64, labels ...string) {
		// Counters cannot be decremented.
		if value < 0 {
			panic("metricslite: counter must be called with a zero or positive value")
		}

		c.WithLabelValues(labels...).Add(value)
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

// ConstCounter implements Interface.
func (discard) ConstCounter(_, _ string, _ ...string) {}

// ConstGauge implements Interface.
func (discard) ConstGauge(_, _ string, _ ...string) {}

// OnConstScrape implements Interface.
func (discard) OnConstScrape(_ ScrapeFunc) {}

// Counter implements Interface.
func (discard) Counter(_, _ string, _ ...string) Counter {
	return func(value float64, _ ...string) {
		// Counters cannot be decremented.
		if value < 0 {
			panic("metricslite: counter must be called with a zero or positive value")
		}
	}
}

// Gauge implements Interface.
func (discard) Gauge(_, _ string, _ ...string) Gauge { return func(_ float64, _ ...string) {} }

// Memory implements Interface by storing timeseries and samples in memory.
// This type is primarily useful for tests.
type Memory struct {
	mu     sync.Mutex
	series map[string]*series
	scrape ScrapeFunc
	consts map[string][]string
}

// Series produces a copy of all of the timeseries and samples stored by
// the Memory.
func (m *Memory) Series() map[string]Series {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Now that the caller is requesting output, scrape const metrics on demand
	// to generate more timeseries.
	m.scrapeConstLocked()

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
func NewMemory() *Memory {
	return &Memory{
		series: make(map[string]*series),
		scrape: func(_ map[string]func(float64, ...string)) error {
			panic("metricslite: Interface collected const metrics invoked before calling OnConstScrape")
		},
		consts: make(map[string][]string),
	}
}

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

// OnConstScrape implements Interface.
func (m *Memory) OnConstScrape(scrape ScrapeFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scrape = scrape
}

// scrapeConstLocked updates const metrics. m.mu must be locked before calling
// this function.
func (m *Memory) scrapeConstLocked() {
	if len(m.consts) == 0 {
		return
	}

	metrics := make(map[string]func(float64, ...string), len(m.consts))
	for name, labelNames := range m.consts {
		// Shadow key/value for each closure.
		var (
			name       = name
			labelNames = labelNames
		)

		metrics[name] = func(value float64, labels ...string) {
			m.series[name].Samples.Set(sampleKVs(name, labelNames, labels), value)
		}
	}

	err := m.scrape(metrics)
	if err == nil {
		return
	}

	// Scrape failed, try to report more information.
	serr, ok := err.(*ScrapeError)
	if !ok {
		// Cannot report on error!
		return
	}

	if _, ok := m.consts[serr.Metric]; !ok {
		// Caller reported an invalid metric name. Since Memory is more likely
		// to be used in tests, panic to let them know.
		panicf("metricslite: *ScrapeError contained non-existent metric %q", serr.Metric)
	}

	m.series[serr.Metric].Samples.Set("", -1)
}

// ConstCounter implements Interface.
func (m *Memory) ConstCounter(name, help string, labelNames ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Samples map is updated on-demand and unneeded here.
	_ = m.register(name, help, labelNames...)

	m.consts[name] = labelNames
}

// ConstGauge implements Interface.
func (m *Memory) ConstGauge(name, help string, labelNames ...string) {
	// For Memory, these are identical.
	m.ConstCounter(name, help, labelNames...)
}

// Counter implements Interface.
func (m *Memory) Counter(name, help string, labelNames ...string) Counter {
	m.mu.Lock()
	defer m.mu.Unlock()

	samples := m.register(name, help, labelNames...)

	return func(value float64, labels ...string) {
		// Counters cannot be decremented.
		if value < 0 {
			panic("metricslite: counter must be called with a zero or positive value")
		}

		m.mu.Lock()
		defer m.mu.Unlock()

		samples.Add(sampleKVs(name, labelNames, labels), value)
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

// Add adds v to the value stored in k.
func (sm *sampleMap) Add(k string, v float64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.m[k] += v
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
