package api_test

// Bot v2 task 20b: on wire 6 the unpaged GET /v1/users/{id}/device-list head carries the user's
// umk_pub, ssk_pub and sig_umk_ssk after the attestation, so a member who shares no group with the
// community's Bot can build the Bot's device_list_push from the head and the history pages.

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestAWire6DeviceListHeadCarriesTheUsersIdentityKeys(t *testing.T) {
	h, d := accountKindAPI(t)
	_, _, token := seedAPISession(t, d)
	negotiateWire(t, h, token, 6)
	bot := store.UserRow{ID: id.New(), Username: "bot" + id.New().String()[:8], Display: "Bot", Kind: 1,
		UMKPub: bytes.Repeat([]byte{0x11}, 32), SSKPub: bytes.Repeat([]byte{0x22}, 32), SigUMKSSK: bytes.Repeat([]byte{0x33}, 64),
		Created: d.Clock.Now().Unix()}
	if err := d.Repo.CreateUser(context.Background(), bot); err != nil {
		t.Fatal(err)
	}
	putList(t, d, bot.ID)

	got := head(t, h, token, bot.ID)
	if len(got) != 8 {
		t.Fatalf("wire-6 head has %d elements, want 8 [version, blob, ssk_signature, prev_hash, attestation, umk_pub, ssk_pub, sig_umk_ssk]", len(got))
	}
	row, err := d.Repo.GetUser(context.Background(), bot.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range [][]byte{row.UMKPub, row.SSKPub, row.SigUMKSSK} {
		if b, _ := got[5+i].([]byte); !bytes.Equal(b, want) {
			t.Fatalf("head element %d = %x, want the users row's %x", 5+i, b, want)
		}
	}
	// The attestation stays the fifth element; the chain row stays the first four.
	if _, ok := got[4].([]byte); !ok || got[0] != uint64(1) {
		t.Fatalf("head %v: want [1, …, attestation, …]", got)
	}
}

// The route's access rule is unchanged: an enrolled session reads any user's head (now with the
// keys), and a pending session reads only its own.
func TestTheIdentityKeysAreServedOnlyUnderTheExistingReadRule(t *testing.T) {
	h, d := accountKindAPI(t)
	owner, dev, _ := seedAPIDevice(t, d)
	listerOf(t, d).list(owner.ID, dev)
	putList(t, d, owner.ID)
	_, pending := pendingSession(t, h, d, owner.ID)
	other, _, otherToken := seedAPISession(t, d)
	negotiateWire(t, h, otherToken, 6)
	putList(t, d, other.ID)

	if got := head(t, h, otherToken, owner.ID); len(got) != 8 {
		t.Fatalf("an enrolled session reading another user's head: %d elements, want 8", len(got))
	}
	rec := doAuth(h, http.MethodGet, "/v1/users/"+other.ID.String()+"/device-list", pending)
	if rec.Code != http.StatusForbidden || refusalCode(t, rec) != "E_FORBIDDEN" {
		t.Fatalf("a pending session reading another user's head = %d, want 403 E_FORBIDDEN", rec.Code)
	}
	if rec := doAuth(h, http.MethodGet, "/v1/users/"+owner.ID.String()+"/device-list", pending); rec.Code != http.StatusOK {
		t.Fatalf("a pending session reading its own head = %d, want 200", rec.Code)
	}
}

// A session on an older wire (no longer negotiable) keeps the shape it always had.
func TestAnOlderWireHeadCarriesNoIdentityKeys(t *testing.T) {
	h, d := accountKindAPI(t)
	_, _, token := seedAPISession(t, d)
	if err := d.Repo.SetSessionVersions(context.Background(), auth.TokenHash(token), 5, 1, 1); err != nil {
		t.Fatal(err)
	}
	user, _, _ := seedAPISession(t, d)
	putList(t, d, user.ID)
	if got := head(t, h, token, user.ID); len(got) != 5 {
		t.Fatalf("wire-5 head has %d elements, want 5", len(got))
	}
}
