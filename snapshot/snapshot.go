// Package snapshot is the contract between the local collector and the backend.
// The collector writes raw AWS facts as JSON the engineer can inspect; the
// backend rebuilds an awsx.Live from it (via Live) and runs the same rules it
// runs for a connected role, so pricing logic never ships in this module.
package snapshot

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"github.com/auditaz/auditaz-cli/awsx"
)

// SchemaVersion is bumped on incompatible changes so the backend rejects an old
// file instead of misreading it. v2 made CloudWatch readings awsx.Metric: a v1
// file wrote plain 0 for both "measured idle" and "no data", so its zeros must
// be treated as absent rather than billed as recoverable savings.
const SchemaVersion = 2

// CollectError records a Live call that failed. Collection continues past it,
// and the backend can see what was missed.
type CollectError struct {
	Call    string `json:"call"`
	Message string `json:"message"`
}

// Snapshot is one collection run. Every field mirrors awsx.FakeLive so Live can
// hand it straight back to the rules engine.
type Snapshot struct {
	SchemaVersion    int            `json:"schema_version"`
	CollectorVersion string         `json:"collector_version,omitempty"`
	CollectedAt      string         `json:"collected_at,omitempty"` // RFC3339, stamped by the CLI
	AccountID        string         `json:"account_id,omitempty"`
	Regions          []string       `json:"regions,omitempty"`
	Errors           []CollectError `json:"errors,omitempty"`

	ElasticIPs       []awsx.ElasticIP       `json:"elastic_ips"`
	EBSVolumes       []awsx.Volume          `json:"ebs_volumes"`
	EBSSnapshots     []awsx.Snapshot        `json:"ebs_snapshots"`
	S3Buckets        []awsx.Bucket          `json:"s3_buckets"`
	EC2Instances     []awsx.Instance        `json:"ec2_instances"`
	RDSInstances     []awsx.DBInstance      `json:"rds_instances"`
	RDSSnapshots     []awsx.DBSnapshot      `json:"rds_snapshots"`
	NatGateways      []awsx.NatGateway      `json:"nat_gateways"`
	RouteTables      []awsx.RouteTable      `json:"route_tables"`
	LoadBalancers    []awsx.LoadBalancer    `json:"load_balancers"`
	LogGroups        []awsx.LogGroup        `json:"log_groups"`
	DynamoTables     []awsx.DynamoTable     `json:"dynamodb_tables"`
	RedshiftClusters []awsx.RedshiftCluster `json:"redshift_clusters"`
	HostedZones      []awsx.HostedZone      `json:"hosted_zones"`
	CostLines        []awsx.CostLine        `json:"cost_lines"`
	// MetricGaps travel with the data so the backend marks a report partial
	// instead of presenting unmeasured resources as healthy.
	MetricGaps []awsx.MetricGap `json:"metric_gaps,omitempty"`
}

// Collect reads every Live surface concurrently and never returns an error: a
// failed call is recorded in Errors and the rest still land.
func Collect(ctx context.Context, live awsx.Live) Snapshot {
	s := Snapshot{SchemaVersion: SchemaVersion}
	var mu sync.Mutex
	var wg sync.WaitGroup

	gather(ctx, &wg, &mu, &s, "CostAndUsage", live.CostAndUsage, func(v []awsx.CostLine) { s.CostLines = v })
	gather(ctx, &wg, &mu, &s, "DescribeAddresses", live.DescribeAddresses, func(v []awsx.ElasticIP) { s.ElasticIPs = v })
	gather(ctx, &wg, &mu, &s, "DescribeVolumes", live.DescribeVolumes, func(v []awsx.Volume) { s.EBSVolumes = v })
	gather(ctx, &wg, &mu, &s, "DescribeSnapshots", live.DescribeSnapshots, func(v []awsx.Snapshot) { s.EBSSnapshots = v })
	gather(ctx, &wg, &mu, &s, "ListBuckets", live.ListBuckets, func(v []awsx.Bucket) { s.S3Buckets = v })
	gather(ctx, &wg, &mu, &s, "DescribeInstances", live.DescribeInstances, func(v []awsx.Instance) { s.EC2Instances = v })
	gather(ctx, &wg, &mu, &s, "DescribeDBInstances", live.DescribeDBInstances, func(v []awsx.DBInstance) { s.RDSInstances = v })
	gather(ctx, &wg, &mu, &s, "DescribeDBSnapshots", live.DescribeDBSnapshots, func(v []awsx.DBSnapshot) { s.RDSSnapshots = v })
	gather(ctx, &wg, &mu, &s, "DescribeNatGateways", live.DescribeNatGateways, func(v []awsx.NatGateway) { s.NatGateways = v })
	gather(ctx, &wg, &mu, &s, "DescribeRouteTables", live.DescribeRouteTables, func(v []awsx.RouteTable) { s.RouteTables = v })
	gather(ctx, &wg, &mu, &s, "DescribeLoadBalancers", live.DescribeLoadBalancers, func(v []awsx.LoadBalancer) { s.LoadBalancers = v })
	gather(ctx, &wg, &mu, &s, "DescribeLogGroups", live.DescribeLogGroups, func(v []awsx.LogGroup) { s.LogGroups = v })
	gather(ctx, &wg, &mu, &s, "DescribeDynamoTables", live.DescribeDynamoTables, func(v []awsx.DynamoTable) { s.DynamoTables = v })
	gather(ctx, &wg, &mu, &s, "DescribeRedshiftClusters", live.DescribeRedshiftClusters, func(v []awsx.RedshiftCluster) { s.RedshiftClusters = v })
	gather(ctx, &wg, &mu, &s, "ListHostedZones", live.ListHostedZones, func(v []awsx.HostedZone) { s.HostedZones = v })

	wg.Wait()

	// Only the real *awsx.Client implements these; a FakeLive leaves them empty.
	if r, ok := live.(interface{ ScannedRegions() []string }); ok {
		s.Regions = r.ScannedRegions()
	}
	if a, ok := live.(interface{ AccountID() string }); ok {
		s.AccountID = a.AccountID()
	}
	// Read gaps last: they are only complete once every gather above has run.
	if g, ok := live.(interface{ MetricGaps() []awsx.MetricGap }); ok {
		s.MetricGaps = g.MetricGaps()
	}
	return s
}

// gather runs fn concurrently, storing the result via assign or the failure in
// s.Errors, all under mu.
func gather[T any](
	ctx context.Context,
	wg *sync.WaitGroup,
	mu *sync.Mutex,
	s *Snapshot,
	call string,
	fn func(context.Context) ([]T, error),
	assign func([]T),
) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		items, err := fn(ctx)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			s.Errors = append(s.Errors, CollectError{Call: call, Message: err.Error()})
		}
		// Assigned even on error: dropping what the healthy regions returned
		// would turn a partial read into a confident zero.
		assign(items)
	}()
}

// Live adapts the snapshot back into an awsx.Live, so the backend runs its rules
// with no live AWS access.
func (s Snapshot) Live() awsx.Live {
	return &awsx.FakeLive{
		Gaps:             s.MetricGaps,
		Regions:          s.Regions,
		Account:          s.AccountID,
		Addresses:        s.ElasticIPs,
		Volumes:          s.EBSVolumes,
		Snapshots:        s.EBSSnapshots,
		Buckets:          s.S3Buckets,
		Instances:        s.EC2Instances,
		DBInstances:      s.RDSInstances,
		DBSnapshots:      s.RDSSnapshots,
		NatGateways:      s.NatGateways,
		RouteTables:      s.RouteTables,
		LoadBalancers:    s.LoadBalancers,
		LogGroups:        s.LogGroups,
		DynamoTables:     s.DynamoTables,
		RedshiftClusters: s.RedshiftClusters,
		HostedZones:      s.HostedZones,
		CostLines:        s.CostLines,
	}
}

// Encode writes indented JSON, so it stays inspectable before upload.
func (s Snapshot) Encode(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// Decode rejects an unknown schema version rather than misreading it.
func Decode(r io.Reader) (Snapshot, error) {
	var s Snapshot
	if err := json.NewDecoder(r).Decode(&s); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}
