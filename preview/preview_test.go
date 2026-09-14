package preview

import (
	"bytes"
	"strings"
	"testing"

	"github.com/auditaz/auditaz-cli/awsx"
	"github.com/auditaz/auditaz-cli/snapshot"
)

func fixture() snapshot.Snapshot {
	return snapshot.Snapshot{
		AccountID: "123456789012",
		Regions:   []string{"us-east-1", "eu-west-1"},
		EC2Instances: []awsx.Instance{
			{InstanceID: "i-1", State: "running"},
			{InstanceID: "i-2", State: "stopped"},
		},
		EBSVolumes: []awsx.Volume{
			{VolumeID: "vol-1", State: "available"}, // unattached
			{VolumeID: "vol-2", State: "in-use"},
		},
		HostedZones: []awsx.HostedZone{
			{ID: "Z1", RecordCount: 2},  // empty
			{ID: "Z2", RecordCount: 10}, // in use
		},
		// CostLines carry dollar amounts — preview must never surface them.
		CostLines: []awsx.CostLine{{Service: "Amazon EC2", AmountUSD: 999.99}},
	}
}

func TestSummarizeCounts(t *testing.T) {
	sum := Summarize(fixture())

	find := func(label string) Row {
		for _, r := range sum.Rows {
			if r.Label == label {
				return r
			}
		}
		t.Fatalf("row %q not found", label)
		return Row{}
	}

	if r := find("EC2 instances"); r.Count != 2 || !strings.Contains(r.Note, "1 stopped") {
		t.Errorf("EC2 row wrong: %+v", r)
	}
	if r := find("EBS volumes"); r.Count != 2 || !strings.Contains(r.Note, "1 unattached") {
		t.Errorf("EBS row wrong: %+v", r)
	}
	if r := find("Route 53 hosted zones"); r.Count != 2 || !strings.Contains(r.Note, "1 empty") {
		t.Errorf("hosted zone row wrong: %+v", r)
	}
}

// TestPreviewNeverShowsMoneyOrCommands is the scope guard as an executable
// check: the rendered preview must contain no dollar amount and no "aws " CLI
// command, even though the snapshot carries cost data. This protects the paid
// product's boundary (spec §2.3 preview scope).
func TestPreviewNeverShowsMoneyOrCommands(t *testing.T) {
	var buf bytes.Buffer
	if err := Summarize(fixture()).Write(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, "$") || strings.Contains(out, "999") || strings.Contains(out, "USD") {
		t.Errorf("preview leaked a dollar value:\n%s", out)
	}
	if strings.Contains(out, "aws ") {
		t.Errorf("preview leaked a fix command:\n%s", out)
	}
}
