package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auditaz/auditaz-cli/awsx"
	"github.com/auditaz/auditaz-cli/preview"
	"github.com/auditaz/auditaz-cli/snapshot"
)

// runScan collects a snapshot locally, writes it to a JSON file, and — only with
// the user's confirmation — uploads it. With no confirmation (answered no, or a
// non-interactive shell) it just prints how to submit later.
func runScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	// Empty so the SDK resolves it (AWS_REGION, profile, then us-east-1). Every
	// enabled region is scanned regardless; this only picks where discovery starts.
	region := fs.String("region", "", "bootstrap AWS region for region discovery (default: AWS_REGION / profile, else us-east-1)")
	profile := fs.String("profile", "", "AWS named profile to use (default: AWS_PROFILE / default credential chain)")
	output := fs.String("output", "", "output file (default: auditaz-scan-<timestamp>.json)")
	email := fs.String("email", "", "email to associate with the report (asked on upload if omitted)")
	yes := fs.Bool("yes", false, "upload immediately without the interactive prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := newContext()
	defer cancel()

	// Local credential chain only — never assumes a role, never touches a remote.
	if *profile != "" {
		fmt.Fprintf(os.Stderr, "Collecting (read-only) with AWS profile %q...\n", *profile)
	} else {
		fmt.Fprintln(os.Stderr, "Collecting (read-only) with your local AWS credentials...")
	}
	client, err := awsx.NewLocalClient(ctx, *profile, *region)
	if err != nil {
		return fmt.Errorf("initialize AWS access: %w%s", err, credentialHint(err, *profile))
	}
	fmt.Fprintf(os.Stderr, "Scanning %d region(s).\n", len(client.ScannedRegions()))

	snap := snapshot.Collect(ctx, client)
	snap.CollectorVersion = version
	snap.CollectedAt = time.Now().UTC().Format(time.RFC3339)

	// An explicit --output is the caller's choice and is truncated as asked; the
	// generated name never clobbers an earlier snapshot.
	path, err := writeSnapshot(*output, snap, time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("Wrote snapshot to %s\n\n", path)

	// Show the same offline summary `preview` would, so the user sees what was
	// collected before deciding to upload.
	if err := preview.Summarize(snap).Write(os.Stdout); err != nil {
		return err
	}
	return maybeUpload(ctx, snap, *email, *yes, path)
}

// credentialHint turns a rejected credential into the next command to run,
// named for the profile that failed since the fix differs per profile.
func credentialHint(err error, profile string) string {
	if !errors.Is(err, awsx.ErrCredentialsUnusable) {
		return ""
	}
	whose, profileFlag := "your default profile", ""
	if profile != "" {
		whose, profileFlag = fmt.Sprintf("profile %q", profile), " --profile "+profile
	}
	return fmt.Sprintf(`

The credentials for %s are expired or rejected — nothing was read from AWS.
Refresh them and run the scan again:

  aws sso login%s

If that profile doesn't use SSO, renew its keys in ~/.aws/credentials, or
re-export AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN.`, whose, profileFlag)
}

// maybeUpload decides whether to upload now, prompt, or defer to `submit`. The
// destination is the hardcoded defaultEndpoint — there is no runtime override.
func maybeUpload(ctx context.Context, snap snapshot.Snapshot, emailFlag string, yes bool, path string) error {
	deferInstruction := func() {
		fmt.Printf("\nNot uploaded. When you're ready, run:\n  auditaz submit %s --email you@example.com\n", path)
	}

	// --yes: upload without prompting (documented CI/automation path). Still needs
	// an email.
	if yes {
		if emailFlag == "" {
			return fmt.Errorf("--yes requires --email")
		}
		return uploadOrExplain(ctx, emailFlag, snap, path)
	}

	// Non-interactive without --yes: never upload silently.
	if !isInteractive() {
		deferInstruction()
		return nil
	}

	// Name the file again at the decision point — this is the moment the engineer
	// inspects exactly what will be sent before answering.
	fmt.Printf("\nReview %s before uploading — it contains exactly what will be sent.\n", path)
	ans := prompt(fmt.Sprintf("Upload to %s/api/scan and generate your report? [y/N] ", defaultEndpoint))
	if ans != "y" && ans != "Y" && ans != "yes" {
		deferInstruction()
		return nil
	}

	email := emailFlag
	for !looksLikeEmail(email) {
		email = prompt("Email for your report: ")
		if email == "" {
			deferInstruction()
			return nil
		}
	}
	return uploadOrExplain(ctx, email, snap, path)
}

// uploadOrExplain uploads and, on failure, says where the snapshot still is: a
// failed upload is not a lost scan, and `submit` retries it without touching AWS.
func uploadOrExplain(ctx context.Context, email string, snap snapshot.Snapshot, path string) error {
	err := uploadAndReport(ctx, defaultEndpoint, email, snap)
	if err == nil {
		return nil
	}
	// Wrapped, not printed: main prints the error last, so stderr output here
	// would land above the failure it explains.
	return fmt.Errorf("%w\n\nThe snapshot is still saved at %s — nothing was lost and there's no need to scan again.\nRetry the upload on its own with:\n  auditaz submit %s --email %s", err, path, path, email)
}

// snapshotFileMode keeps the file owner-only: it is a full inventory of an AWS
// account, not something to leave world-readable on a shared machine.
const snapshotFileMode fs.FileMode = 0o600

// maxSnapshotSuffix bounds the -2, -3 … search for a free name.
const maxSnapshotSuffix = 100

// defaultSnapshotPath carries a full timestamp, not just the date: several scans
// a day (one per profile, a re-run after a fix) are normal, and a date-only name
// would silently overwrite the earlier one.
func defaultSnapshotPath(now time.Time) string {
	return fmt.Sprintf("auditaz-scan-%s.json", now.Format("2006-01-02-150405"))
}

// writeSnapshot writes indented JSON and returns the path used. An empty
// requested path means "name it for me", and never destroys an existing file.
func writeSnapshot(requested string, snap snapshot.Snapshot, now time.Time) (string, error) {
	f, path, err := createSnapshotFile(requested, now)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := snap.Encode(f); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// createSnapshotFile opens the destination. A generated name is created with
// O_EXCL, walking to -2, -3 … on collision. O_EXCL rather than a prior os.Stat
// because check and create must be one operation: otherwise two concurrent
// scans both see "free" and one loses its snapshot.
func createSnapshotFile(requested string, now time.Time) (*os.File, string, error) {
	if requested != "" {
		f, err := os.OpenFile(requested, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, snapshotFileMode)
		if err != nil {
			return nil, "", fmt.Errorf("create %s: %w", requested, err)
		}
		return f, requested, nil
	}

	base := defaultSnapshotPath(now)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i <= maxSnapshotSuffix; i++ {
		path := base
		if i > 1 {
			path = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, snapshotFileMode)
		if err == nil {
			return f, path, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", fmt.Errorf("create %s: %w", path, err)
		}
	}
	return nil, "", fmt.Errorf("create %s: %d files with that name already exist; pass --output to choose one", base, maxSnapshotSuffix)
}
