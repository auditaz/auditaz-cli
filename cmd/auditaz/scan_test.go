package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auditaz/auditaz-cli/awsx"
	"github.com/auditaz/auditaz-cli/snapshot"
)

func fixedTime(t *testing.T) time.Time {
	t.Helper()
	now, err := time.Parse(time.RFC3339, "2026-09-14T08:30:15Z")
	if err != nil {
		t.Fatalf("parse fixed time: %v", err)
	}
	return now
}

// chdirTemp moves into a scratch directory so generated names land there.
func chdirTemp(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(prev) })
}

func TestDefaultSnapshotPathIncludesTimeOfDay(t *testing.T) {
	// Arrange
	now := fixedTime(t)

	// Act
	path := defaultSnapshotPath(now)

	// Assert
	if want := "auditaz-scan-2026-09-14-083015.json"; path != want {
		t.Fatalf("defaultSnapshotPath = %q, want %q", path, want)
	}
}

func TestGeneratedNameDoesNotOverwriteAnExistingSnapshot(t *testing.T) {
	// Arrange: a scan already wrote its file this second.
	chdirTemp(t)
	now := fixedTime(t)
	first, err := writeSnapshot("", snapshot.Snapshot{AccountID: "111111111111"}, now)
	if err != nil {
		t.Fatalf("first writeSnapshot: %v", err)
	}

	// Act: a second scan in the same second asks for a generated name too.
	second, err := writeSnapshot("", snapshot.Snapshot{AccountID: "222222222222"}, now)
	if err != nil {
		t.Fatalf("second writeSnapshot: %v", err)
	}

	// Assert: distinct files, and the first one still holds its own data.
	if first == second {
		t.Fatalf("second scan reused %q instead of picking a free name", first)
	}
	if want := "auditaz-scan-2026-09-14-083015-2.json"; second != want {
		t.Fatalf("second path = %q, want %q", second, want)
	}
	if got := readAccountID(t, first); got != "111111111111" {
		t.Fatalf("first file was clobbered: account id = %q", got)
	}
	if got := readAccountID(t, second); got != "222222222222" {
		t.Fatalf("second file holds %q", got)
	}
}

func TestExplicitOutputIsTruncatedAsAsked(t *testing.T) {
	// Arrange
	path := filepath.Join(t.TempDir(), "chosen.json")
	if err := os.WriteFile(path, []byte("stale contents that must go away"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	// Act
	got, err := writeSnapshot(path, snapshot.Snapshot{AccountID: "333333333333"}, fixedTime(t))
	if err != nil {
		t.Fatalf("writeSnapshot: %v", err)
	}

	// Assert
	if got != path {
		t.Fatalf("writeSnapshot returned %q, want the requested %q", got, path)
	}
	if id := readAccountID(t, path); id != "333333333333" {
		t.Fatalf("account id = %q, want the freshly written snapshot", id)
	}
}

func TestSnapshotFileIsOwnerReadableOnly(t *testing.T) {
	// Arrange
	chdirTemp(t)

	// Act
	path, err := writeSnapshot("", snapshot.Snapshot{}, fixedTime(t))
	if err != nil {
		t.Fatalf("writeSnapshot: %v", err)
	}

	// Assert
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != snapshotFileMode {
		t.Fatalf("permissions = %o, want %o", perm, snapshotFileMode)
	}
}

// readAccountID decodes a written snapshot and returns its account id, which is
// enough to tell two snapshots apart.
func readAccountID(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var snap snapshot.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return snap.AccountID
}

func TestCredentialHintNamesTheProfileThatFailed(t *testing.T) {
	// Arrange
	err := fmt.Errorf("initialize: %w: ExpiredToken", awsx.ErrCredentialsUnusable)

	// Act
	hint := credentialHint(err, "auditaz")

	// Assert
	if !strings.Contains(hint, "aws sso login --profile auditaz") {
		t.Errorf("hint doesn't offer the profile-specific login command:\n%s", hint)
	}
	if strings.Contains(hint, "role") {
		t.Errorf("hint mentions a role, which the local collector never assumes:\n%s", hint)
	}
}

func TestCredentialHintFallsBackToTheDefaultProfile(t *testing.T) {
	// Arrange
	err := fmt.Errorf("wrapped: %w", awsx.ErrCredentialsUnusable)

	// Act
	hint := credentialHint(err, "")

	// Assert
	if !strings.Contains(hint, "your default profile") {
		t.Errorf("hint doesn't name the default profile:\n%s", hint)
	}
	if strings.Contains(hint, "--profile") {
		t.Errorf("hint suggests a --profile flag with no profile to name:\n%s", hint)
	}
}

func TestCredentialHintStaysSilentForUnrelatedFailures(t *testing.T) {
	// Arrange: a network error, not a credential rejection.
	err := errors.New("dial tcp: connection refused")

	// Act
	hint := credentialHint(err, "auditaz")

	// Assert
	if hint != "" {
		t.Errorf("hint fired on an unrelated error:\n%s", hint)
	}
}
