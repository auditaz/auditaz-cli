package snapshot

import (
	"encoding/json"
	"os"
	"testing"
)

// examplePath is the sample snapshot the README points readers at. It lives in
// docs/ rather than testdata/ because its job is documentation; this test exists
// so the documentation can't drift away from the type it claims to describe.
const examplePath = "../docs/example-snapshot.json"

func TestDocumentedExampleMatchesTheSnapshotType(t *testing.T) {
	// Arrange
	f, err := os.Open(examplePath)
	if err != nil {
		t.Fatalf("open %s: %v", examplePath, err)
	}
	defer f.Close()

	// Act: unknown fields are an error on purpose — a key the example shows but
	// the type no longer has would mislead anyone reading it as the format.
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var snap Snapshot
	err = dec.Decode(&snap)

	// Assert
	if err != nil {
		t.Fatalf("decode %s: %v", examplePath, err)
	}
	if snap.SchemaVersion != SchemaVersion {
		t.Errorf("example declares schema_version %d, but the collector writes %d", snap.SchemaVersion, SchemaVersion)
	}
	if snap.AccountID == "" {
		t.Error("example has no account_id")
	}
	if len(snap.Regions) == 0 {
		t.Error("example shows no regions, so it doesn't illustrate a multi-region scan")
	}
}
