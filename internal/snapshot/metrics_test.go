package snapshot

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestNewMetricsRegistersAllCollectors(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}
	// Touch every collector so it reports on Gather.
	m.RebuildsTotal.WithLabelValues("pop3", "success").Inc()
	m.RebuildDuration.WithLabelValues("pop3").Observe(0.1)
	m.SnapshotSeq.WithLabelValues("pop3").Set(1)
	m.DirtyGen.WithLabelValues("pop3").Set(1)
	m.NetBoxAPIDuration.WithLabelValues("devices").Observe(0.02)
	m.NetBoxAPIErrors.WithLabelValues("devices", "500").Inc()
	m.SiteStale.WithLabelValues("pop3").Set(0)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"slagworks_snapshot_rebuilds_total":           false,
		"slagworks_snapshot_rebuild_duration_seconds": false,
		"slagworks_snapshot_seq":                      false,
		"slagworks_snapshot_dirty_gen":                false,
		"slagworks_netbox_api_duration_seconds":       false,
		"slagworks_netbox_api_errors_total":           false,
		"slagworks_site_stale":                        false,
	}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("metric %q not registered", name)
		}
	}
}

func TestNewMetricsWorksOnMultipleRegistries(t *testing.T) {
	// Guards against a refactor that turns these into package-level
	// globals; tests would then panic on the second call.
	m1 := NewMetrics(prometheus.NewRegistry())
	m2 := NewMetrics(prometheus.NewRegistry())
	if m1 == m2 {
		t.Fatal("separate registries must yield separate Metrics")
	}
}
