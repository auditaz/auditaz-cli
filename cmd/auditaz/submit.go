package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/auditaz/auditaz-cli/snapshot"
)

// runSubmit uploads a snapshot file the user already collected. It shares the
// exact upload path used by `scan`'s confirmation (uploadAndReport), so there is
// one and only one place in the CLI that sends data over the network besides it.
const submitUsage = "usage: auditaz submit <file.json> --email you@example.com"

func runSubmit(args []string) error {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	email := fs.String("email", "", "email to associate with the report (required)")
	file, err := parseSubmitArgs(fs, args)
	if err != nil {
		return err
	}
	if !looksLikeEmail(*email) {
		return fmt.Errorf("a valid --email is required")
	}

	snap, err := readSnapshot(file)
	if err != nil {
		return err
	}

	ctx, cancel := newContext()
	defer cancel()
	return uploadAndReport(ctx, defaultEndpoint, *email, snap)
}

// parseSubmitArgs returns the snapshot file name, accepting flags on either
// side of it. Go's flag package stops at the first positional, so a single Parse
// would silently drop the --email in `submit snap.json --email you@example.com`
// — the exact line `scan` prints and the README documents. Hence a second pass.
func parseSubmitArgs(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() == 0 {
		return "", errors.New(submitUsage)
	}
	file, rest := fs.Arg(0), fs.Args()[1:]
	if len(rest) == 0 {
		return file, nil
	}
	if err := fs.Parse(rest); err != nil {
		return "", err
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), submitUsage)
	}
	return file, nil
}

// uploadResponse is the backend's reply from POST /api/scan.
type uploadResponse struct {
	ReportURL  string `json:"report_url"`
	PublicSlug string `json:"public_slug"`
	Error      string `json:"error"`
}

// uploadAndReport uploads the snapshot and prints the resulting report URL.
func uploadAndReport(ctx context.Context, endpoint, email string, snap snapshot.Snapshot) error {
	res, err := upload(ctx, endpoint, email, snap)
	if err != nil {
		return err
	}
	fmt.Printf("\nReport ready: %s\n", res.ReportURL)
	return nil
}

// upload POSTs {email, snapshot} as JSON to <endpoint>/api/scan. This is one of
// only two network calls the entire CLI ever makes.
func upload(ctx context.Context, endpoint, email string, snap snapshot.Snapshot) (uploadResponse, error) {
	payload := struct {
		Email    string            `json:"email"`
		Snapshot snapshot.Snapshot `json:"snapshot"`
	}{Email: email, Snapshot: snap}

	body, err := json.Marshal(payload)
	if err != nil {
		return uploadResponse{}, fmt.Errorf("encode request: %w", err)
	}

	url := endpoint + "/api/scan"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return uploadResponse{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	fmt.Fprintf(os.Stderr, "Uploading snapshot to %s...\n", url)
	resp, err := httpClient.Do(req)
	if err != nil {
		return uploadResponse{}, fmt.Errorf("upload: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out uploadResponse
	_ = json.Unmarshal(raw, &out)

	if resp.StatusCode/100 != 2 {
		if out.Error != "" {
			return uploadResponse{}, fmt.Errorf("server returned %d: %s", resp.StatusCode, out.Error)
		}
		return uploadResponse{}, fmt.Errorf("server returned %d", resp.StatusCode)
	}
	if out.ReportURL == "" {
		return uploadResponse{}, fmt.Errorf("server did not return a report URL")
	}
	return out, nil
}

// readSnapshot loads and decodes a snapshot file.
func readSnapshot(path string) (snapshot.Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return snapshot.Snapshot{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	snap, err := snapshot.Decode(f)
	if err != nil {
		return snapshot.Snapshot{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return snap, nil
}
