package awsx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/redshift"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"golang.org/x/sync/errgroup"
)

// ErrAccessRevoked signals the role can no longer be assumed (e.g. the customer
// deleted the CloudFormation stack / role, or removed the trust). It is a
// terminal auth failure — the connection must be re-onboarded, not retried.
var ErrAccessRevoked = errors.New("awsx: role can no longer be assumed")

// ErrCredentialsUnusable is the local counterpart of ErrAccessRevoked: the
// machine's own credentials were rejected (expired SSO, revoked keys). Kept
// separate because the CLI assumes no role — telling its user a role can't be
// assumed sends them looking for something that doesn't exist.
var ErrCredentialsUnusable = errors.New("awsx: local AWS credentials are expired or invalid")

// isAuthError reports whether err is a terminal auth failure rather than a
// transient one, so callers stop instead of silently scanning an unreadable account.
func isAuthError(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "AccessDenied", "AccessDeniedException", "NoSuchEntity", "NoSuchEntityException",
		"InvalidClientTokenId", "UnrecognizedClientException", "ExpiredToken",
		"ExpiredTokenException", "Unauthorized", "SignatureDoesNotMatch":
		return true
	}
	return false
}

const (
	maxRetries       = 5
	regionParallel   = 8 // concurrent regions per describe call
	ceEndpointRegion = "us-east-1"
	// defaultBootstrapRegion is the last resort when neither the caller nor the
	// environment names a region. Discovery starts here; the scan still covers
	// every enabled region.
	defaultBootstrapRegion = "us-east-1"
)

// baseConfig loads an aws.Config from the default credential chain — the CLI's
// only credential source, never assuming a role. A non-empty profile selects a
// named profile from the shared config; empty means the default chain.
func baseConfig(ctx context.Context, profile, region string) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
		config.WithRetryMaxAttempts(maxRetries),
	}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("awsx: load base config: %w", err)
	}
	return cfg, nil
}

// assumedConfig builds an aws.Config whose credentials come from assuming
// roleARN with the mandatory external id (spec §3.1).
func assumedConfig(ctx context.Context, roleARN, externalID, region string) (aws.Config, error) {
	base, err := baseConfig(ctx, "", region)
	if err != nil {
		return aws.Config{}, err
	}
	provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(base), roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.ExternalID = aws.String(externalID)
	})
	base.Credentials = aws.NewCredentialsCache(provider)
	return base, nil
}

// Validate assumes the role and makes one cheap read-only call to confirm the
// connection works before scans are scheduled (spec §2.2). A terminal auth
// failure is wrapped as ErrAccessRevoked so callers can prompt a reconnect.
func Validate(ctx context.Context, roleARN, externalID, region string) (string, error) {
	cfg, err := assumedConfig(ctx, roleARN, externalID, region)
	if err != nil {
		return "", err
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		if isAuthError(err) {
			return "", fmt.Errorf("%w: %v", ErrAccessRevoked, err)
		}
		return "", fmt.Errorf("awsx: assume-role validation failed: %w", err)
	}
	return aws.ToString(out.Account), nil
}

// simulateBatchSize keeps each SimulatePrincipalPolicy request well within
// limits and isolates a bad action name.
const simulateBatchSize = 25

// Coverage uses iam:SimulatePrincipalPolicy to report which of the given actions
// the deployed role does NOT allow; an empty (non-nil) slice means full coverage.
// It detects drift precisely: a role deployed before a permission was added
// reports that action missing, regardless of any version stamp.
func Coverage(ctx context.Context, roleARN, externalID, region string, actions []string) ([]string, error) {
	if len(actions) == 0 {
		return []string{}, nil
	}
	cfg, err := assumedConfig(ctx, roleARN, externalID, region)
	if err != nil {
		return nil, err
	}
	client := iam.NewFromConfig(cfg)
	missing := []string{}
	for start := 0; start < len(actions); start += simulateBatchSize {
		end := start + simulateBatchSize
		if end > len(actions) {
			end = len(actions)
		}
		in := &iam.SimulatePrincipalPolicyInput{
			PolicySourceArn: aws.String(roleARN),
			ActionNames:     actions[start:end],
			ResourceArns:    []string{"*"},
		}
		p := iam.NewSimulatePrincipalPolicyPaginator(client, in)
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("awsx: simulate principal policy: %w", err)
			}
			for _, res := range page.EvaluationResults {
				if res.EvalDecision != iamtypes.PolicyEvaluationDecisionTypeAllowed {
					missing = append(missing, aws.ToString(res.EvalActionName))
				}
			}
		}
	}
	return missing, nil
}

// memo caches a slice for one scan, so callers needing the same data don't
// each re-hit the API.
type memo[T any] struct {
	once sync.Once
	val  []T
	err  error
}

func (m *memo[T]) do(fn func() ([]T, error)) ([]T, error) {
	m.once.Do(func() { m.val, m.err = fn() })
	return m.val, m.err
}

// Client is the SDK-backed Live implementation. It scans every enabled region
// and memoizes each describe call per scan.
type Client struct {
	roleARN     string
	externalID  string
	bootstrap   string
	accountID   string // discovered from GetCallerIdentity during init
	regions     []string
	cfgByRegion map[string]aws.Config
	// configFn is the only difference between an assume-role Client and a
	// local-credentials one; the scan path is identical.
	configFn func(ctx context.Context, region string) (aws.Config, error)

	mAddresses memo[ElasticIP]
	mVolumes   memo[Volume]
	mSnapshots memo[Snapshot]
	mInstances memo[Instance]
	mDBs       memo[DBInstance]
	mDBSnaps   memo[DBSnapshot]
	mNats      memo[NatGateway]
	mRoutes    memo[RouteTable]
	mRedshift  memo[RedshiftCluster]
	mLBs       memo[LoadBalancer]
	mLogs      memo[LogGroup]
	mDynamo    memo[DynamoTable]
	mBuckets   memo[Bucket]
	mCost      memo[CostLine]

	// gaps records unreadable series, so silence isn't read as a healthy account.
	gaps   metricGaps
	mZones memo[HostedZone]
}

// NewClient assumes the customer role and prepares a config per enabled region.
// Used by the backend's monthly monitor.
func NewClient(ctx context.Context, roleARN, externalID, bootstrapRegion string) (*Client, error) {
	c := &Client{roleARN: roleARN, externalID: externalID, cfgByRegion: map[string]aws.Config{}}
	c.configFn = func(ctx context.Context, region string) (aws.Config, error) {
		return assumedConfig(ctx, roleARN, externalID, region)
	}
	return c.bootstrapInit(ctx, bootstrapRegion, "assume-role preflight", ErrAccessRevoked)
}

// NewLocalClient builds a Client from the machine's own credential chain, with
// no role assumed — the CLI's entry point, so no third party ever gets access.
// profile pins a named profile; empty uses the default chain (honoring AWS_PROFILE).
func NewLocalClient(ctx context.Context, profile, bootstrapRegion string) (*Client, error) {
	c := &Client{cfgByRegion: map[string]aws.Config{}}
	c.configFn = func(ctx context.Context, region string) (aws.Config, error) {
		return baseConfig(ctx, profile, region)
	}
	return c.bootstrapInit(ctx, bootstrapRegion, "credential preflight", ErrCredentialsUnusable)
}

// bootstrapInit is the shared setup for both constructors: bootstrap config,
// caller-identity preflight, region discovery, per-region configs.
//
// The preflight runs BEFORE scanning because broken credentials would otherwise
// fail every describe call and get swallowed by the per-region resilience
// below, yielding an empty "success" scan.
func (c *Client) bootstrapInit(ctx context.Context, bootstrapRegion, preflightLabel string, terminalAuthErr error) (*Client, error) {
	// An empty region defers to the SDK (AWS_REGION, profile, IMDS); hardcoding
	// one here would override a caller pointing at another partition.
	bootCfg, err := c.config(ctx, bootstrapRegion)
	if err != nil {
		return nil, err
	}
	if bootstrapRegion == "" {
		delete(c.cfgByRegion, "")
		bootstrapRegion = bootCfg.Region
		if bootstrapRegion == "" {
			bootstrapRegion = defaultBootstrapRegion
			if bootCfg, err = c.config(ctx, bootstrapRegion); err != nil {
				return nil, err
			}
		} else {
			c.cfgByRegion[bootstrapRegion] = bootCfg
		}
	}
	c.bootstrap = bootstrapRegion
	ident, err := sts.NewFromConfig(bootCfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		if isAuthError(err) {
			return nil, fmt.Errorf("%w: %v", terminalAuthErr, err)
		}
		return nil, fmt.Errorf("awsx: %s failed: %w", preflightLabel, err)
	}
	c.accountID = aws.ToString(ident.Account)

	regions, err := enabledRegions(ctx, bootCfg)
	if err != nil || len(regions) == 0 {
		// Credentials work (the preflight passed) but lack ec2:DescribeRegions.
		regions = []string{bootstrapRegion}
	}
	c.regions = regions
	for _, r := range regions {
		if _, err := c.config(ctx, r); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Client) ScannedRegions() []string { return c.regions }

func (c *Client) AccountID() string { return c.accountID }

func (c *Client) config(ctx context.Context, region string) (aws.Config, error) {
	if cfg, ok := c.cfgByRegion[region]; ok {
		return cfg, nil
	}
	cfg, err := c.configFn(ctx, region)
	if err != nil {
		return aws.Config{}, err
	}
	c.cfgByRegion[region] = cfg
	return cfg, nil
}

func enabledRegions(ctx context.Context, cfg aws.Config) ([]string, error) {
	out, err := ec2.NewFromConfig(cfg).DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	if err != nil {
		return nil, fmt.Errorf("awsx: describe regions: %w", err)
	}
	var regions []string
	for _, r := range out.Regions {
		if s := aws.ToString(r.OptInStatus); s == "not-opted-in" {
			continue
		}
		regions = append(regions, aws.ToString(r.RegionName))
	}
	return regions, nil
}

// mapRegions runs fn for every region concurrently.
//
// A failing region costs only its own results. Deliberately NOT
// errgroup.WithContext, which would cancel siblings that were answering fine,
// and the partial slice is returned alongside the error rather than discarded:
// a false zero reads as "nothing to optimize" instead of "we couldn't look".
func mapRegions[T any](c *Client, ctx context.Context, fn func(ctx context.Context, region string, cfg aws.Config) ([]T, error)) ([]T, error) {
	var (
		mu   sync.Mutex
		out  []T
		errs []error
	)
	g := new(errgroup.Group)
	g.SetLimit(regionParallel)
	for _, r := range c.regions {
		r := r
		cfg := c.cfgByRegion[r]
		g.Go(func() error {
			items, err := fn(ctx, r, cfg)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return nil // never abort the siblings
			}
			out = append(out, items...)
			return nil
		})
	}
	_ = g.Wait() // every goroutine returns nil; failures are collected in errs
	return out, errors.Join(errs...)
}

// --- Cost Explorer ----------------------------------------------------------

// CostAndUsage returns last full month's unblended cost by service and usage
// type. Cost Explorer is global, via the us-east-1 endpoint.
func (c *Client) CostAndUsage(ctx context.Context) ([]CostLine, error) {
	return c.mCost.do(func() ([]CostLine, error) {
		cfg, err := c.config(ctx, ceEndpointRegion)
		if err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		firstThis := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		firstLast := firstThis.AddDate(0, -1, 0)

		out, err := costexplorer.NewFromConfig(cfg).GetCostAndUsage(ctx, &costexplorer.GetCostAndUsageInput{
			TimePeriod:  &cetypes.DateInterval{Start: aws.String(firstLast.Format("2006-01-02")), End: aws.String(firstThis.Format("2006-01-02"))},
			Granularity: cetypes.GranularityMonthly,
			Metrics:     []string{"UnblendedCost"},
			GroupBy: []cetypes.GroupDefinition{
				{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String("SERVICE")},
				{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String("USAGE_TYPE")},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("awsx: cost and usage: %w", err)
		}
		var lines []CostLine
		for _, byTime := range out.ResultsByTime {
			for _, g := range byTime.Groups {
				if len(g.Keys) < 2 {
					continue
				}
				amt := 0.0
				if m, ok := g.Metrics["UnblendedCost"]; ok && m.Amount != nil {
					fmt.Sscanf(aws.ToString(m.Amount), "%f", &amt)
				}
				lines = append(lines, CostLine{Service: g.Keys[0], UsageType: g.Keys[1], AmountUSD: amt})
			}
		}
		return lines, nil
	})
}

// --- EC2 / EBS --------------------------------------------------------------

func (c *Client) DescribeAddresses(ctx context.Context) ([]ElasticIP, error) {
	return c.mAddresses.do(func() ([]ElasticIP, error) {
		return mapRegions(c, ctx, func(ctx context.Context, r string, cfg aws.Config) ([]ElasticIP, error) {
			resp, err := ec2.NewFromConfig(cfg).DescribeAddresses(ctx, &ec2.DescribeAddressesInput{})
			if err != nil {
				return nil, fmt.Errorf("awsx: describe addresses in %s: %w", r, err)
			}
			var out []ElasticIP
			for _, a := range resp.Addresses {
				out = append(out, ElasticIP{AllocationID: aws.ToString(a.AllocationId), PublicIP: aws.ToString(a.PublicIp), Associated: a.AssociationId != nil, InstanceID: aws.ToString(a.InstanceId), Region: r})
			}
			return out, nil
		})
	})
}

func (c *Client) DescribeVolumes(ctx context.Context) ([]Volume, error) {
	return c.mVolumes.do(func() ([]Volume, error) {
		return mapRegions(c, ctx, func(ctx context.Context, r string, cfg aws.Config) ([]Volume, error) {
			var out []Volume
			cw := cloudwatch.NewFromConfig(cfg)
			p := ec2.NewDescribeVolumesPaginator(ec2.NewFromConfig(cfg), &ec2.DescribeVolumesInput{})
			for p.HasMorePages() {
				resp, err := p.NextPage(ctx)
				if err != nil {
					return nil, fmt.Errorf("awsx: describe volumes in %s: %w", r, err)
				}
				for _, v := range resp.Volumes {
					attached := ""
					if len(v.Attachments) > 0 {
						attached = aws.ToString(v.Attachments[0].InstanceId)
					}
					id := aws.ToString(v.VolumeId)
					// A detached volume publishes no series; reading that as
					// "no I/O" double-reports what the orphan rule owns.
					var readOps, writeOps Metric
					if attached != "" {
						readOps = c.sumMetric(ctx, cw, "AWS/EBS", r, "VolumeId", id, "VolumeReadOps")
						writeOps = c.sumMetric(ctx, cw, "AWS/EBS", r, "VolumeId", id, "VolumeWriteOps")
					}
					out = append(out, Volume{VolumeID: id, State: string(v.State), SizeGiB: aws.ToInt32(v.Size), Type: string(v.VolumeType), Iops: aws.ToInt32(v.Iops), Throughput: aws.ToInt32(v.Throughput), AttachedInstanceID: attached, Region: r, ReadOps: readOps, WriteOps: writeOps})
				}
			}
			return out, nil
		})
	})
}

func (c *Client) DescribeSnapshots(ctx context.Context) ([]Snapshot, error) {
	return c.mSnapshots.do(func() ([]Snapshot, error) {
		now := time.Now()
		return mapRegions(c, ctx, func(ctx context.Context, r string, cfg aws.Config) ([]Snapshot, error) {
			var out []Snapshot
			p := ec2.NewDescribeSnapshotsPaginator(ec2.NewFromConfig(cfg), &ec2.DescribeSnapshotsInput{OwnerIds: []string{"self"}})
			for p.HasMorePages() {
				resp, err := p.NextPage(ctx)
				if err != nil {
					return nil, fmt.Errorf("awsx: describe snapshots in %s: %w", r, err)
				}
				for _, s := range resp.Snapshots {
					age := 0
					if s.StartTime != nil {
						age = int(now.Sub(*s.StartTime).Hours() / 24)
					}
					out = append(out, Snapshot{SnapshotID: aws.ToString(s.SnapshotId), SizeGiB: aws.ToInt32(s.VolumeSize), AgeDays: age, Region: r, Description: aws.ToString(s.Description)})
				}
			}
			return out, nil
		})
	})
}

func (c *Client) DescribeInstances(ctx context.Context) ([]Instance, error) {
	return c.mInstances.do(func() ([]Instance, error) {
		return mapRegions(c, ctx, func(ctx context.Context, r string, cfg aws.Config) ([]Instance, error) {
			ec2c := ec2.NewFromConfig(cfg)
			cw := cloudwatch.NewFromConfig(cfg)
			var out []Instance
			p := ec2.NewDescribeInstancesPaginator(ec2c, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{{Name: aws.String("instance-state-name"), Values: []string{"running", "stopped"}}}})
			for p.HasMorePages() {
				resp, err := p.NextPage(ctx)
				if err != nil {
					return nil, fmt.Errorf("awsx: describe instances in %s: %w", r, err)
				}
				for _, res := range resp.Reservations {
					for _, i := range res.Instances {
						id := aws.ToString(i.InstanceId)
						state := ""
						if i.State != nil {
							state = string(i.State.Name)
						}
						// No CPU series when stopped; absent keeps it out of
						// the idle band.
						var cpu Metric
						if state == "running" {
							cpu = c.avgMetric(ctx, cw, "AWS/EC2", r, "InstanceId", id)
						}
						out = append(out, Instance{InstanceID: id, InstanceType: string(i.InstanceType), State: state, Region: r, AvgCPUPercent: cpu})
					}
				}
			}
			return out, nil
		})
	})
}

// --- RDS --------------------------------------------------------------------

func (c *Client) DescribeDBInstances(ctx context.Context) ([]DBInstance, error) {
	return c.mDBs.do(func() ([]DBInstance, error) {
		return mapRegions(c, ctx, func(ctx context.Context, r string, cfg aws.Config) ([]DBInstance, error) {
			cw := cloudwatch.NewFromConfig(cfg)
			var out []DBInstance
			p := rds.NewDescribeDBInstancesPaginator(rds.NewFromConfig(cfg), &rds.DescribeDBInstancesInput{})
			for p.HasMorePages() {
				resp, err := p.NextPage(ctx)
				if err != nil {
					return nil, fmt.Errorf("awsx: describe db instances in %s: %w", r, err)
				}
				for _, db := range resp.DBInstances {
					id := aws.ToString(db.DBInstanceIdentifier)
					out = append(out, DBInstance{DBInstanceID: id, Class: aws.ToString(db.DBInstanceClass), Region: r, AvgCPUPercent: c.avgMetric(ctx, cw, "AWS/RDS", r, "DBInstanceIdentifier", id), StorageType: aws.ToString(db.StorageType), AllocatedStorageGiB: aws.ToInt32(db.AllocatedStorage)})
				}
			}
			return out, nil
		})
	})
}

func (c *Client) DescribeDBSnapshots(ctx context.Context) ([]DBSnapshot, error) {
	return c.mDBSnaps.do(func() ([]DBSnapshot, error) {
		now := time.Now()
		return mapRegions(c, ctx, func(ctx context.Context, r string, cfg aws.Config) ([]DBSnapshot, error) {
			var out []DBSnapshot
			p := rds.NewDescribeDBSnapshotsPaginator(rds.NewFromConfig(cfg), &rds.DescribeDBSnapshotsInput{SnapshotType: aws.String("manual")})
			for p.HasMorePages() {
				resp, err := p.NextPage(ctx)
				if err != nil {
					return nil, fmt.Errorf("awsx: describe db snapshots in %s: %w", r, err)
				}
				for _, s := range resp.DBSnapshots {
					age := 0
					if s.SnapshotCreateTime != nil {
						age = int(now.Sub(*s.SnapshotCreateTime).Hours() / 24)
					}
					out = append(out, DBSnapshot{SnapshotID: aws.ToString(s.DBSnapshotIdentifier), SizeGiB: aws.ToInt32(s.AllocatedStorage), AgeDays: age, Region: r})
				}
			}
			return out, nil
		})
	})
}

// --- Network ----------------------------------------------------------------

func (c *Client) DescribeNatGateways(ctx context.Context) ([]NatGateway, error) {
	return c.mNats.do(func() ([]NatGateway, error) {
		return mapRegions(c, ctx, func(ctx context.Context, r string, cfg aws.Config) ([]NatGateway, error) {
			cw := cloudwatch.NewFromConfig(cfg)
			var out []NatGateway
			p := ec2.NewDescribeNatGatewaysPaginator(ec2.NewFromConfig(cfg), &ec2.DescribeNatGatewaysInput{})
			for p.HasMorePages() {
				resp, err := p.NextPage(ctx)
				if err != nil {
					return nil, fmt.Errorf("awsx: describe nat gateways in %s: %w", r, err)
				}
				for _, n := range resp.NatGateways {
					id := aws.ToString(n.NatGatewayId)
					// Safe here and only here: NAT-A001 looks for HEAVY traffic,
					// so an absent reading suppresses the finding instead of
					// manufacturing one.
					bytesOut := c.sumMetric(ctx, cw, "AWS/NATGateway", r, "NatGatewayId", id, "BytesOutToDestination")
					out = append(out, NatGateway{NatGatewayID: id, Region: r, MonthlyDataGB: bytesOut.Or(0) / 1_000_000_000})
				}
			}
			return out, nil
		})
	})
}

func (c *Client) DescribeRouteTables(ctx context.Context) ([]RouteTable, error) {
	return c.mRoutes.do(func() ([]RouteTable, error) {
		return mapRegions(c, ctx, func(ctx context.Context, r string, cfg aws.Config) ([]RouteTable, error) {
			var out []RouteTable
			p := ec2.NewDescribeRouteTablesPaginator(ec2.NewFromConfig(cfg), &ec2.DescribeRouteTablesInput{})
			for p.HasMorePages() {
				resp, err := p.NextPage(ctx)
				if err != nil {
					return nil, fmt.Errorf("awsx: describe route tables in %s: %w", r, err)
				}
				for _, rt := range resp.RouteTables {
					var nats []string
					for _, route := range rt.Routes {
						if id := aws.ToString(route.NatGatewayId); id != "" {
							nats = append(nats, id)
						}
					}
					out = append(out, RouteTable{ID: aws.ToString(rt.RouteTableId), Region: r, NatGatewayIDs: nats})
				}
			}
			return out, nil
		})
	})
}

func (c *Client) DescribeLoadBalancers(ctx context.Context) ([]LoadBalancer, error) {
	return c.mLBs.do(func() ([]LoadBalancer, error) {
		return mapRegions(c, ctx, func(ctx context.Context, r string, cfg aws.Config) ([]LoadBalancer, error) {
			elb := elbv2.NewFromConfig(cfg)
			var out []LoadBalancer
			p := elbv2.NewDescribeLoadBalancersPaginator(elb, &elbv2.DescribeLoadBalancersInput{})
			for p.HasMorePages() {
				resp, err := p.NextPage(ctx)
				if err != nil {
					return nil, fmt.Errorf("awsx: describe load balancers in %s: %w", r, err)
				}
				for _, lb := range resp.LoadBalancers {
					arn := aws.ToString(lb.LoadBalancerArn)
					// Network LBs publish no RequestCount; absent, not "no traffic".
					var reqPerDay Metric
					if string(lb.Type) == "application" {
						if total := c.sumMetric(ctx, cloudwatch.NewFromConfig(cfg), "AWS/ApplicationELB", r, "LoadBalancer", lbDimension(arn), "RequestCount"); total.Known {
							reqPerDay = Measured(total.Value / 30.0)
						}
					}
					out = append(out, LoadBalancer{ARN: arn, Name: aws.ToString(lb.LoadBalancerName), Kind: string(lb.Type), Region: r, HealthyTargets: countHealthyTargets(ctx, elb, arn), AvgDailyRequests: reqPerDay})
				}
			}
			return out, nil
		})
	})
}

// lbDimension is everything after "loadbalancer/" in an ELB ARN.
func lbDimension(arn string) string {
	const marker = "loadbalancer/"
	if i := strings.Index(arn, marker); i >= 0 {
		return arn[i+len(marker):]
	}
	return arn
}

func countHealthyTargets(ctx context.Context, elb *elbv2.Client, lbARN string) int {
	tgs, err := elb.DescribeTargetGroups(ctx, &elbv2.DescribeTargetGroupsInput{LoadBalancerArn: aws.String(lbARN)})
	if err != nil {
		return 0
	}
	count := 0
	for _, tg := range tgs.TargetGroups {
		h, err := elb.DescribeTargetHealth(ctx, &elbv2.DescribeTargetHealthInput{TargetGroupArn: tg.TargetGroupArn})
		if err != nil {
			continue
		}
		for _, d := range h.TargetHealthDescriptions {
			if d.TargetHealth != nil && string(d.TargetHealth.State) == "healthy" {
				count++
			}
		}
	}
	return count
}

// --- Observability / DynamoDB ----------------------------------------------

func (c *Client) DescribeLogGroups(ctx context.Context) ([]LogGroup, error) {
	return c.mLogs.do(func() ([]LogGroup, error) {
		return mapRegions(c, ctx, func(ctx context.Context, r string, cfg aws.Config) ([]LogGroup, error) {
			var out []LogGroup
			p := cloudwatchlogs.NewDescribeLogGroupsPaginator(cloudwatchlogs.NewFromConfig(cfg), &cloudwatchlogs.DescribeLogGroupsInput{})
			for p.HasMorePages() {
				resp, err := p.NextPage(ctx)
				if err != nil {
					return nil, fmt.Errorf("awsx: describe log groups in %s: %w", r, err)
				}
				for _, g := range resp.LogGroups {
					out = append(out, LogGroup{Name: aws.ToString(g.LogGroupName), Region: r, StoredBytes: aws.ToInt64(g.StoredBytes), RetentionDays: aws.ToInt32(g.RetentionInDays)})
				}
			}
			return out, nil
		})
	})
}

func (c *Client) DescribeDynamoTables(ctx context.Context) ([]DynamoTable, error) {
	return c.mDynamo.do(func() ([]DynamoTable, error) {
		return mapRegions(c, ctx, func(ctx context.Context, r string, cfg aws.Config) ([]DynamoTable, error) {
			ddb := dynamodb.NewFromConfig(cfg)
			cw := cloudwatch.NewFromConfig(cfg)
			var out []DynamoTable
			p := dynamodb.NewListTablesPaginator(ddb, &dynamodb.ListTablesInput{})
			for p.HasMorePages() {
				resp, err := p.NextPage(ctx)
				if err != nil {
					return nil, fmt.Errorf("awsx: list dynamo tables in %s: %w", r, err)
				}
				for _, name := range resp.TableNames {
					d, err := ddb.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(name)})
					if err != nil || d.Table == nil {
						continue
					}
					billing := "PROVISIONED"
					if d.Table.BillingModeSummary != nil {
						billing = string(d.Table.BillingModeSummary.BillingMode)
					}
					var rcu, wcu int64
					if d.Table.ProvisionedThroughput != nil {
						rcu = aws.ToInt64(d.Table.ProvisionedThroughput.ReadCapacityUnits)
						wcu = aws.ToInt64(d.Table.ProvisionedThroughput.WriteCapacityUnits)
					}
					class := "STANDARD"
					if d.Table.TableClassSummary != nil && d.Table.TableClassSummary.TableClass != "" {
						class = string(d.Table.TableClassSummary.TableClass)
					}
					out = append(out, DynamoTable{Name: name, Region: r, BillingMode: billing, ProvisionedRCU: rcu, ProvisionedWCU: wcu,
						ConsumedRCU: c.sumMetric(ctx, cw, "AWS/DynamoDB", r, "TableName", name, "ConsumedReadCapacityUnits"),
						ConsumedWCU: c.sumMetric(ctx, cw, "AWS/DynamoDB", r, "TableName", name, "ConsumedWriteCapacityUnits"),
						SizeBytes:   aws.ToInt64(d.Table.TableSizeBytes), TableClass: class})
				}
			}
			return out, nil
		})
	})
}

func (c *Client) DescribeRedshiftClusters(ctx context.Context) ([]RedshiftCluster, error) {
	return c.mRedshift.do(func() ([]RedshiftCluster, error) {
		return mapRegions(c, ctx, func(ctx context.Context, r string, cfg aws.Config) ([]RedshiftCluster, error) {
			cw := cloudwatch.NewFromConfig(cfg)
			var out []RedshiftCluster
			p := redshift.NewDescribeClustersPaginator(redshift.NewFromConfig(cfg), &redshift.DescribeClustersInput{})
			for p.HasMorePages() {
				resp, err := p.NextPage(ctx)
				if err != nil {
					return nil, fmt.Errorf("awsx: describe redshift clusters in %s: %w", r, err)
				}
				for _, cl := range resp.Clusters {
					id := aws.ToString(cl.ClusterIdentifier)
					out = append(out, RedshiftCluster{
						ClusterID:      id,
						NodeType:       aws.ToString(cl.NodeType),
						NumberOfNodes:  aws.ToInt32(cl.NumberOfNodes),
						Region:         r,
						AvgConnections: c.avgNamedMetric(ctx, cw, "AWS/Redshift", "DatabaseConnections", r, "ClusterIdentifier", id),
					})
				}
			}
			return out, nil
		})
	})
}

// --- S3 (global-ish; queried from the bootstrap region) ---------------------

func (c *Client) ListBuckets(ctx context.Context) ([]Bucket, error) {
	return c.mBuckets.do(func() ([]Bucket, error) {
		pcfg := c.cfgByRegion[c.bootstrap]
		origins, err := c.cloudFrontOriginDomains(ctx)
		if err != nil {
			return nil, err
		}
		resp, err := s3.NewFromConfig(pcfg).ListBuckets(ctx, &s3.ListBucketsInput{})
		if err != nil {
			return nil, fmt.Errorf("awsx: list buckets: %w", err)
		}
		var out []Bucket
		for _, b := range resp.Buckets {
			name := aws.ToString(b.Name)
			region := c.bucketRegion(ctx, pcfg, name)
			cfg, err := c.config(ctx, region)
			if err != nil {
				continue
			}
			s3c := s3.NewFromConfig(cfg)
			mpuBytes, mpuCount, mpuAge := incompleteMPU(ctx, s3c, name)
			out = append(out, Bucket{Name: name, Region: region, HasLifecycle: hasLifecycle(ctx, s3c, name), SizeBytes: bucketSizeBytes(ctx, cloudwatch.NewFromConfig(cfg), name), IsCloudFrontOrigin: matchesOrigin(name, origins), IncompleteMPUBytes: mpuBytes, IncompleteMPUCount: mpuCount, OldestMPUAgeDays: mpuAge})
		}
		return out, nil
	})
}

// ListHostedZones: Route 53 is global, so one call covers the account.
func (c *Client) ListHostedZones(ctx context.Context) ([]HostedZone, error) {
	return c.mZones.do(func() ([]HostedZone, error) {
		pcfg := c.cfgByRegion[c.bootstrap]
		r53 := route53.NewFromConfig(pcfg)
		var out []HostedZone
		p := route53.NewListHostedZonesPaginator(r53, &route53.ListHostedZonesInput{})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("awsx: list hosted zones: %w", err)
			}
			for _, z := range page.HostedZones {
				hz := HostedZone{
					ID:   strings.TrimPrefix(aws.ToString(z.Id), "/hostedzone/"),
					Name: aws.ToString(z.Name),
				}
				if z.ResourceRecordSetCount != nil {
					hz.RecordCount = *z.ResourceRecordSetCount
				}
				if z.Config != nil {
					hz.Private = z.Config.PrivateZone
				}
				out = append(out, hz)
			}
		}
		return out, nil
	})
}

func (c *Client) bucketRegion(ctx context.Context, pcfg aws.Config, name string) string {
	loc, err := s3.NewFromConfig(pcfg).GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String(name)})
	if err != nil {
		return c.bootstrap
	}
	switch r := string(loc.LocationConstraint); r {
	case "":
		return "us-east-1"
	case "EU":
		return "eu-west-1"
	default:
		return r
	}
}

func hasLifecycle(ctx context.Context, s3c *s3.Client, name string) bool {
	out, err := s3c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(name)})
	return err == nil && len(out.Rules) > 0
}

// staleMPUAgeDays is how old an incomplete multipart upload must be before it
// counts as abandoned waste (a fresh upload in flight is not).
const staleMPUAgeDays = 7

// incompleteMPU sums storage held by uploads abandoned past staleMPUAgeDays.
// Best-effort: a denied listing returns what it got, never failing the scan.
func incompleteMPU(ctx context.Context, s3c *s3.Client, name string) (bytes int64, count, oldestAgeDays int) {
	now := time.Now()
	up := s3.NewListMultipartUploadsPaginator(s3c, &s3.ListMultipartUploadsInput{Bucket: aws.String(name)})
	for up.HasMorePages() {
		page, err := up.NextPage(ctx)
		if err != nil {
			return bytes, count, oldestAgeDays
		}
		for _, u := range page.Uploads {
			age := 0
			if u.Initiated != nil {
				age = int(now.Sub(*u.Initiated).Hours() / 24)
			}
			if age < staleMPUAgeDays {
				continue
			}
			var size int64
			pp := s3.NewListPartsPaginator(s3c, &s3.ListPartsInput{Bucket: aws.String(name), Key: u.Key, UploadId: u.UploadId})
			for pp.HasMorePages() {
				parts, err := pp.NextPage(ctx)
				if err != nil {
					break
				}
				for _, part := range parts.Parts {
					size += aws.ToInt64(part.Size)
				}
			}
			bytes += size
			count++
			if age > oldestAgeDays {
				oldestAgeDays = age
			}
		}
	}
	return bytes, count, oldestAgeDays
}

func bucketSizeBytes(ctx context.Context, cw *cloudwatch.Client, name string) int64 {
	now := time.Now()
	out, err := cw.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("AWS/S3"),
		MetricName: aws.String("BucketSizeBytes"),
		StartTime:  aws.Time(now.Add(-48 * time.Hour)),
		EndTime:    aws.Time(now),
		Period:     aws.Int32(86400),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticAverage},
		Dimensions: []cwtypes.Dimension{{Name: aws.String("BucketName"), Value: aws.String(name)}, {Name: aws.String("StorageType"), Value: aws.String("StandardStorage")}},
	})
	if err != nil || len(out.Datapoints) == 0 {
		return 0
	}
	var latest int64
	for _, d := range out.Datapoints {
		if d.Average != nil {
			latest = int64(*d.Average)
		}
	}
	return latest
}

func (c *Client) cloudFrontOriginDomains(ctx context.Context) ([]string, error) {
	cfg, err := c.config(ctx, "us-east-1")
	if err != nil {
		cfg = c.cfgByRegion[c.bootstrap]
	}
	var domains []string
	p := cloudfront.NewListDistributionsPaginator(cloudfront.NewFromConfig(cfg), &cloudfront.ListDistributionsInput{})
	for p.HasMorePages() {
		resp, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("awsx: list cloudfront distributions: %w", err)
		}
		if resp.DistributionList == nil {
			continue
		}
		for _, d := range resp.DistributionList.Items {
			if d.Origins == nil {
				continue
			}
			for _, o := range d.Origins.Items {
				domains = append(domains, aws.ToString(o.DomainName))
			}
		}
	}
	return domains, nil
}

func matchesOrigin(bucket string, originDomains []string) bool {
	for _, d := range originDomains {
		if strings.HasPrefix(d, bucket+".s3") {
			return true
		}
	}
	return false
}

// --- CloudWatch helpers -----------------------------------------------------

const cpuLookback = 14 * 24 * time.Hour

func (c *Client) avgMetric(ctx context.Context, cw *cloudwatch.Client, namespace, region, dimName, dimValue string) Metric {
	return c.avgNamedMetric(ctx, cw, namespace, "CPUUtilization", region, dimName, dimValue)
}

// avgNamedMetric averages a named metric over the CPU lookback window. Used for
// idle signals other than CPU (e.g. Redshift/ElastiCache connection counts).
//
// A failed call or an empty series returns an ABSENT metric, never 0: the
// backend's idle rules read a zero as "this resource does no work" and claim
// its whole monthly cost, so a throttle or a permission gap would invent
// savings that do not exist.
func (c *Client) avgNamedMetric(ctx context.Context, cw *cloudwatch.Client, namespace, metric, region, dimName, dimValue string) Metric {
	now := time.Now()
	out, err := cw.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String(namespace),
		MetricName: aws.String(metric),
		StartTime:  aws.Time(now.Add(-cpuLookback)),
		EndTime:    aws.Time(now),
		Period:     aws.Int32(3600),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticAverage},
		Dimensions: []cwtypes.Dimension{{Name: aws.String(dimName), Value: aws.String(dimValue)}},
	})
	if err != nil || len(out.Datapoints) == 0 {
		c.gaps.record(namespace, metric, region)
		return Metric{}
	}
	var sum float64
	var n int
	for _, d := range out.Datapoints {
		if d.Average != nil {
			sum += *d.Average
			n++
		}
	}
	if n == 0 {
		c.gaps.record(namespace, metric, region)
		return Metric{}
	}
	return Measured(sum / float64(n))
}

// sumMetric totals a metric over the last 30 days.
//
// Like avgNamedMetric it reports ABSENCE rather than 0. An empty series is
// genuinely ambiguous — a table nobody read and a table CloudWatch would not
// tell us about look identical — and since the rules that consume these sums
// (idle DynamoDB, low-traffic ALB) claim the resource's full cost on a zero,
// the ambiguity has to resolve to "we do not know", not to money.
func (c *Client) sumMetric(ctx context.Context, cw *cloudwatch.Client, namespace, region, dimName, dimValue, metric string) Metric {
	now := time.Now()
	out, err := cw.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String(namespace),
		MetricName: aws.String(metric),
		StartTime:  aws.Time(now.Add(-30 * 24 * time.Hour)),
		EndTime:    aws.Time(now),
		Period:     aws.Int32(86400),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticSum},
		Dimensions: []cwtypes.Dimension{{Name: aws.String(dimName), Value: aws.String(dimValue)}},
	})
	if err != nil || len(out.Datapoints) == 0 {
		c.gaps.record(namespace, metric, region)
		return Metric{}
	}
	var total float64
	var n int
	for _, d := range out.Datapoints {
		if d.Sum != nil {
			total += *d.Sum
			n++
		}
	}
	if n == 0 {
		c.gaps.record(namespace, metric, region)
		return Metric{}
	}
	return Measured(total)
}

var _ Live = (*Client)(nil)
