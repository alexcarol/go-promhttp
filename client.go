package promhttp

import (
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	pph "github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
	"strings"
)

// Client embeds original http Client.
// It allow to creates its copy that will be instrumented for given recipient.
type Client struct {
	*http.Client
	Registerer prometheus.Registerer
	Namespace  string
}

// ForRecipient allocates new client based on base one with incomingInstrumentation.
// Given recipient is used as a constant label.
func (c *Client) ForRecipient(recipient string) (*http.Client, error) {
	return instrumentClientWithConstLabels(c.Namespace, c.Client, c.Registerer, map[string]string{
		"recipient": recipient,
	})
}

func instrumentClientWithConstLabels(namespace string, c *http.Client, reg prometheus.Registerer, constLabels map[string]string) (*http.Client, error) {
	i := &outgoingInstrumentation{
		requests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace:   namespace,
				Subsystem:   subsystemHTTPOutgoing,
				Name:        "requests_total",
				Help:        "A counter for outgoing requests from the wrapped client.",
				ConstLabels: constLabels,
			},
			[]string{"code", "method"},
		),
		requestSize: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace:   namespace,
				Subsystem:   subsystemHTTPOutgoing,
				Name:        "request_size_histogram_bytes",
				Help:        "Request size in bytes.",
				Buckets:     []float64{100, 1000, 2000, 5000, 10000},
				ConstLabels: constLabels,
			},
			[]string{"code", "method"},
		),
		responseContentLength: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{

				Namespace:   namespace,
				Subsystem:   subsystemHTTPOutgoing,
				Name:        "response_content_length_histogram",
				Help:        "Response content length in bytes.",
				Buckets:     []float64{100, 1000, 2000, 5000, 10000},
				ConstLabels: constLabels,
			},
			[]string{"code", "method"},
		),
		duration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace:   namespace,
				Subsystem:   subsystemHTTPOutgoing,
				Name:        "request_duration_histogram_seconds",
				Help:        "A histogram of outgoing request latencies.",
				Buckets:     prometheus.DefBuckets,
				ConstLabels: constLabels,
			},
			[]string{"method"},
		),
		dnsDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace:   namespace,
				Subsystem:   subsystemHTTPOutgoing,
				Name:        "dns_duration_histogram_seconds",
				Help:        "Trace dns latency histogram.",
				Buckets:     []float64{.005, .01, .025, .05},
				ConstLabels: constLabels,
			},
			[]string{"event"},
		),
		tlsDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace:   namespace,
				Subsystem:   subsystemHTTPOutgoing,
				Name:        "tls_duration_histogram_seconds",
				Help:        "Trace tls latency histogram.",
				Buckets:     []float64{.05, .1, .25, .5},
				ConstLabels: constLabels,
			},
			[]string{"event"},
		),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace:   namespace,
			Subsystem:   subsystemHTTPOutgoing,
			Name:        "in_flight_requests",
			Help:        "A gauge of in-flight outgoing requests for the wrapped client.",
			ConstLabels: constLabels,
		}),
	}

	trace := &pph.InstrumentTrace{
		DNSStart: func(t float64) {
			i.dnsDuration.WithLabelValues("dns_start").Observe(t)
		},
		DNSDone: func(t float64) {
			i.dnsDuration.WithLabelValues("dns_done").Observe(t)
		},
		TLSHandshakeStart: func(t float64) {
			i.tlsDuration.WithLabelValues("tls_handshake_start").Observe(t)
		},
		TLSHandshakeDone: func(t float64) {
			i.tlsDuration.WithLabelValues("tls_handshake_done").Observe(t)
		},
	}

	transport := c.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &http.Client{
		CheckRedirect: c.CheckRedirect,
		Jar:           c.Jar,
		Timeout:       c.Timeout,
		Transport: pph.InstrumentRoundTripperInFlight(i.inflight,
			pph.InstrumentRoundTripperCounter(i.requests,
				pph.InstrumentRoundTripperTrace(trace,
					instrumentRoundTripperRequestSize(i.requestSize,
						instrumentRoundTripperResponseContentLength(i.responseContentLength,
							pph.InstrumentRoundTripperDuration(i.duration, transport),
						),
					),
				),
			),
		),
	}, reg.Register(i)
}

func instrumentRoundTripperRequestSize(obs prometheus.ObserverVec, next http.RoundTripper) pph.RoundTripperFunc {
	return func(r *http.Request) (*http.Response, error) {
		resp, err := next.RoundTrip(r)
		if err == nil {
			labels := prometheus.Labels{
				"code":   fmt.Sprint(resp.StatusCode),
				"method": strings.ToLower(r.Method),
			}

			obs.With(labels).Observe(float64(computeApproximateRequestSize(r)))
		}
		return resp, err
	}
}

func computeApproximateRequestSize(r *http.Request) int {
	s := 0
	if r.URL != nil {
		s += len(r.URL.String())
	}

	s += len(r.Method)
	s += len(r.Proto)
	for name, values := range r.Header {
		s += len(name)
		for _, value := range values {
			s += len(value)
		}
	}
	s += len(r.Host)

	// N.B. r.Form and r.MultipartForm are assumed to be included in r.URL.

	if r.ContentLength != -1 {
		s += int(r.ContentLength)
	}
	return s
}

func instrumentRoundTripperResponseContentLength(obs prometheus.ObserverVec, next http.RoundTripper) pph.RoundTripperFunc {
	return func(r *http.Request) (*http.Response, error) {
		resp, err := next.RoundTrip(r)
		if err == nil {
			labels := prometheus.Labels{
				"code":   fmt.Sprint(resp.StatusCode),
				"method": strings.ToLower(r.Method),
			}

			obs.With(labels).Observe(float64(resp.ContentLength))
		}
		return resp, err
	}
}

type outgoingInstrumentation struct {
	duration              *prometheus.HistogramVec
	requests              *prometheus.CounterVec
	dnsDuration           *prometheus.HistogramVec
	tlsDuration           *prometheus.HistogramVec
	inflight              prometheus.Gauge
	requestSize           *prometheus.HistogramVec
	responseContentLength *prometheus.HistogramVec
}

// Describe implements prometheus.Collector interface.
func (i *outgoingInstrumentation) Describe(in chan<- *prometheus.Desc) {
	i.duration.Describe(in)
	i.requests.Describe(in)
	i.dnsDuration.Describe(in)
	i.tlsDuration.Describe(in)
	i.inflight.Describe(in)
	i.requestSize.Describe(in)
	i.responseContentLength.Describe(in)
}

// Collect implements prometheus.Collector interface.
func (i *outgoingInstrumentation) Collect(in chan<- prometheus.Metric) {
	i.duration.Collect(in)
	i.requests.Collect(in)
	i.dnsDuration.Collect(in)
	i.tlsDuration.Collect(in)
	i.inflight.Collect(in)
	i.requestSize.Collect(in)
	i.responseContentLength.Collect(in)
}
