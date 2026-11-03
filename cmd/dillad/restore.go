package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/ops"
)

// cliClock is the clock `dillad restore` stamps the heal deadline and the end
// of every live call with. A variable only so a test can hold it still.
var cliClock = clock.System()

// runRestore is `dillad restore --from=PATH|- [--dry-run] [--force]
// [--remove-old]`: the archive is verified end to end before anything is
// written, the entries of the instance's data directory are swapped for the
// restored ones, and invariant 11's restore half runs on the restored database.
// Every refusal is ops.Restore's: exit 75 while a serve or a backup holds the
// directory, 78 for an archive from a newer schema, another engine or (without
// --force) another instance, 65 for a damaged archive, (without --force) one
// with gaps, or an earlier restore's interrupted swap.
func runRestore(args []string, stdout, stderr io.Writer) error {
	fs, cfgPath := newFlagSet("restore", stderr)
	from := fs.String("from", "", "the archive `dillad backup` wrote, or - for stdin")
	dryRun := fs.Bool("dry-run", false, "verify the archive and print what a restore would do, writing nothing")
	force := fs.Bool("force", false, "restore an archive from another instance, or one whose manifest names gaps")
	removeOld := fs.Bool("remove-old", false, "delete the live entries once the restored ones are in place (default: keep them in <data_dir>/.old-<hex>)")
	if err := parse(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dillad restore: unexpected argument %q\n", fs.Arg(0))
		return exit.Usage
	}
	if *from == "" {
		fmt.Fprintln(stderr, "dillad restore: --from is required")
		return exit.Usage
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	var r io.Reader
	if *from == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(*from)
		if err != nil {
			return fmt.Errorf("restore: %w: %w", err, exit.Data)
		}
		defer func() { _ = f.Close() }()
		r = f
	}

	plan, err := ops.Restore(context.Background(), cfg, ops.RestoreOptions{
		From: r, DryRun: *dryRun, Force: *force, RemoveOld: *removeOld, Clock: cliClock,
	})
	if err != nil {
		return fmt.Errorf("restore: %s: %w", *from, err)
	}
	printPlan(stdout, *from, *cfgPath, plan, *dryRun)
	return nil
}

// printPlan is what the operator reads: the plan on a dry run, and after a
// restore exactly what the clients will see.
func printPlan(w io.Writer, from, cfgPath string, p ops.RestorePlan, dryRun bool) {
	m := p.Manifest
	verb := "restored"
	if dryRun {
		verb = "would restore"
	}
	fmt.Fprintf(w, "dillad restore: %s %s (engine %s, schema %d, written %s by dillad %s)\n",
		verb, from, m.Engine, m.SchemaVersion, m.CreatedAt, m.DillaVersion)
	fmt.Fprintf(w, "  generation: %d -> %d\n", p.GenerationOld, p.GenerationNew)
	fmt.Fprintf(w, "  blobs: %d\n", p.BlobsToWrite)
	tables := make([]string, 0, len(p.RowCounts))
	var total int64
	for t, n := range p.RowCounts {
		tables = append(tables, t)
		total += n
	}
	slices.Sort(tables)
	fmt.Fprintf(w, "  rows: %d in %d tables; the tables holding any:\n", total, len(tables))
	for _, t := range tables {
		if p.RowCounts[t] > 0 {
			fmt.Fprintf(w, "    %-24s %d\n", t, p.RowCounts[t])
		}
	}
	if m.SchemaVersion < p.BinarySchema {
		fmt.Fprintf(w, "  schema: the database is at %d and this binary's at %d; `dillad serve` migrates it at start, after a pre-migration backup\n",
			m.SchemaVersion, p.BinarySchema)
	}
	for _, warning := range p.Warnings {
		fmt.Fprintf(w, "  warning: %s\n", warning)
	}
	// R38: the restore output is one of the three places its wording appears.
	fmt.Fprintf(w, "dillad restore: note: %s\n", ops.ContentNotice)
	window := ds.DefaultPolicy().HealWindow
	if dryRun {
		fmt.Fprintf(w, "dillad restore: dry run: nothing was written. Without --dry-run: live calls end, every client resyncs, "+
			"and every group gets %s to heal.\n", humanWindow(window))
		return
	}
	if p.OldDir != "" {
		fmt.Fprintf(w, "  the previous entries of the data directory are in %s; inspect or remove them (--remove-old does that)\n", p.OldDir)
	}
	if p.ArchivedConfig != "" {
		fmt.Fprintf(w, "  the archive's dilla.toml is at %s; the configuration in use (%s) was not changed\n",
			p.ArchivedConfig, cfgPath)
	}
	fmt.Fprintf(w, "dillad restore: what happens next:\n")
	fmt.Fprintf(w, "  - live calls ended; every client sees generation %d on its next request and resyncs\n", p.GenerationNew)
	fmt.Fprintf(w, "  - %d unused KeyPackages were purged (last-resort ones kept); clients publish fresh ones\n", p.KeyPackagesPurged)
	fmt.Fprintf(w, "  - every group is epoch-unknown: a member heals it by uploading its member-signed GroupInfo and handshake tail\n")
	fmt.Fprintf(w, "  - a group with no member-signed GroupInfo within %s of the next `dillad serve` start is closed and re-created by the channel owner's device\n",
		humanWindow(window))
	fmt.Fprintf(w, "  - start the instance with `dillad serve`; it finishes the restore at start\n")
}

// humanWindow renders a whole-hour window as "24h", the way the protocol
// documents name it.
func humanWindow(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
}
