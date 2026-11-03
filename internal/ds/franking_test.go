package ds

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

// protocol/04-envelope-and-franking.md: T = HMAC-SHA256(K_frank, "dilla frank tag v1" ||
// group_id || epoch(8 BE) || seq(8 BE) || uploader_device(16) || C || recv_ts(8 BE)). The
// committed vectors are the cross-language check; a Go-only constant would prove nothing.
func TestFrankingTagReproducesTheCommittedVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "protocol", "vectors", "franking.json"))
	if err != nil {
		t.Fatalf("franking.json: %v", err)
	}
	var v struct {
		InstanceFrankingKey string `json:"instance_franking_key"`
		Cases               []struct {
			GroupID        string `json:"group_id"`
			Epoch          uint64 `json:"epoch"`
			Seq            uint64 `json:"seq"`
			UploaderDevice string `json:"uploader_device"`
			Commitment     string `json:"commitment"`
			RecvTS         uint64 `json:"recv_ts"`
			Tag            string `json:"tag"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("franking.json: %v", err)
	}
	keyBytes, err := hex.DecodeString(v.InstanceFrankingKey)
	if err != nil || len(keyBytes) != 32 {
		t.Fatalf("instance_franking_key: %v (%d bytes)", err, len(keyBytes))
	}
	var key [32]byte
	copy(key[:], keyBytes)

	if len(v.Cases) < 3 {
		t.Fatalf("franking.json has %d cases, want at least 3", len(v.Cases))
	}
	for _, c := range v.Cases {
		groupID, err := id.Parse(c.GroupID)
		if err != nil {
			t.Fatalf("group_id: %v", err)
		}
		uploader, err := id.Parse(c.UploaderDevice)
		if err != nil {
			t.Fatalf("uploader_device: %v", err)
		}
		commitment, err := hex.DecodeString(c.Commitment)
		if err != nil {
			t.Fatalf("commitment: %v", err)
		}
		got := FrankingTag(key, groupID, c.Epoch, c.Seq, uploader, commitment, c.RecvTS)
		if hex.EncodeToString(got) != c.Tag {
			t.Errorf("seq %d: tag = %x, want %s", c.Seq, got, c.Tag)
		}
	}
}

// Every field is in the preimage: changing any one of them changes the tag. This is what makes a
// tag bind the message to its place in the stream rather than to its bytes alone.
func TestEveryPreimageFieldChangesTheTag(t *testing.T) {
	var key [32]byte
	group, uploader := id.New(), id.New()
	commitment := make([]byte, 32)
	base := FrankingTag(key, group, 1, 1, uploader, commitment, 1)

	other := id.New()
	for name, got := range map[string][]byte{
		"group_id":        FrankingTag(key, other, 1, 1, uploader, commitment, 1),
		"epoch":           FrankingTag(key, group, 2, 1, uploader, commitment, 1),
		"seq":             FrankingTag(key, group, 1, 2, uploader, commitment, 1),
		"uploader_device": FrankingTag(key, group, 1, 1, other, commitment, 1),
		"commitment":      FrankingTag(key, group, 1, 1, uploader, append([]byte{1}, commitment[1:]...), 1),
		"recv_ts":         FrankingTag(key, group, 1, 1, uploader, commitment, 2),
	} {
		if string(got) == string(base) {
			t.Errorf("changing %s did not change the tag", name)
		}
	}
}
