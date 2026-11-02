package main

import (
	"testing"
)

// Runtime flags cannot replace compiled trust, roots, feeds or a program to launch.
func TestArgumentsPermitOnlyFixedEntryForms(t *testing.T) {
	for _, args := range [][]string{nil, {"--hidden"}, {"dilla://invite/example.com/ABC_1"}, {"--hidden", "dilla://invite/[::1]:8443/code"}, {"--describe"}, {"--state"}} {
		if _, e := parseArguments(args); e != nil {
			t.Fatalf("valid %q: %v", args, e)
		}
	}
	for _, args := range [][]string{{"--root=C:/evil"}, {"--public-key=evil"}, {"--update-bootstrap=" + string(make([]byte, 64))}, {"--hidden", "--hidden"}, {"--state", "--hidden"}, {"dilla://invite/example.com./abc"}, {"dilla://invite/example.com:99999/abc"}, {"dilla://invite/user@example.com/abc"}, {"dilla://invite/example.com/abc?x=1"}, {"dilla://invite/example.com/abc", "dilla://invite/example.com/def"}, {"dilla://invite/[bogus]/abc"}, {"https://example.com"}} {
		if _, e := parseArguments(args); e == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func TestInstallerRetryArgumentsHaveNoPathOrMutableAuthority(t *testing.T) {
	for input, mode := range map[string]string{"--installer-retry=initial": "installer-retry-initial", "--installer-preflight=initial-complete": "installer-initial-complete", "--installer-finalize=initial": "installer-finalize-initial"} {
		parsed, err := parseArguments([]string{input})
		if err != nil || parsed.mode != mode {
			t.Fatalf("fixed mode %q: %v %v", input, parsed, err)
		}
		if _, err = parseArguments([]string{input, "--path=C:\\foreign"}); err == nil {
			t.Fatal("accepted caller authority")
		}
	}
}
