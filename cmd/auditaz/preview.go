package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/auditaz/auditaz-cli/preview"
)

// runPreview summarizes a snapshot file entirely offline. It makes no network
// call of any kind — the whole point is that the user can audit what was
// collected with zero data leaving the machine.
func runPreview(args []string) error {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: auditaz preview <file.json>")
	}

	snap, err := readSnapshot(fs.Arg(0))
	if err != nil {
		return err
	}
	return preview.Summarize(snap).Write(os.Stdout)
}
