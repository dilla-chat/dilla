package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/dilla-chat/dilla/internal/exit"
)

// newFlagSet registers the flags every verb shares. --config is registered on
// EVERY subcommand's FlagSet, not only globally: a flag defined only on the
// global set is not reached by `dillad serve --config=X` (gap-82 verdict 7).
func newFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("dillad "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := fs.String("config", "/etc/dilla/dilla.toml", "path to dilla.toml")
	return fs, cfg
}

// errHelp is what parse returns once it has answered --help: the verb's caller returns it at
// once, without running the verb, and dispatch turns it into exit 0.
var errHelp = errors.New("help requested")

// parse runs fs.Parse and converts its outcomes into dillad's exit codes.
// flag.ContinueOnError writes the message and the usage block itself, to the
// FlagSet's output, so nothing here prints them a second time (gap-82 verdict
// 8). A request for help is answered on stdout and only there: the output is
// pointed at stdout before parsing when one is present, and Parse's ErrHelp
// becomes errHelp so the verb body never runs (review I15).
func parse(fs *flag.FlagSet, args []string, stdout io.Writer) error {
	if wantsHelp(args) {
		fs.SetOutput(stdout)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelp
		}
		return fmt.Errorf("%s: %w", fs.Name(), exit.Usage)
	}
	return nil
}

// wantsHelp reports whether args ask for help before flag parsing stops at "--".
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		name, _, _ := strings.Cut(a, "=")
		if isHelpFlag(name) {
			return true
		}
	}
	return false
}

func runVersion(args []string, stdout, stderr io.Writer) error {
	fs, _ := newFlagSet("version", stderr)
	if err := parse(fs, args, stdout); err != nil {
		return err
	}
	revision, cgo := "unknown", "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "CGO_ENABLED":
				cgo = s.Value
			}
		}
	}
	fmt.Fprintf(stdout, "dillad %s (%s %s/%s) revision=%s CGO_ENABLED=%s\n",
		Version, runtime.Version(), runtime.GOOS, runtime.GOARCH, revision, cgo)
	return nil
}
