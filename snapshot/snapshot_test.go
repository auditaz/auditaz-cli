package snapshot

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/auditaz/auditaz-cli/awsx"
)

func sampleLive() *awsx.FakeLive {
	return &awsx.FakeLive{
		Volumes: []awsx.Volume{
			{VolumeID: "vol-1", State: "available", SizeGiB: 100, Type: "gp2", Region: "us-east-1"},
		},
		HostedZones: []awsx.HostedZone{
			{ID: "Z123", Name: "example.com.", RecordCount: 2},
		},
		CostLines: []awsx.CostLine{
			{Service: "Amazon EC2", UsageType: "BoxUsage:m5.large", AmountUSD: 42.5},
		},
	}
}

// TestCollectGathersData confirms Collect reads the Live surfaces into the
// matching snapshot fields with no spurious errors on success.
func TestCollectGathersData(t *testing.T) {
	s := Collect(context.Background(), sampleLive())

	if s.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", s.SchemaVersion, SchemaVersion)
	}
	if len(s.Errors) != 0 {
		t.Errorf("unexpected collect errors: %+v", s.Errors)
	}
	if len(s.EBSVolumes) != 1 || s.EBSVolumes[0].VolumeID != "vol-1" {
		t.Errorf("EBSVolumes not collected: %+v", s.EBSVolumes)
	}
	if len(s.HostedZones) != 1 || s.HostedZones[0].ID != "Z123" {
		t.Errorf("HostedZones not collected: %+v", s.HostedZones)
	}
	if len(s.CostLines) != 1 || s.CostLines[0].AmountUSD != 42.5 {
		t.Errorf("CostLines not collected: %+v", s.CostLines)
	}
}

// TestCollectToleratesPartialFailure confirms a failing Live records an error
// per call instead of aborting — the whole run must not die on one denial.
func TestCollectToleratesPartialFailure(t *testing.T) {
	live := &awsx.FakeLive{Err: errors.New("AccessDenied")}
	s := Collect(context.Background(), live)

	// All fifteen Live methods fail, so we expect fifteen recorded errors and no
	// data — but Collect still returns normally.
	if len(s.Errors) != 15 {
		t.Fatalf("expected 15 recorded errors, got %d: %+v", len(s.Errors), s.Errors)
	}
	if len(s.EBSVolumes) != 0 {
		t.Errorf("expected no data on total failure, got %+v", s.EBSVolumes)
	}
}

// TestRoundTripAndLive confirms Encode/Decode preserves the data and that Live
// hands it back through the awsx.Live interface unchanged.
func TestRoundTripAndLive(t *testing.T) {
	orig := Collect(context.Background(), sampleLive())

	var buf bytes.Buffer
	if err := orig.Encode(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := Decode(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	live := got.Live()
	vols, err := live.DescribeVolumes(context.Background())
	if err != nil {
		t.Fatalf("DescribeVolumes: %v", err)
	}
	if len(vols) != 1 || vols[0].VolumeID != "vol-1" || vols[0].SizeGiB != 100 {
		t.Errorf("round-tripped volume mismatch: %+v", vols)
	}
	zones, _ := live.ListHostedZones(context.Background())
	if len(zones) != 1 || zones[0].Name != "example.com." {
		t.Errorf("round-tripped hosted zone mismatch: %+v", zones)
	}
}
