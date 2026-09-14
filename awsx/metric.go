package awsx

import (
	"bytes"
	"encoding/json"
)

// Metric is a CloudWatch reading that may be absent, because "CloudWatch
// returned nothing" and "the resource did nothing" are different facts and
// collapsing them invents savings: an idle rule reads a zero as "stop this" and
// claims the full monthly cost. The zero value is the unknown one, and an
// unknown Metric answers no to every comparison, so a rule that forgets to
// check loses a finding rather than fabricating one.
type Metric struct {
	Value float64
	Known bool
}

// Measured builds a Metric from a real reading. A genuine 0 stays a finding.
func Measured(v float64) Metric { return Metric{Value: v, Known: true} }

// Below reports whether the metric was measured and sits strictly under t.
func (m Metric) Below(t float64) bool { return m.Known && m.Value < t }

// AtLeast reports whether the metric was measured and reaches t.
func (m Metric) AtLeast(t float64) bool { return m.Known && m.Value >= t }

// Or returns the reading, or fallback when there is none, forcing the caller to
// name the value it assumes instead of inheriting a silent zero.
func (m Metric) Or(fallback float64) float64 {
	if !m.Known {
		return fallback
	}
	return m.Value
}

// MarshalJSON writes a bare number, or null when the metric is absent.
func (m Metric) MarshalJSON() ([]byte, error) {
	if !m.Known {
		return []byte("null"), nil
	}
	return json.Marshal(m.Value)
}

// UnmarshalJSON reads a number as a reading and null as absent. An absent key
// leaves the zero value, which is already unknown.
func (m *Metric) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		*m = Metric{}
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*m = Measured(v)
	return nil
}
