package awsx

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// Access levels as classified by the AWS Service Authorization Reference.
const (
	levelList = "List"
	levelRead = "Read"
)

// readOnlyLevels are the only levels the collector may use — anything else
// (Write, Tagging, Permissions management) means the collector could change the
// account, which must never happen.
var readOnlyLevels = map[string]bool{levelList: true, levelRead: true}

// loadReference reads the vendored AWS Service Authorization Reference subset:
// action -> access level.
func loadReference(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reference: %v", err)
	}
	var ref map[string]string
	if err := json.Unmarshal(b, &ref); err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	return ref
}

// validateReadOnly fails if any action is unknown to the reference or is not a
// read-only (List/Read) action. An unknown action is a failure so a newly added
// call must be classified consciously before it can ship.
func validateReadOnly(ref map[string]string, actions []string) []string {
	var problems []string
	for _, a := range actions {
		lvl, ok := ref[a]
		if !ok {
			problems = append(problems, a+" (unknown — add it to service-reference.json with its access level)")
			continue
		}
		if !readOnlyLevels[lvl] {
			problems = append(problems, a+" is "+lvl+" (not read-only)")
		}
	}
	sort.Strings(problems)
	return problems
}

// TestCollectorActionsAreReadOnly is the trust guardrail (spec §3.3, item 7):
// every action the collector issues must be a List/Read action classified in
// the vendored AWS reference. A write / tagging / permission-management action —
// or one not classified at all — fails this test and blocks the release.
func TestCollectorActionsAreReadOnly(t *testing.T) {
	ref := loadReference(t, "testdata/service-reference.json")
	if problems := validateReadOnly(ref, CollectorActions); len(problems) > 0 {
		t.Fatalf("collector issues non-read-only or unclassified actions:\n  - %s", strings.Join(problems, "\n  - "))
	}
}

// TestCollectorActionsShapeSanity guards against an accidentally empty or
// malformed action list (which would make the guardrail above vacuously pass).
func TestCollectorActionsShapeSanity(t *testing.T) {
	if len(CollectorActions) < 20 {
		t.Fatalf("expected the collector to declare its full read-only surface; got %d actions", len(CollectorActions))
	}
	seen := map[string]bool{}
	for _, a := range CollectorActions {
		if !strings.Contains(a, ":") {
			t.Errorf("action %q is not in service:Action form", a)
		}
		if seen[a] {
			t.Errorf("duplicate action %q", a)
		}
		seen[a] = true
	}
}

// TestValidateReadOnlyCatchesWrite proves the guardrail rejects a write action,
// so we know it isn't silently passing everything.
func TestValidateReadOnlyCatchesWrite(t *testing.T) {
	ref := map[string]string{"ec2:TerminateInstances": "Write", "ec2:DescribeInstances": "List"}
	if p := validateReadOnly(ref, []string{"ec2:DescribeInstances", "ec2:TerminateInstances"}); len(p) == 0 {
		t.Fatal("expected validation to reject a Write action")
	}
}

// TestValidateReadOnlyCatchesUnknown proves an unclassified action fails.
func TestValidateReadOnlyCatchesUnknown(t *testing.T) {
	if p := validateReadOnly(map[string]string{}, []string{"ec2:SomethingNew"}); len(p) == 0 {
		t.Fatal("expected validation to reject an unknown action")
	}
}
