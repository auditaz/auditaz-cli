// Package preview produces an offline summary of a snapshot: counts and raw
// facts the AWS console already shows for free.
//
// SCOPE GUARD: never surface a dollar estimate or a fix command here. Pricing
// and remediation are the paid product and live only on the backend. A fact that
// can only be expressed with a price or a fix belongs in the full report.
package preview

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/auditaz/auditaz-cli/snapshot"
)

// Row is one counted fact. Note carries an extra raw fact (never a dollar amount
// and never a command).
type Row struct {
	Label string
	Count int
	Note  string
}

// Summary is the offline preview of a snapshot.
type Summary struct {
	AccountID    string
	Regions      []string
	CollectedAt  string
	Rows         []Row
	SkippedCalls []string // Live calls that failed during collection (e.g. denied)
}

// Summarize reduces a snapshot to counts and raw facts. No pricing, no commands.
func Summarize(s snapshot.Snapshot) Summary {
	sum := Summary{
		AccountID:   s.AccountID,
		Regions:     s.Regions,
		CollectedAt: s.CollectedAt,
	}
	for _, e := range s.Errors {
		sum.SkippedCalls = append(sum.SkippedCalls, e.Call)
	}

	add := func(label string, count int, note string) {
		sum.Rows = append(sum.Rows, Row{Label: label, Count: count, Note: note})
	}

	// EC2 / EBS.
	stopped := countFunc(s.EC2Instances, func(i int) bool { return s.EC2Instances[i].State == "stopped" })
	add("EC2 instances", len(s.EC2Instances), fmt.Sprintf("%d stopped", stopped))
	unattached := countFunc(s.EBSVolumes, func(i int) bool { return s.EBSVolumes[i].State == "available" })
	add("EBS volumes", len(s.EBSVolumes), fmt.Sprintf("%d unattached", unattached))
	add("EBS snapshots", len(s.EBSSnapshots), "")
	unassoc := countFunc(s.ElasticIPs, func(i int) bool { return !s.ElasticIPs[i].Associated })
	add("Elastic IPs", len(s.ElasticIPs), fmt.Sprintf("%d not associated", unassoc))

	// S3.
	noLifecycle := countFunc(s.S3Buckets, func(i int) bool { return !s.S3Buckets[i].HasLifecycle })
	empty := countFunc(s.S3Buckets, func(i int) bool { return s.S3Buckets[i].SizeBytes == 0 })
	add("S3 buckets", len(s.S3Buckets), fmt.Sprintf("%d without lifecycle, %d empty", noLifecycle, empty))

	// RDS.
	add("RDS instances", len(s.RDSInstances), "")
	add("RDS snapshots", len(s.RDSSnapshots), "")

	// Networking.
	add("NAT gateways", len(s.NatGateways), "")
	noHealthy := countFunc(s.LoadBalancers, func(i int) bool { return s.LoadBalancers[i].HealthyTargets == 0 })
	add("Load balancers", len(s.LoadBalancers), fmt.Sprintf("%d with no healthy targets", noHealthy))

	// Logs / DynamoDB / Redshift.
	neverExpire := countFunc(s.LogGroups, func(i int) bool { return s.LogGroups[i].RetentionDays == 0 })
	add("CloudWatch log groups", len(s.LogGroups), fmt.Sprintf("%d never expire", neverExpire))
	provisioned := countFunc(s.DynamoTables, func(i int) bool { return s.DynamoTables[i].BillingMode == "PROVISIONED" })
	add("DynamoDB tables", len(s.DynamoTables), fmt.Sprintf("%d provisioned", provisioned))
	add("Redshift clusters", len(s.RedshiftClusters), "")

	// Route 53. An empty zone has just the apex NS + SOA (RecordCount <= 2).
	emptyZones := countFunc(s.HostedZones, func(i int) bool { return s.HostedZones[i].RecordCount <= 2 })
	add("Route 53 hosted zones", len(s.HostedZones), fmt.Sprintf("%d empty", emptyZones))

	return sum
}

// countFunc counts indices in a slice of length len(xs) for which pred is true.
// It takes the length via the slice so callers pass the typed slice directly.
func countFunc[T any](xs []T, pred func(i int) bool) int {
	n := 0
	for i := range xs {
		if pred(i) {
			n++
		}
	}
	return n
}

// Write renders the summary as a plain-text table. It deliberately prints no
// monetary value and no remediation command (see the package scope guard).
func (sum Summary) Write(w io.Writer) error {
	if sum.AccountID != "" {
		fmt.Fprintf(w, "Account:   %s\n", sum.AccountID)
	}
	if len(sum.Regions) > 0 {
		fmt.Fprintf(w, "Regions:   %d scanned\n", len(sum.Regions))
	}
	if sum.CollectedAt != "" {
		fmt.Fprintf(w, "Collected: %s\n", sum.CollectedAt)
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RESOURCE\tCOUNT\tNOTES")
	for _, r := range sum.Rows {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", r.Label, r.Count, r.Note)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(sum.SkippedCalls) > 0 {
		fmt.Fprintf(w, "\nPartial: %d call(s) failed in at least one region: %v\n", len(sum.SkippedCalls), sum.SkippedCalls)
		fmt.Fprint(w, "Causes vary — a denied permission, an unreachable endpoint, a service absent\nfrom a region. The counts above cover only the regions that answered, so treat\nthose rows as a floor, not a total. Full messages are in the snapshot's errors.\n")
	}
	fmt.Fprintln(w, "\nCounts and facts only. Estimated savings and fix commands are in the full report.")
	return nil
}
