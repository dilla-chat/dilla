package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/ops"
)

// runBackup is `dillad backup [--out=PATH|-] [--include-blobs] [--label=TEXT]
// [--allow-gaps]` and `dillad backup verify --from=PATH`. The sub-verb is
// popped BEFORE fs.Parse, as migrate does: Go's flag package stops at the first
// non-flag argument, so `verify --config=X` would otherwise never reach
// --config.
func runBackup(args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if args[0] != "verify" {
			fmt.Fprintf(stderr, "dillad backup: unknown sub-verb %q; want verify\n", args[0])
			return exit.Usage
		}
		return runBackupVerify(args[1:], stdout, stderr)
	}

	fs, cfgPath := newFlagSet("backup", stderr)
	out := fs.String("out", "", "archive path, or - for stdout (default dilla-backup-<UTC time>.tar.gz in the working directory)")
	includeBlobs := fs.Bool("include-blobs", true, "archive the attachment blobs (--include-blobs=false leaves them out)")
	label := fs.String("label", "", "free text recorded in the manifest")
	allowGaps := fs.Bool("allow-gaps", false, "write the archive even when a referenced blob's file is missing, naming each gap in the manifest")
	if err := parse(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dillad backup: unexpected argument %q\n", fs.Arg(0))
		return exit.Usage
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	repo, _, err := openRepository(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()

	opts := ops.BackupOptions{
		IncludeBlobs: *includeBlobs, Label: *label, AllowGaps: *allowGaps,
		ConfigPath: *cfgPath, DillaVersion: Version,
	}
	ctx := context.Background()

	// With --out=- stdout carries the gzip stream and nothing else, so the
	// summary goes to stderr.
	if *out == "-" {
		opts.Out = stdout
		man, err := ops.Backup(ctx, cfg, repo, opts)
		if err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		summarise(stderr, "-", man)
		return nil
	}

	dest := *out
	if dest == "" {
		dest = "dilla-backup-" + time.Now().UTC().Format("20060102T150405Z") + ".tar.gz"
	}
	// The archive is written beside its destination and renamed into place
	// only once it is complete, so a refused or interrupted backup never
	// leaves a partial file under the name an operator's rotation looks for.
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	tmp := dest + ".partial-" + hex.EncodeToString(suffix)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator's own --out path
	if err != nil {
		return fmt.Errorf("backup: %w: %w", err, exit.CantCreate)
	}
	opts.Out = f
	man, err := ops.Backup(ctx, cfg, repo, opts)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dest)
	}
	if err != nil {
		_ = os.Remove(tmp)
		var code exit.Code
		if !errors.As(err, &code) {
			err = fmt.Errorf("%w: %w", err, exit.IOErr)
		}
		return fmt.Errorf("backup: %w", err)
	}
	if abs, err := filepath.Abs(dest); err == nil {
		dest = abs
	}
	summarise(stdout, dest, man)
	return nil
}

// summarise prints what was written, and R38's sentence about what it holds.
func summarise(w io.Writer, dest string, man ops.Manifest) {
	fmt.Fprintf(w, "dillad backup: wrote %s (engine %s, schema %d, generation %d, %d blobs, %d bytes in %d members)\n",
		dest, man.Engine, man.SchemaVersion, man.Generation, man.BlobCount, man.TotalBytes, len(man.Entries)+1)
	if len(man.Gaps) > 0 {
		fmt.Fprintf(w, "dillad backup: %d gap(s): referenced blobs whose files were missing are named in MANIFEST.json\n", len(man.Gaps))
	}
	fmt.Fprintf(w, "dillad backup: note: %s\n", ops.ContentNotice)
}

// runBackupVerify is `dillad backup verify --from=PATH`: the manifest and every
// member's size and SHA-256, with no writes. A damaged archive exits 65.
func runBackupVerify(args []string, stdout, stderr io.Writer) error {
	fs, _ := newFlagSet("backup verify", stderr)
	from := fs.String("from", "", "the archive to verify, or - for stdin")
	if err := parse(fs, args, stdout); err != nil {
		return err
	}
	if *from == "" {
		fmt.Fprintln(stderr, "dillad backup verify: --from is required")
		return exit.Usage
	}
	var r io.Reader
	if *from == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(*from)
		if err != nil {
			return fmt.Errorf("backup verify: %w: %w", err, exit.Data)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	man, err := ops.Verify(context.Background(), r)
	if err != nil {
		return fmt.Errorf("backup verify: %s: %w", *from, err)
	}
	fmt.Fprintf(stdout, "dillad backup verify: %s is intact (format %d, engine %s, schema %d, generation %d, created %s, %d members, %d blobs)\n",
		*from, man.FormatVersion, man.Engine, man.SchemaVersion, man.Generation, man.CreatedAt, len(man.Entries)+1, man.BlobCount)
	return nil
}
