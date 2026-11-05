// Command dillad is the dilla server.
//
// The command line is a verb dispatcher with eight names, every one implemented.
// admin is itself a dispatcher, noun then verb (admin.go).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/dilla-chat/dilla/internal/exit"
)

// Version is the release's tag without its leading v, stamped at link time by
// release.yml (-ldflags "-X main.Version=<version>"). Every other build, server.yml's
// included, keeps 0.0.0-dev. It is a variable because the linker sets only a
// package-level string variable; it ignores a constant without a word.
var Version = "0.0.0-dev"

type verb struct {
	name    string
	summary string
	run     func(args []string, stdout, stderr io.Writer) error
}

func verbs() map[string]verb {
	return map[string]verb{
		"serve":   {"serve", "run the instance", runServe},
		"media":   {"media", "run the sidecar SFU", runMedia},
		"netns":   {"netns", "hold the Compose network namespace", runNetns},
		"init":    {"init", "create the data directory, config, database and bootstrap invite", runInit},
		"migrate": {"migrate", "apply or inspect schema migrations", runMigrate},
		"doctor":  {"doctor", "check configuration, database, wasi artifact and clock", runDoctor},
		"version": {"version", "print the version, VCS revision and cgo status", runVersion},
		"backup":  {"backup", "write a backup archive, or check one with `backup verify`", runBackup},
		"restore": {"restore", "replace the instance with a backup archive, and arm the group heal", runRestore},
		"admin":   {"admin", "user, invite, community, device, blob and audit commands", runAdmin},
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: dillad <verb> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "verbs:")
	all := verbs()
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "  %-8s %s\n", name, all[name].summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "every verb accepts --config <path> (default /etc/dilla/dilla.toml)")
}

// dispatch runs one verb. args excludes argv[0]. A nil return is exit 0.
func dispatch(args []string, stdout, stderr io.Writer) error {
	restrictUmask() // R17: first thing, so no file any verb creates depends on the caller's umask
	if len(args) == 0 {
		usage(stdout)
		return exit.Usage
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(stdout)
		return nil
	}
	v, ok := verbs()[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "dillad: unknown verb %q\n", args[0])
		usage(stderr)
		return exit.Usage
	}
	if err := v.run(args[1:], stdout, stderr); !errors.Is(err, errHelp) {
		return err
	}
	// The verb printed its usage for --help and did nothing else: exit 0.
	return nil
}

func main() {
	err := dispatch(os.Args[1:], os.Stdout, os.Stderr)
	var code exit.Code
	if err != nil && !errors.As(err, &code) {
		err = fmt.Errorf("%w: %w", err, exit.Fail)
	}
	exit.Exit(err)
}
