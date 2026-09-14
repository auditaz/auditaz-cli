package awsx

import "sync"

// MetricGap counts resources whose CloudWatch series could not be read, per
// namespace/metric pair. Absent readings are already safe (see metric.go), but
// safe is not visible: without the gap, a degraded scan looks like a clean
// account instead of saying "we could not measure 47 instances".
type MetricGap struct {
	Namespace string `json:"namespace"` // e.g. "AWS/EC2"
	Metric    string `json:"metric"`    // e.g. "CPUUtilization"
	Region    string `json:"region,omitempty"`
	Resources int    `json:"resources"` // how many resources lacked this series
}

type metricGaps struct {
	mu sync.Mutex
	by map[MetricGap]int
}

// record notes one unreadable series. Region is carried so the report can say a
// whole region went dark rather than blaming the account.
func (g *metricGaps) record(namespace, metric, region string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.by == nil {
		g.by = map[MetricGap]int{}
	}
	g.by[MetricGap{Namespace: namespace, Metric: metric, Region: region}]++
}

// list returns the accumulated gaps in a stable order.
func (g *metricGaps) list() []MetricGap {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.by) == 0 {
		return nil
	}
	out := make([]MetricGap, 0, len(g.by))
	for k, n := range g.by {
		k.Resources = n
		out = append(out, k)
	}
	sortMetricGaps(out)
	return out
}

// sortMetricGaps keeps two renders of the same scan identical.
func sortMetricGaps(gaps []MetricGap) {
	for i := 1; i < len(gaps); i++ {
		for j := i; j > 0 && lessMetricGap(gaps[j], gaps[j-1]); j-- {
			gaps[j], gaps[j-1] = gaps[j-1], gaps[j]
		}
	}
}

func lessMetricGap(a, b MetricGap) bool {
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.Metric != b.Metric {
		return a.Metric < b.Metric
	}
	return a.Region < b.Region
}

// MetricGaps reports the series this scan could not read, so a report can be
// marked partial instead of presenting missing data as an absence of problems.
func (c *Client) MetricGaps() []MetricGap { return c.gaps.list() }

// MetricGaps keeps a replayed scan as honest about its blind spots as a live one.
func (f *FakeLive) MetricGaps() []MetricGap { return f.Gaps }

// ScannedRegions makes both scan paths compute region coverage identically.
func (f *FakeLive) ScannedRegions() []string { return f.Regions }

func (f *FakeLive) AccountID() string { return f.Account }
