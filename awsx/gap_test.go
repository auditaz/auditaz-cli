package awsx

import (
	"sync"
	"testing"
)

func TestMetricGapsAggregateByNamespaceMetricAndRegion(t *testing.T) {
	var g metricGaps
	g.record("AWS/EC2", "CPUUtilization", "sa-east-1")
	g.record("AWS/EC2", "CPUUtilization", "sa-east-1")
	g.record("AWS/EC2", "CPUUtilization", "us-east-1")
	g.record("AWS/RDS", "CPUUtilization", "sa-east-1")

	got := g.list()
	if len(got) != 3 {
		t.Fatalf("gaps = %+v, want 3 distinct buckets", got)
	}
	want := []MetricGap{
		{Namespace: "AWS/EC2", Metric: "CPUUtilization", Region: "sa-east-1", Resources: 2},
		{Namespace: "AWS/EC2", Metric: "CPUUtilization", Region: "us-east-1", Resources: 1},
		{Namespace: "AWS/RDS", Metric: "CPUUtilization", Region: "sa-east-1", Resources: 1},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("gap[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestMetricGapsEmptyIsNil(t *testing.T) {
	var g metricGaps
	if got := g.list(); got != nil {
		t.Errorf("list() = %+v, want nil for a scan with no gaps", got)
	}
}

// Regions are scanned concurrently, so recording must be safe under -race.
func TestMetricGapsConcurrentRecord(t *testing.T) {
	var g metricGaps
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.record("AWS/EC2", "CPUUtilization", "sa-east-1")
		}()
	}
	wg.Wait()
	got := g.list()
	if len(got) != 1 || got[0].Resources != 50 {
		t.Errorf("gaps = %+v, want a single bucket counting 50", got)
	}
}
