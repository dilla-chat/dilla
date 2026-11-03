package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

// C4 (fix wave), web-2a task 8: an accepted list publish is handed to AfterDeviceList, on a context
// the request does not end, with the devices it revoked; a refused one — a repeat of a version or a
// list that does not verify — is not.
func TestAnAcceptedDeviceListPublishIsHandedToAfterDeviceList(t *testing.T) {
	type run struct {
		user    id.ID
		revoked []id.ID
	}
	var seen []run
	h, deps := listAPI(t, func(ctx context.Context, u id.ID, revoked []id.ID) {
		if ctx.Err() != nil {
			t.Errorf("the hook's context has already ended: %v", ctx.Err())
		}
		seen = append(seen, run{u, revoked})
	})
	lu := seedListedUser(t, deps, 0x5a, 1)
	path := "/v1/users/" + lu.User.ID.String() + "/device-list"
	body1, blob1 := signedListPut(t, lu.SSK, lu.User.ID, 1, nil, []apiListEntry{entryOf(lu, 0, nil)})

	if rec := cborCall(t, h, http.MethodPut, path, lu.Tokens[0], body1); rec.Code != http.StatusNoContent {
		t.Fatalf("publish: status %d, want 204: %x", rec.Code, rec.Body.Bytes())
	}
	if rec := cborCall(t, h, http.MethodPut, path, lu.Tokens[0], body1); rec.Code != http.StatusConflict {
		t.Fatalf("republishing version 1: status %d, want 409", rec.Code)
	}
	forged, _ := signedListPut(t, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x77}, 32)), lu.User.ID, 2, blob1,
		[]apiListEntry{entryOf(lu, 0, nil)})
	if rec := cborCall(t, h, http.MethodPut, path, lu.Tokens[0], forged); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unverifiable list: status %d, want 400", rec.Code)
	}
	if len(seen) != 1 || seen[0].user != lu.User.ID || len(seen[0].revoked) != 0 {
		t.Fatalf("AfterDeviceList saw %v, want exactly one run for %s revoking nothing", seen, lu.User.ID)
	}
}
