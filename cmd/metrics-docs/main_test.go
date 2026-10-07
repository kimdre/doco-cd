package main

import (
	"strings"
	"testing"
)

const collectorFixture = `package metrics

import p "github.com/prometheus/client_golang/prometheus"

var (
	QueueDepth = p.NewGaugeVec(p.GaugeOpts{
		Namespace: MetricsNamespace,
		Name:      "queue_depth",
		Help:      "Current queue depth",
	}, []string{"repository", "queue"})
	Operations = p.NewCounterVec(p.CounterOpts{
		Namespace: "custom",
		Name:      "operations_total",
		Help:      "Completed operations | by repository",
	}, []string{"repository"})
	RequestDuration = p.NewHistogramVec(p.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "request_duration_seconds",
		Help:      "Request duration",
	}, []string{})
	Events = p.NewCounter(p.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "events_total",
		Help:      "Total events",
	})
)
`

func TestParseCollectors(t *testing.T) {
	got, err := parseCollectors("collectors.go", []byte(collectorFixture), "doco_cd")
	if err != nil {
		t.Fatalf("parseCollectors() error = %v", err)
	}

	want := []metric{
		{name: "doco_cd_queue_depth", kind: "Gauge", help: "Current queue depth", labels: []string{"repository", "queue"}},
		{name: "custom_operations_total", kind: "Counter", help: "Completed operations | by repository", labels: []string{"repository"}},
		{name: "doco_cd_request_duration_seconds", kind: "Histogram", help: "Request duration", labels: []string{}},
		{name: "doco_cd_events_total", kind: "Counter", help: "Total events"},
	}
	if len(got) != len(want) {
		t.Fatalf("parseCollectors() returned %d metrics, want %d", len(got), len(want))
	}

	for index := range want {
		if got[index].name != want[index].name ||
			got[index].kind != want[index].kind ||
			got[index].help != want[index].help ||
			!sameStrings(got[index].labels, want[index].labels) {
			t.Errorf("metric %d = %#v, want %#v", index, got[index], want[index])
		}
	}
}

func TestParseCollectorsRejectsUnsupportedConstructor(t *testing.T) {
	source := `package metrics
import "github.com/prometheus/client_golang/prometheus"
var Unsupported = prometheus.NewSummaryVec(prometheus.SummaryOpts{
	Namespace: MetricsNamespace,
	Name:      "unsupported",
	Help:      "Unsupported",
}, []string{"repository"})
`

	_, err := parseCollectors("collectors.go", []byte(source), "doco_cd")
	if err == nil || !strings.Contains(err.Error(), `unsupported Prometheus collector constructor "NewSummaryVec"`) {
		t.Fatalf("parseCollectors() error = %v, want unsupported constructor error", err)
	}
}

func TestRenderMarkdownEscapesTableContent(t *testing.T) {
	got := renderMarkdown([]metric{
		{name: "doco_cd_events_total", kind: "Counter", help: "Events | by type", labels: []string{"event_type"}},
		{name: "doco_cd_source_gc_evicted_total", kind: "Counter", help: "Evicted sources"},
	})

	want := "| Metric name | Type | Description | Labels |\n" +
		"| --- | --- | --- | --- |\n" +
		"| `doco_cd_events_total` | Counter | Events \\| by type | `event_type` |\n" +
		"| `doco_cd_source_gc_evicted_total` | Counter | Evicted sources | None |\n"
	if got != want {
		t.Fatalf("renderMarkdown() =\n%s\nwant:\n%s", got, want)
	}
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}

	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}

	return true
}
