package main

import (
	"slices"
	"strings"
	"testing"
)

// web3.8 S1: -wire is the one wire version the host's hello offers and GET /v1/instance
// advertises. Its default is the current wire version; E2's version-reload spec passes another.
func TestTheWireFlagReachesTheHostOptions(t *testing.T) {
	o, err := parseFlags([]string{"-wire", "3"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if ho := o.hostOptions("/data", "/core.wasm", ""); !slices.Equal(ho.WireVersions, []uint64{3}) {
		t.Fatalf("WireVersions = %v, want [3]", ho.WireVersions)
	}
	// An explicit legacy version remains available for compatibility tests.
	o, err = parseFlags([]string{"-wire", "2"})
	if err != nil {
		t.Fatalf("parseFlags(-wire 2): %v", err)
	}
	if ho := o.hostOptions("/data", "/core.wasm", ""); !slices.Equal(ho.WireVersions, []uint64{2}) {
		t.Fatalf("explicit legacy WireVersions = %v, want [2]", ho.WireVersions)
	}
	// Control: the flag left out keeps dillad's production list (nil here; wire [4,3,2] in the
	// host), so people negotiate wire 4 and managed Bots wire 3 (Task S1 part 2).
	o, err = parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if ho := o.hostOptions("/data", "/core.wasm", ""); ho.WireVersions != nil {
		t.Fatalf("WireVersions = %v, want nil (dillad's default list)", ho.WireVersions)
	}
	if _, err := parseFlags([]string{"-wire", "0"}); err == nil {
		t.Fatal("an explicit -wire 0 was accepted")
	}
}

// S1 fix round 1 (m3): wire versions start at 1, so -wire 0 is refused rather than a hello [0].
// The control is -wire 1, the lowest version, which is accepted.
func TestTheWireFlagRefusesZero(t *testing.T) {
	if _, err := parseFlags([]string{"-wire", "0"}); err == nil || !strings.Contains(err.Error(), "-wire") {
		t.Fatalf("parseFlags(-wire 0) = %v, want an error naming -wire", err)
	}
	o, err := parseFlags([]string{"-wire", "1"})
	if err != nil {
		t.Fatalf("control: parseFlags(-wire 1): %v", err)
	}
	if ho := o.hostOptions("/data", "/core.wasm", ""); !slices.Equal(ho.WireVersions, []uint64{1}) {
		t.Fatalf("control: WireVersions = %v, want [1]", ho.WireVersions)
	}
}
