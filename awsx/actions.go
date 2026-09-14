package awsx

// CollectorActions is the exhaustive set of AWS actions the collector issues,
// every one a Describe/List/Get. It is the source of truth for the trust story:
// the README lists it and nonwrite_test.go fails the build if any entry isn't
// classified read-only. A new API call means updating this list, the backend's
// CloudFormation role, and the site's Security page.
var CollectorActions = []string{
	"ce:GetCostAndUsage",

	"ec2:DescribeRegions", // enabled-region discovery
	"ec2:DescribeInstances",
	"ec2:DescribeVolumes",
	"ec2:DescribeSnapshots",
	"ec2:DescribeAddresses",
	"ec2:DescribeNatGateways",
	"ec2:DescribeRouteTables",

	"rds:DescribeDBInstances",
	"rds:DescribeDBSnapshots",

	"s3:ListAllMyBuckets",
	"s3:GetBucketLocation",
	"s3:GetLifecycleConfiguration",
	"s3:ListBucketMultipartUploads",
	"s3:ListMultipartUploadParts",

	// CloudFront: tells whether a bucket serves a live distribution.
	"cloudfront:ListDistributions",

	// CloudWatch: the utilization behind every idle/oversized signal.
	"cloudwatch:GetMetricStatistics",

	"elasticloadbalancing:DescribeLoadBalancers",
	"elasticloadbalancing:DescribeTargetGroups",
	"elasticloadbalancing:DescribeTargetHealth",

	"logs:DescribeLogGroups",

	"dynamodb:ListTables",
	"dynamodb:DescribeTable",

	"redshift:DescribeClusters",

	"route53:ListHostedZones",

	// STS: preflight that the credentials work before scanning.
	"sts:GetCallerIdentity",
}
