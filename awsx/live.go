// Package awsx wraps aws-sdk-go-v2 behind a read-only Live interface, backed
// either by an assumed role (backend) or the machine's credential chain (CLI).
// It returns normalized structs rather than SDK types, with stable snake_case
// JSON tags so a snapshot stays human-inspectable before it is uploaded.
package awsx

import "context"

type ElasticIP struct {
	AllocationID string `json:"allocation_id"`
	PublicIP     string `json:"public_ip"`
	Associated   bool   `json:"associated"`  // false => not attached to anything (billable waste)
	InstanceID   string `json:"instance_id"` // set when associated to an EC2 instance (empty otherwise)
	Region       string `json:"region"`
}

type Volume struct {
	VolumeID           string `json:"volume_id"`
	State              string `json:"state"` // "available" means unattached
	SizeGiB            int32  `json:"size_gib"`
	Type               string `json:"type"`       // gp2, gp3, io1, ...
	Iops               int32  `json:"iops"`       // provisioned IOPS (io1/io2/gp3)
	Throughput         int32  `json:"throughput"` // provisioned MiB/s (gp3)
	AttachedInstanceID string `json:"attached_instance_id"`
	Region             string `json:"region"`
	// 30-day I/O totals: an attached volume with zero of both is paid for and
	// unused, waste an "is it attached?" check cannot see. Absent is never idle.
	ReadOps  Metric `json:"read_ops"`
	WriteOps Metric `json:"write_ops"`
}

type Snapshot struct {
	SnapshotID  string `json:"snapshot_id"`
	SizeGiB     int32  `json:"size_gib"`
	AgeDays     int    `json:"age_days"`
	Region      string `json:"region"`
	Description string `json:"description"`
}

type Bucket struct {
	Name               string `json:"name"`
	Region             string `json:"region"`
	HasLifecycle       bool   `json:"has_lifecycle"`
	SizeBytes          int64  `json:"size_bytes"`
	IsCloudFrontOrigin bool   `json:"is_cloudfront_origin"` // serves a live CloudFront distribution (spec §4.2)
	IncompleteMPUBytes int64  `json:"incomplete_mpu_bytes"` // bytes held by multipart uploads left incomplete past the staleness window
	IncompleteMPUCount int    `json:"incomplete_mpu_count"`
	OldestMPUAgeDays   int    `json:"oldest_mpu_age_days"`
}

type Instance struct {
	InstanceID    string `json:"instance_id"`
	InstanceType  string `json:"instance_type"`
	State         string `json:"state"` // "running" | "stopped" | ...
	Region        string `json:"region"`
	AvgCPUPercent Metric `json:"avg_cpu_percent"` // average over the lookback window (running only); absent when CloudWatch had no data
}

type DBSnapshot struct {
	SnapshotID string `json:"snapshot_id"`
	SizeGiB    int32  `json:"size_gib"`
	AgeDays    int    `json:"age_days"`
	Region     string `json:"region"`
}

type DBInstance struct {
	DBInstanceID        string `json:"db_instance_id"`
	Class               string `json:"class"` // e.g. db.r5.large
	Region              string `json:"region"`
	AvgCPUPercent       Metric `json:"avg_cpu_percent"`
	StorageType         string `json:"storage_type"` // gp2, gp3, io1, ...
	AllocatedStorageGiB int32  `json:"allocated_storage_gib"`
}

type NatGateway struct {
	NatGatewayID  string  `json:"nat_gateway_id"`
	Region        string  `json:"region"`
	MonthlyDataGB float64 `json:"monthly_data_gb"`
}

// RouteTable is a normalized VPC route table listing the NAT gateways it routes
// through. Used to spot NAT gateways referenced by no route table (orphans).
type RouteTable struct {
	ID            string   `json:"id"`
	Region        string   `json:"region"`
	NatGatewayIDs []string `json:"nat_gateway_ids"`
}

type LoadBalancer struct {
	ARN              string `json:"arn"`
	Name             string `json:"name"`
	Kind             string `json:"kind"` // "application" | "network"
	Region           string `json:"region"`
	HealthyTargets   int    `json:"healthy_targets"`
	AvgDailyRequests Metric `json:"avg_daily_requests"` // requests/day over 30d (application LBs only; absent for network)
}

type LogGroup struct {
	Name          string `json:"name"`
	Region        string `json:"region"`
	StoredBytes   int64  `json:"stored_bytes"`
	RetentionDays int32  `json:"retention_days"` // 0 => never expires
}

type DynamoTable struct {
	Name           string `json:"name"`
	Region         string `json:"region"`
	BillingMode    string `json:"billing_mode"` // "PROVISIONED" | "PAY_PER_REQUEST"
	ProvisionedRCU int64  `json:"provisioned_rcu"`
	ProvisionedWCU int64  `json:"provisioned_wcu"`
	ConsumedRCU    Metric `json:"consumed_rcu"` // total consumed read units over the lookback window
	ConsumedWCU    Metric `json:"consumed_wcu"` // total consumed write units over the lookback window
	SizeBytes      int64  `json:"size_bytes"`
	TableClass     string `json:"table_class"` // "STANDARD" | "STANDARD_INFREQUENT_ACCESS"
}

// RedshiftCluster is a normalized Redshift cluster with its average database
// connections over the lookback window (the idle signal).
type RedshiftCluster struct {
	ClusterID      string `json:"cluster_id"`
	NodeType       string `json:"node_type"`
	NumberOfNodes  int32  `json:"number_of_nodes"`
	Region         string `json:"region"`
	AvgConnections Metric `json:"avg_connections"`
}

// HostedZone is a normalized Route 53 hosted zone. RecordCount is the total
// resource-record-set count; a brand-new empty zone has exactly 2 (the apex NS
// and SOA), so RecordCount <= 2 means the zone hosts nothing billable.
type HostedZone struct {
	ID          string `json:"id"`   // e.g. Z1234567890ABC (no /hostedzone/ prefix)
	Name        string `json:"name"` // e.g. example.com.
	RecordCount int64  `json:"record_count"`
	Private     bool   `json:"private"`
}

// CostLine is one aggregated cost line from Cost Explorer (last full month),
// grouped by service + usage type — feeds the CSV-style rules in live mode.
type CostLine struct {
	Service   string  `json:"service"`
	UsageType string  `json:"usage_type"`
	AmountUSD float64 `json:"amount_usd"`
}

// Live is the read-only AWS access rules use, implemented by Client and FakeLive.
type Live interface {
	CostAndUsage(ctx context.Context) ([]CostLine, error)
	DescribeAddresses(ctx context.Context) ([]ElasticIP, error)
	DescribeVolumes(ctx context.Context) ([]Volume, error)
	DescribeSnapshots(ctx context.Context) ([]Snapshot, error)
	ListBuckets(ctx context.Context) ([]Bucket, error)
	DescribeInstances(ctx context.Context) ([]Instance, error)
	DescribeDBInstances(ctx context.Context) ([]DBInstance, error)
	DescribeDBSnapshots(ctx context.Context) ([]DBSnapshot, error)
	DescribeNatGateways(ctx context.Context) ([]NatGateway, error)
	DescribeRouteTables(ctx context.Context) ([]RouteTable, error)
	DescribeLoadBalancers(ctx context.Context) ([]LoadBalancer, error)
	DescribeLogGroups(ctx context.Context) ([]LogGroup, error)
	DescribeDynamoTables(ctx context.Context) ([]DynamoTable, error)
	DescribeRedshiftClusters(ctx context.Context) ([]RedshiftCluster, error)
	ListHostedZones(ctx context.Context) ([]HostedZone, error)
}

// FakeLive is a test double for Live, and backs snapshot replay: the backend
// rebuilds one from an upload so the same rules run with no live AWS access.
type FakeLive struct {
	Addresses        []ElasticIP
	Volumes          []Volume
	Snapshots        []Snapshot
	Buckets          []Bucket
	Instances        []Instance
	DBInstances      []DBInstance
	DBSnapshots      []DBSnapshot
	NatGateways      []NatGateway
	RouteTables      []RouteTable
	LoadBalancers    []LoadBalancer
	LogGroups        []LogGroup
	DynamoTables     []DynamoTable
	RedshiftClusters []RedshiftCluster
	HostedZones      []HostedZone
	CostLines        []CostLine
	// Gaps carries unreadable series through a replay, so a reconstructed scan
	// knows its own blind spots.
	Gaps []MetricGap
	// Regions and Account mirror what the real Client found; without them the
	// two scan paths describe the same account differently.
	Regions []string
	Account string
	Err     error
}

func (f *FakeLive) CostAndUsage(context.Context) ([]CostLine, error) {
	return f.CostLines, f.Err
}

func (f *FakeLive) DescribeAddresses(context.Context) ([]ElasticIP, error) {
	return f.Addresses, f.Err
}

func (f *FakeLive) DescribeVolumes(context.Context) ([]Volume, error) {
	return f.Volumes, f.Err
}

func (f *FakeLive) DescribeSnapshots(context.Context) ([]Snapshot, error) {
	return f.Snapshots, f.Err
}

func (f *FakeLive) ListBuckets(context.Context) ([]Bucket, error) {
	return f.Buckets, f.Err
}

func (f *FakeLive) DescribeInstances(context.Context) ([]Instance, error) {
	return f.Instances, f.Err
}

func (f *FakeLive) DescribeDBInstances(context.Context) ([]DBInstance, error) {
	return f.DBInstances, f.Err
}

func (f *FakeLive) DescribeDBSnapshots(context.Context) ([]DBSnapshot, error) {
	return f.DBSnapshots, f.Err
}

func (f *FakeLive) DescribeNatGateways(context.Context) ([]NatGateway, error) {
	return f.NatGateways, f.Err
}

func (f *FakeLive) DescribeRouteTables(context.Context) ([]RouteTable, error) {
	return f.RouteTables, f.Err
}

func (f *FakeLive) DescribeLoadBalancers(context.Context) ([]LoadBalancer, error) {
	return f.LoadBalancers, f.Err
}

func (f *FakeLive) DescribeLogGroups(context.Context) ([]LogGroup, error) {
	return f.LogGroups, f.Err
}

func (f *FakeLive) DescribeDynamoTables(context.Context) ([]DynamoTable, error) {
	return f.DynamoTables, f.Err
}

func (f *FakeLive) DescribeRedshiftClusters(context.Context) ([]RedshiftCluster, error) {
	return f.RedshiftClusters, f.Err
}

func (f *FakeLive) ListHostedZones(context.Context) ([]HostedZone, error) {
	return f.HostedZones, f.Err
}

var _ Live = (*FakeLive)(nil)
