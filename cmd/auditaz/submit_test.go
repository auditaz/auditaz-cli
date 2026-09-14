package main

import (
	"flag"
	"strings"
	"testing"
)

// parse runs the argument splitter the way runSubmit does, returning the file
// name and the value --email ended up with.
func parse(t *testing.T, args ...string) (file, email string, err error) {
	t.Helper()
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	fs.SetOutput(new(strings.Builder)) // keep flag's own usage dump out of test output
	e := fs.String("email", "", "")
	file, err = parseSubmitArgs(fs, args)
	return file, *e, err
}

func TestFlagsAfterTheFileNameAreParsed(t *testing.T) {
	// Arrange: the exact line `scan` prints when you decline its upload.
	// Act
	file, email, err := parse(t, "auditaz-scan-2026-09-14-115650.json", "--email", "you@example.com")

	// Assert
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if file != "auditaz-scan-2026-09-14-115650.json" {
		t.Errorf("file = %q", file)
	}
	if email != "you@example.com" {
		t.Errorf("email = %q, want it parsed from after the file name", email)
	}
}

func TestFlagsBeforeTheFileNameStillWork(t *testing.T) {
	// Act
	file, email, err := parse(t, "--email", "you@example.com", "snap.json")

	// Assert
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if file != "snap.json" || email != "you@example.com" {
		t.Errorf("file = %q, email = %q", file, email)
	}
}

func TestEqualsFormIsAcceptedOnEitherSide(t *testing.T) {
	// Act
	file, email, err := parse(t, "snap.json", "--email=you@example.com")

	// Assert
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if file != "snap.json" || email != "you@example.com" {
		t.Errorf("file = %q, email = %q", file, email)
	}
}

func TestMissingFileIsRejected(t *testing.T) {
	// Act
	_, _, err := parse(t, "--email", "you@example.com")

	// Assert
	if err == nil {
		t.Fatal("expected an error when no snapshot file is given")
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Errorf("error should show usage, got %q", err)
	}
}

func TestASecondFileNameIsRejectedRatherThanIgnored(t *testing.T) {
	// Act
	_, _, err := parse(t, "first.json", "second.json")

	// Assert
	if err == nil {
		t.Fatal("expected an error: silently uploading only the first file would be worse")
	}
	if !strings.Contains(err.Error(), "second.json") {
		t.Errorf("error should name the unexpected argument, got %q", err)
	}
}
