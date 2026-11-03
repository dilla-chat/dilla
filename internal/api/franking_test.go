package api_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
)

// frankingVectors is protocol/vectors/franking.json: one envelope, its
// commitment, and three tags under a fixed instance key.
type frankingVectors struct {
	InstanceFrankingKey string `json:"instance_franking_key"`
	EnvelopeCBOR        string `json:"envelope_cbor"`
	Cases               []struct {
		GroupID        string `json:"group_id"`
		Epoch          uint64 `json:"epoch"`
		Seq            uint64 `json:"seq"`
		UploaderDevice string `json:"uploader_device"`
		Commitment     string `json:"commitment"`
		RecvTS         int64  `json:"recv_ts"`
		Tag            string `json:"tag"`
	} `json:"cases"`
}

func readVectors(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "protocol", "vectors", name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

// The readable path computes protocol/04's real C from the envelope and the k_f
// it carries in the clear, so it must reproduce the committed vectors exactly: a
// report against a readable message is verified by the same equation.
func TestCommitmentReproducesTheCommittedVectors(t *testing.T) {
	var env struct {
		Cases []struct {
			Name       string `json:"name"`
			CBOR       string `json:"cbor"`
			Commitment string `json:"commitment"`
			Envelope   struct {
				KF string `json:"kf"`
			} `json:"envelope"`
		} `json:"cases"`
	}
	readVectors(t, "envelope.json", &env)
	if len(env.Cases) == 0 {
		t.Fatal("envelope.json has no cases")
	}
	for _, c := range env.Cases {
		got, err := api.Commitment(mustHex(t, c.CBOR), mustHex(t, c.Envelope.KF))
		if err != nil {
			t.Fatalf("%s: Commitment: %v", c.Name, err)
		}
		if hex.EncodeToString(got) != c.Commitment {
			t.Fatalf("%s: commitment %x, want %s", c.Name, got, c.Commitment)
		}
	}

	var fr frankingVectors
	readVectors(t, "franking.json", &fr)
	got, err := api.Commitment(mustHex(t, fr.EnvelopeCBOR), bytes.Repeat([]byte{0x06}, 32))
	if err != nil {
		t.Fatalf("franking.json envelope: %v", err)
	}
	if hex.EncodeToString(got) != fr.Cases[0].Commitment {
		t.Fatalf("franking.json commitment %x, want %s", got, fr.Cases[0].Commitment)
	}
}

func TestCommitmentRefusesAMalformedEnvelope(t *testing.T) {
	for name, b := range map[string][]byte{
		"not CBOR":        {0xff},
		"eight elements":  {0x88, 0, 0, 0, 0, 0, 0, 0, 0},
		"trailing byte":   append(mustHex(t, "8901500101010101010101010101010101010100f6f660808040"), 0x00),
		"not an array":    {0x01},
		"indefinite list": {0x9f, 0xff},
	} {
		if _, err := api.Commitment(b, make([]byte, 32)); err == nil {
			t.Errorf("%s: Commitment accepted it", name)
		}
	}
}

// Tag reproduces the committed vectors and is, for a 32-byte key, the delivery
// service's own FrankingTag: one T in dilla, two call sites (follow-up card 7).
func TestTagIsTheDeliveryServicesTag(t *testing.T) {
	var fr frankingVectors
	readVectors(t, "franking.json", &fr)
	key := mustHex(t, fr.InstanceFrankingKey)
	if len(fr.Cases) < 3 {
		t.Fatalf("franking.json has %d cases", len(fr.Cases))
	}
	for _, c := range fr.Cases {
		group, err := id.Parse(c.GroupID)
		if err != nil {
			t.Fatalf("group_id: %v", err)
		}
		uploader, err := id.Parse(c.UploaderDevice)
		if err != nil {
			t.Fatalf("uploader_device: %v", err)
		}
		commitment := mustHex(t, c.Commitment)
		got := api.Tag(key, group, c.Epoch, c.Seq, uploader, commitment, c.RecvTS)
		if hex.EncodeToString(got) != c.Tag {
			t.Fatalf("seq %d: tag %x, want %s", c.Seq, got, c.Tag)
		}
		want := ds.FrankingTag([32]byte(key), group, c.Epoch, c.Seq, uploader, commitment, uint64(c.RecvTS))
		if !bytes.Equal(got, want) {
			t.Fatalf("seq %d: api.Tag and ds.FrankingTag disagree", c.Seq)
		}
	}
}

func TestStaticFrankingKeys(t *testing.T) {
	cur := api.FrankingKey{ID: id.New(), Key: bytes.Repeat([]byte{1}, 32)}
	old := api.FrankingKey{ID: id.New(), Key: bytes.Repeat([]byte{2}, 32)}
	k := api.NewStaticFrankingKeys(cur, old)
	if gotID, gotKey := k.Current(); gotID != cur.ID || !bytes.Equal(gotKey, cur.Key) {
		t.Fatal("Current is not the first key")
	}
	if key, ok := k.ByID(old.ID); !ok || !bytes.Equal(key, old.Key) {
		t.Fatal("ByID did not find the retained key")
	}
	if _, ok := k.ByID(id.New()); ok {
		t.Fatal("ByID found an unknown key")
	}
	if all := k.All(); len(all) != 2 || all[0].ID != cur.ID || all[1].ID != old.ID {
		t.Fatalf("All = %v, want current then retained", all)
	}
}
