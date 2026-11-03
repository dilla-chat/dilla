package dilladtest

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// satisfyFactor clears a seeded account's owed second factor (web3.6 S1) through the repository,
// for a test about something else. A seeded human owes one exactly as a real sign-up does, and
// neither the control listener nor SeedRequest offers a way round that: this test-file helper is
// the only one.
func satisfyFactor(t *testing.T, h *Host, user string) {
	t.Helper()
	uid, err := id.Parse(user)
	if err != nil {
		t.Fatalf("seeded user id %q: %v", user, err)
	}
	if err := h.Server().Repo().SetFactorOwed(context.Background(), uid, false); err != nil {
		t.Fatalf("SetFactorOwed(%s, false): %v", uid, err)
	}
}

// seedOne writes the owed flag as POST /v1/accounts does: a seeded human owes a second factor, a
// seeded bot does not (only human accounts sign in with one).
func TestASeededHumanOwesASecondFactorAndABotDoesNot(t *testing.T) {
	h := sfuHost(t, false)
	ctx := t.Context()
	zero := func(n int) string { return hex.EncodeToString(make([]byte, n)) }
	req := func(name string) SeedRequest {
		return SeedRequest{Username: name, Display: name, UMKPub: zero(32), SSKPub: zero(32),
			SigUMKSSK: zero(64), Devices: []Device{{DeviceID: id.New().String(), DSKPub: zero(32), Credential: "01"}}}
	}
	out, err := SeedUsers(ctx, h.Server(), []SeedRequest{req("human")})
	if err != nil {
		t.Fatalf("SeedUsers: %v", err)
	}
	botID := id.New()
	if err := h.Server().Repo().CreateUser(ctx, store.UserRow{
		ID: botID, Username: "bot", Display: "bot", Kind: 1,
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64),
		Created: h.Server().Now().Unix(),
	}); err != nil {
		t.Fatalf("create bot account: %v", err)
	}

	for _, tc := range []struct {
		name string
		id   id.ID
		want bool
	}{{"human", mustID(t, out[0].UserID), true}, {"bot", botID, false}} {
		u, err := h.Server().Repo().GetUser(ctx, tc.id)
		if err != nil {
			t.Fatalf("GetUser(%s): %v", tc.name, err)
		}
		if u.FactorOwed != tc.want {
			t.Errorf("seeded %s: factor_owed = %v, want %v", u.Username, u.FactorOwed, tc.want)
		}
	}
}

func mustID(t *testing.T, value string) id.ID {
	t.Helper()
	parsed, err := id.Parse(value)
	if err != nil {
		t.Fatalf("user id %q: %v", value, err)
	}
	return parsed
}
