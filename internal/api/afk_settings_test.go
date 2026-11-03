package api_test

import (
	"net/http"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/id"
)

// web3.9: the settings take a timeout only; the channel is the built-in one, which a manager reads
// and cannot change, and which can no longer be deleted out from under the pointer.
func TestAFKSettingsRequireManagerAndNameTheBuiltInChannel(t *testing.T) {
	r := newRefusalEnv(t)
	path := "/v1/communities/" + r.cid.String() + "/afk/settings"
	if code, _ := r.e.Do(http.MethodPut, path, r.memberTok, []any{uint64(60)}); code != http.StatusForbidden {
		t.Fatalf("member PUT = %d", code)
	}
	elsewhere := createCommunity(t, r.e, r.ownerTok)
	foreign, _ := afkPointer(t, r.e, elsewhere)
	if code, _ := r.e.Do(http.MethodPut, path, r.ownerTok, []any{foreign[:], uint64(60)}); code != http.StatusBadRequest {
		t.Fatalf("another community's AFK channel PUT = %d", code)
	}
	if code, body := r.e.Do(http.MethodPut, path, r.ownerTok, []any{uint64(60)}); code != http.StatusNoContent {
		t.Fatalf("owner PUT = %d: %x", code, body)
	}
	if code, _ := r.e.Do(http.MethodGet, path, r.memberTok, nil); code != http.StatusForbidden {
		t.Fatalf("member GET = %d", code)
	}
	code, body := r.e.Do(http.MethodGet, path, r.ownerTok, nil)
	if code != http.StatusOK {
		t.Fatalf("manager GET = %d: %x", code, body)
	}
	var got []cbor.RawMessage
	mustUnmarshal(t, body, &got)
	var channel id.ID
	var timeout uint64
	mustUnmarshal(t, got[0], &channel)
	mustUnmarshal(t, got[1], &timeout)
	if channel != r.afk || timeout != 60 {
		t.Fatalf("settings = %v, %d", channel, timeout)
	}
	if code, _ := r.e.Do(http.MethodDelete, "/v1/channels/"+r.afk.String(), r.ownerTok, nil); code != http.StatusForbidden {
		t.Fatalf("delete AFK channel = %d, want 403", code)
	}
	code, body = r.e.Do(http.MethodGet, path, r.ownerTok, nil)
	if code != http.StatusOK {
		t.Fatalf("settings after the refused delete = %d: %x", code, body)
	}
	mustUnmarshal(t, body, &got)
	mustUnmarshal(t, got[0], &channel)
	if channel != r.afk {
		t.Fatalf("the destination moved to %s", channel)
	}
}
