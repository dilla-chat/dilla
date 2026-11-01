package permbits_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/jonasthim/dilla/internal/permbits"
)

func TestAllMatchesProtocolVectors(t *testing.T) {
	raw, err := os.ReadFile("../../protocol/vectors/permissions.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Bits map[string]uint64 `json:"bits"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	var all uint64
	for name, bit := range v.Bits {
		if bit == 0 || bit&(bit-1) != 0 {
			t.Fatalf("%s = %d is not a single bit", name, bit)
		}
		all |= bit
	}
	if all != permbits.All {
		t.Fatalf("permissions.json bits = %#x, permbits.All = %#x", all, permbits.All)
	}
	if v.Bits["control_music"] != permbits.ControlMusic || v.Bits["view_channel"] != permbits.ViewChannel {
		t.Fatalf("named bits drifted: %v", v.Bits)
	}
}
