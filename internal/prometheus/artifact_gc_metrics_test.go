package prometheus

import (
	"slices"
	"testing"

	clientPrometheus "github.com/prometheus/client_golang/prometheus"
)

func TestArtifactGCMetricsAreRegistered(t *testing.T) {
	t.Parallel()

	ArtifactGCRemovedTotal.WithLabelValues("example.com/owner/registered").Add(0)
	ArtifactGCKept.WithLabelValues("example.com/owner/registered").Set(0)

	metricFamilies, err := clientPrometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	metricNames := make([]string, 0, len(metricFamilies))
	for _, metricFamily := range metricFamilies {
		metricNames = append(metricNames, metricFamily.GetName())
	}

	for _, expectedName := range []string{
		"doco_cd_artifact_gc_removed_total",
		"doco_cd_artifact_gc_kept",
	} {
		if !slices.Contains(metricNames, expectedName) {
			t.Errorf("expected gathered metrics to contain %q", expectedName)
		}
	}
}
