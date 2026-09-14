package awsx

import (
	"encoding/json"
	"testing"
)

// The defect this type exists to prevent: CloudWatch returning nothing — a
// throttle, a missing permission, an instance younger than the lookback window
// — used to surface as 0.0, which every idle rule reads as "does nothing, stop
// it" and bills at the resource's full monthly cost. Absent is not zero.
func TestUnknownMetricIsNeverIdle(t *testing.T) {
	var unknown Metric
	if unknown.Known {
		t.Fatal("the zero Metric must be unknown")
	}
	if unknown.Below(5) {
		t.Error("an unmeasured metric must never satisfy Below: that is how a CloudWatch failure became a finding")
	}
	if unknown.AtLeast(5) {
		t.Error("an unmeasured metric must not satisfy AtLeast either — unknown answers no to both")
	}
}

func TestMeasuredZeroIsStillIdle(t *testing.T) {
	measured := Measured(0)
	if !measured.Known {
		t.Fatal("Measured must mark the value as known")
	}
	if !measured.Below(5) {
		t.Error("a genuine zero reading is idle and must stay a finding")
	}
}

func TestMetricComparisons(t *testing.T) {
	m := Measured(12.5)
	if !m.Below(20) || m.Below(12.5) {
		t.Errorf("Below is a strict less-than: %+v", m)
	}
	if !m.AtLeast(12.5) || m.AtLeast(20) {
		t.Errorf("AtLeast is a greater-or-equal: %+v", m)
	}
}

// Or supplies a fallback for arithmetic that has to produce some number; it
// forces the caller to name the value it is assuming rather than inheriting a
// silent zero.
func TestMetricOrReturnsFallbackWhenUnknown(t *testing.T) {
	var unknown Metric
	if got := unknown.Or(3); got != 3 {
		t.Errorf("Or(3) on unknown = %v, want 3", got)
	}
	if got := Measured(9).Or(3); got != 9 {
		t.Errorf("Or(3) on a known 9 = %v, want 9", got)
	}
}

// The snapshot wire format stays a plain number so the file an engineer
// inspects before uploading is still readable; absent is null, never 0.
func TestMetricJSONRoundTrip(t *testing.T) {
	type holder struct {
		CPU Metric `json:"avg_cpu_percent"`
	}

	b, err := json.Marshal(holder{CPU: Measured(42.5)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"avg_cpu_percent":42.5}` {
		t.Errorf("known metric marshalled as %s, want a bare number", b)
	}

	b, err = json.Marshal(holder{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"avg_cpu_percent":null}` {
		t.Errorf("unknown metric marshalled as %s, want null", b)
	}

	var back holder
	if err := json.Unmarshal([]byte(`{"avg_cpu_percent":null}`), &back); err != nil {
		t.Fatalf("unmarshal null: %v", err)
	}
	if back.CPU.Known {
		t.Error("null must decode as unknown")
	}

	if err := json.Unmarshal([]byte(`{"avg_cpu_percent":7}`), &back); err != nil {
		t.Fatalf("unmarshal number: %v", err)
	}
	if !back.CPU.Known || back.CPU.Value != 7 {
		t.Errorf("number decoded as %+v, want a known 7", back.CPU)
	}
}

// A key the producer never wrote decodes as unknown, so a snapshot from an
// older collector cannot be mistaken for a measured zero.
func TestMetricAbsentKeyDecodesAsUnknown(t *testing.T) {
	var h struct {
		CPU Metric `json:"avg_cpu_percent"`
	}
	if err := json.Unmarshal([]byte(`{}`), &h); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if h.CPU.Known {
		t.Error("a missing key must decode as unknown")
	}
}
