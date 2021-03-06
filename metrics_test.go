package metricslite_test

import (
	"errors"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/mdlayher/metricslite"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func TestPrometheus(t *testing.T) {
	const (
		counterOut = `# HELP bar_total A second counter.
# TYPE bar_total counter
bar_total 5
# HELP foo_total A counter.
# TYPE foo_total counter
foo_total{address="127.0.0.1",interface="eth0"} 1
foo_total{address="2001:db8::1",interface="eth1"} 1
foo_total{address="::1",interface="eth0"} 1
`

		gaugeOut = `# HELP bar_bytes A second gauge.
# TYPE bar_bytes gauge
bar_bytes{device="eth0"} 1024
# HELP foo_celsius A gauge.
# TYPE foo_celsius gauge
foo_celsius{probe="temp0"} 1
foo_celsius{probe="temp1"} 100
`
	)

	tests := []struct {
		name string
		fn   func(m metricslite.Interface)

		// body is used for exact matches. bodyContains supports partial
		// matching. Only one must be set at a time.
		body         string
		bodyContains string
	}{
		{
			name: "noop",
		},
		{
			name: "counters",
			fn:   testCounters,
			body: counterOut,
		},
		{
			name: "const counters",
			fn:   testConstCounters,
			body: counterOut,
		},
		{
			name: "gauges",
			fn:   testGauges,
			body: gaugeOut,
		},
		{
			name: "const gauges",
			fn:   testConstGauges,
			body: gaugeOut,
		},
		{
			name:         "const errors",
			fn:           testConstErrors,
			bodyContains: `error collecting metric Desc{fqName: "errors_present", help: "An error.", constLabels: {}, variableLabels: [type]}: some error`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.body != "" && tt.bodyContains != "" {
				t.Fatalf("both body %q and bodyContains %q were set", tt.body, tt.bodyContains)
			}

			reg := prometheus.NewPedanticRegistry()
			m := metricslite.NewPrometheus(reg)
			h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})

			if tt.fn != nil {
				tt.fn(m)
			}

			s := httptest.NewServer(h)
			defer s.Close()

			u, err := url.Parse(s.URL)
			if err != nil {
				t.Fatalf("failed to parse temporary HTTP server URL: %v", err)
			}

			client := &http.Client{Timeout: 1 * time.Second}
			res, err := client.Get(u.String())
			if err != nil {
				t.Fatalf("failed to perform HTTP request: %v", err)
			}
			defer res.Body.Close()

			// Set a sane upper limit on the number of bytes in the response body.
			const mebibyte = 1 << 20
			body, err := ioutil.ReadAll(io.LimitReader(res.Body, 16*mebibyte))
			if err != nil {
				t.Fatalf("failed to read HTTP response body: %v", err)
			}

			if tt.body != "" {
				// Exact body check.
				if diff := cmp.Diff(tt.body, string(body)); diff != "" {
					t.Fatalf("unexpected HTTP body (-want +got):\n%s", diff)
				}
				return
			}

			// Body contains check.
			if !strings.Contains(string(body), tt.bodyContains) {
				t.Fatalf("failed to find substring in body:\n%s", string(body))
			}
		})
	}
}

func TestMemory(t *testing.T) {
	var (
		counters = map[string]metricslite.Series{
			"bar_total": {
				Name:    "bar_total",
				Help:    "A second counter.",
				Samples: map[string]float64{"": 5},
			},
			"foo_total": {
				Name: "foo_total",
				Help: "A counter.",
				Samples: map[string]float64{
					"address=127.0.0.1,interface=eth0":   1,
					"address=2001:db8::1,interface=eth1": 1,
					"address=::1,interface=eth0":         1,
				},
			},
		}

		gauges = map[string]metricslite.Series{
			"bar_bytes": {
				Name:    "bar_bytes",
				Help:    "A second gauge.",
				Samples: map[string]float64{"device=eth0": 1024},
			},
			"foo_celsius": {
				Name: "foo_celsius",
				Help: "A gauge.",
				Samples: map[string]float64{
					"probe=temp0": 1,
					"probe=temp1": 100,
				},
			},
		}
	)

	tests := []struct {
		name  string
		fn    func(m metricslite.Interface)
		final map[string]metricslite.Series
	}{
		{
			name:  "noop",
			final: map[string]metricslite.Series{},
		},
		{
			name:  "counters",
			fn:    testCounters,
			final: counters,
		},
		{
			name:  "const counters",
			fn:    testConstCounters,
			final: counters,
		},
		{
			name:  "gauges",
			fn:    testGauges,
			final: gauges,
		},
		{
			name:  "const gauges",
			fn:    testConstGauges,
			final: gauges,
		},
		{
			name: "const errors",
			fn:   testConstErrors,
			final: map[string]metricslite.Series{
				"errors_present": {
					Name: "errors_present",
					Help: "An error.",
					Samples: map[string]float64{
						"":               -1,
						"type=permanent": 10,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := metricslite.NewMemory()

			if tt.fn != nil {
				tt.fn(m)
			}

			if diff := cmp.Diff(tt.final, m.Series()); diff != "" {
				t.Fatalf("unexpected timeseries (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMemoryPanics(t *testing.T) {
	// TODO: test other Interface implementations for panics.

	tests := []struct {
		name string
		msg  string
		fn   func(m *metricslite.Memory)
	}{
		{
			name: "already registered",
			msg:  `metricslite: timeseries "foo_total" already registered`,
			fn: func(m *metricslite.Memory) {
				m.Counter("foo_total", "A counter.")
				m.Gauge("foo_total", "A gauge.")
			},
		},
		{
			name: "mismatched cardinality",
			msg:  `metricslite: mismatched label cardinality for timeseries "foo_total"`,
			fn: func(m *metricslite.Memory) {
				c := m.Counter("foo_total", "A counter.")
				c(1.0, "panics")
			},
		},
		{
			name: "counter decrement",
			msg:  `metricslite: counter must be called with a zero or positive value`,
			fn: func(m *metricslite.Memory) {
				c := m.Counter("foo_total", "A counter.")
				c(-1, "panics")
			},
		},
		{
			name: "no for each function",
			msg:  "metricslite: Interface collected const metrics invoked before calling OnConstScrape",
			fn: func(m *metricslite.Memory) {
				m.ConstCounter("foo_total", "A counter.")

				// Force a metrics scrape.
				_ = m.Series()
			},
		},
		{
			name: "invalid scrape error metric",
			msg:  `metricslite: *ScrapeError contained non-existent metric "bar_bytes"`,
			fn: func(m *metricslite.Memory) {
				// Set some metric to ensure ScrapeFunc is invoked.
				m.ConstCounter("foo_total", "A counter.")
				m.OnConstScrape(func(_ map[string]func(float64, ...string)) error {
					return &metricslite.ScrapeError{
						Metric: "bar_bytes",
						Err:    errors.New("some error"),
					}
				})

				// Force a metrics scrape.
				_ = m.Series()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, panics := panics(func() {
				tt.fn(metricslite.NewMemory())
			})
			if !panics {
				t.Fatal("test did not panic")
			}

			if diff := cmp.Diff(tt.msg, msg); diff != "" {
				t.Fatalf("unexpected panic message (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMemoryConcurrent(t *testing.T) {
	// TODO: check concurrency safety of other metricslite.Interface implementations.

	var (
		m = metricslite.NewMemory()
		g = m.Gauge("foo", "", "foo")
	)

	const n = 8

	var wg sync.WaitGroup
	wg.Add(n)
	defer wg.Wait()

	waitC := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-waitC

			for j := 0; j < 1000; j++ {
				g(float64(j), "bar")
				for _, v := range m.Series() {
					_ = v.Samples["foo"]
				}
			}
		}()
	}

	close(waitC)
}

func TestDiscardDoesNotPanic(t *testing.T) {
	tests := []struct {
		name string
		fn   func(m metricslite.Interface)
	}{
		{
			name: "counters",
			fn:   testCounters,
		},
		{
			name: "const counters",
			fn:   testConstCounters,
		},
		{
			name: "gauges",
			fn:   testGauges,
		},
		{
			name: "const gauges",
			fn:   testConstGauges,
		},
		{
			name: "const errors",
			fn:   testConstErrors,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, panics := panics(func() {
				tt.fn(metricslite.Discard())
			})
			if panics {
				t.Fatalf("test panicked: %s", msg)
			}
		})
	}
}

func testCounters(m metricslite.Interface) {
	var (
		c1 = m.Counter("foo_total", "A counter.", "address", "interface")
		c2 = m.Counter("bar_total", "A second counter.")
	)

	// A counter adds values for each call with a given label set.
	c1(1.0, "127.0.0.1", "eth0")
	c1(1.0, "::1", "eth0")
	c1(1.0, "2001:db8::1", "eth1")

	c2(5.0)
}

func testGauges(m metricslite.Interface) {
	var (
		g1 = m.Gauge("foo_celsius", "A gauge.", "probe")
		g2 = m.Gauge("bar_bytes", "A second gauge.", "device")
	)

	// A gauge only stores the last value for a given timeseries.
	g1(0, "temp0")
	g1(100, "temp1")
	g1(1, "temp0")

	g2(1024, "eth0")
}

func testConstCounters(m metricslite.Interface) {
	const (
		fooTotal = "foo_total"
		barTotal = "bar_total"
	)

	m.ConstCounter(fooTotal, "A counter.", "address", "interface")
	m.ConstCounter(barTotal, "A second counter.")

	m.OnConstScrape(func(metrics map[string]func(float64, ...string)) error {
		for m, collect := range metrics {
			switch m {
			case fooTotal:
				collect(1, "127.0.0.1", "eth0")
				collect(1, "::1", "eth0")
				collect(1, "2001:db8::1", "eth1")
			case barTotal:
				collect(5)
			}
		}

		return nil
	})
}

func testConstGauges(m metricslite.Interface) {
	const (
		fooCelsius = "foo_celsius"
		barBytes   = "bar_bytes"
	)

	m.ConstGauge(fooCelsius, "A gauge.", "probe")
	m.ConstGauge(barBytes, "A second gauge.", "device")

	m.OnConstScrape(func(metrics map[string]func(float64, ...string)) error {
		for m, collect := range metrics {
			switch m {
			case fooCelsius:
				collect(100, "temp1")
				collect(1, "temp0")
			case barBytes:
				collect(1024, "eth0")
			}
		}

		return nil
	})
}

func testConstErrors(m metricslite.Interface) {
	const errorsPresent = "errors_present"
	m.ConstGauge(errorsPresent, "An error.", "type")

	m.OnConstScrape(func(metrics map[string]func(float64, ...string)) error {
		for m, collect := range metrics {
			switch m {
			case errorsPresent:
				for i, t := range []string{"permanent", "temporary"} {
					// First scrape succeeds, second returns an error.
					if i == 0 {
						collect(10, t)
						continue
					}

					return &metricslite.ScrapeError{
						Metric: m,
						Err:    errors.New("some error"),
					}
				}
			}
		}

		return nil
	})
}

func panics(fn func()) (msg string, panics bool) {
	defer func() {
		if r := recover(); r != nil {
			// On panic, capture the output string and return that fn did
			// panic.
			msg = r.(string)
			panics = true
		}
	}()

	fn()

	// fn did not panic.
	return "", false
}
