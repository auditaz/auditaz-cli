// Command auditaz is the open-source, local AWS cost data collector.
//
// It runs on your machine with your own credentials, makes only read-only calls,
// and writes a JSON file you can inspect before anything is uploaded. Pricing
// and fix commands live on the backend, so this program cannot change your
// account and never phones home except on the two uploads you trigger yourself.
//
//	auditaz scan      collect a snapshot locally, then optionally upload it
//	auditaz submit    upload a snapshot file you already collected
//	auditaz preview   summarize a snapshot file locally (offline; no network)
package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// version is stamped at build time via -ldflags (e.g. -X main.version=1.0.0).
var version = "dev"

// defaultEndpoint is compiled in with no flag or environment override, so a tool
// holding account data has no runtime way to redirect where it goes. Its own
// hostname lets the backend move by DNS instead of a recompile nobody installs.
const defaultEndpoint = "https://ingest.auditaz.com"

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "scan":
		err = runScan(os.Args[2:])
	case "submit":
		err = runSubmit(os.Args[2:])
	case "preview":
		err = runPreview(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("auditaz", version)
		return
	case "help", "-h", "--help":
		usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `auditaz — local, read-only AWS cost data collector

Usage:
  auditaz scan     [--profile name] [--region id] [--output file.json] [--email you@co] [--yes]
  auditaz submit   <file.json> --email you@co
  auditaz preview  <file.json>
  auditaz version

scan     collects a snapshot using your local AWS credentials and writes it to
         auditaz-scan-<timestamp>.json (a name that is never reused, so repeated
         scans don't overwrite each other), then asks whether to upload it. It
         never uploads without your confirmation (or an explicit --yes).
submit   uploads a snapshot file you already collected.
preview  prints an offline summary of a snapshot (counts and facts only — no
         estimated savings, no fix commands, no network).
`)
}

// isInteractive reports whether stdin is a terminal, so we only prompt when a
// human can answer (never in CI / piped runs).
func isInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// prompt writes a question and reads one trimmed line from stdin.
func prompt(question string) string {
	fmt.Print(question)
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

// looksLikeEmail is a deliberately loose check — the backend validates for real.
func looksLikeEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	return at > 0 && at < len(s)-1 && !strings.ContainsAny(s, " \t")
}

// newContext returns a context with a generous timeout for a full-account scan.
func newContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Minute)
}

// httpClient is the single client used by the only two network paths in the CLI.
var httpClient = &http.Client{Timeout: 60 * time.Second}
