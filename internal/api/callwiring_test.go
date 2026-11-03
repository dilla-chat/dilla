package api_test

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/blob"
)

// Task 10 re-review n2: a visibility PATCH re-derives who may be in the channel and re-syncs its
// live call after the commit, so a participant who no longer has the channel in view is cut from
// the call's room, and the owner, who still does, is not. The view is taken from the member behind
// the handler's back, so only the PATCH can notice.
func TestAVisibilityPatchCutsAParticipantWhoLosesView(t *testing.T) {
	m := newMembershipCallEnv(t)
	ownerDev := deviceOf(t, m.e, m.ownerTok)
	denyInChannel(t, m.e, m.ch, m.member, api.PermViewChannel)
	if removedDevice(m.stub, m.room, m.memberDev) {
		t.Fatal("the member was cut before the PATCH")
	}
	if status, body := m.e.Do(http.MethodPatch, "/v1/channels/"+m.ch.String(), m.ownerTok,
		[]any{nil, nil, nil, uint64(api.VisPrivate), nil, nil, nil}); status != http.StatusNoContent {
		t.Fatalf("PATCH visibility = %d (%x)", status, body)
	}
	if !waitRemoved(m.stub, m.room, m.memberDev) { // queued to the retry loop (parked item)
		t.Fatal("the visibility PATCH left a participant who lost view in the call's room")
	}
	if removedDevice(m.stub, m.room, ownerDev) {
		t.Fatal("the visibility PATCH cut the owner, who still has the channel in view")
	}
}

// Task 10 re-review n2: the admin disable route queues the user's devices for the call cut, and the
// retry loop, woken at once, removes the device from the live call's room. The owner stays.
func TestTheAdminDisableRouteCutsTheUsersDeviceFromALiveCall(t *testing.T) {
	b := newBarredEnv(t)
	bs, err := blob.Open(t.TempDir(), "fs")
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	api.NewAdmin(b.e.Repo, bs, b.e.Clk, slog.New(slog.DiscardHandler)).WithCalls(b.calls).Register(b.e.Mux)
	adminTok := b.e.NewInstanceAdmin("root")
	stop := b.calls.StartRetries(time.Hour)
	defer stop()

	if status, body := b.e.Do(http.MethodPost, "/v1/admin/users/"+b.member.String()+"/disable", adminTok,
		[]any{uint64(1)}); status != http.StatusNoContent {
		t.Fatalf("disable = %d (%x)", status, body)
	}
	if !waitRemoved(b.stub, b.room, b.memberDev) {
		t.Fatal("the admin disable did not cut the user's device from the live call")
	}
	if removedDevice(b.stub, b.room, b.ownerDev) {
		t.Fatal("the admin disable cut the owner too")
	}
}
