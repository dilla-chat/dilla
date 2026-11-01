package api_test

import (
	"net/http"
	"testing"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/id"
)

// Task 8c fix round 2: a device's RTT reaches its own dilla.rtt attribute only through dillad, validated, rate-limited and
// only for a current leaf of the call.
func TestCallRTTSetsOnlyTheCallersOwnAttribute(t *testing.T) {
	e, stub, callID, tok, _ := shareEnv(t, 10, 0)
	path := "/v1/calls/" + callID.String() + "/rtt"
	dev := deviceOf(t, e, tok)
	if status, body := e.Do(http.MethodPost, path, tok, []any{24}); status != http.StatusNoContent {
		t.Fatalf("rtt 24 = %d %s", status, e.ErrCode(body))
	}
	got := stub.attrUpdates()
	if len(got) != 1 || got[0].Identity != dev.String() || len(got[0].Attrs) != 1 || got[0].Attrs["dilla.rtt"] != "24" {
		t.Fatalf("attribute updates = %+v, want one dilla.rtt=24 for the caller's own device", got)
	}

	// Rate limit: a second value within RTTEvery is 429 with a wait; after it, accepted.
	status, body := e.Do(http.MethodPost, path, tok, []any{30})
	if status != http.StatusTooManyRequests || e.ErrCode(body) != "E_RATE_LIMITED" {
		t.Fatalf("a second report at once = %d %s, want 429 E_RATE_LIMITED", status, e.ErrCode(body))
	}
	e.Clk.Advance(api.RTTEvery)
	if status, _ := e.Do(http.MethodPost, path, tok, []any{30}); status != http.StatusNoContent {
		t.Fatalf("a report after RTTEvery = %d", status)
	}

	// A clear goes at once (a pause must not leave a stale ping) and a second clear writes nothing.
	if status, _ := e.Do(http.MethodPost, path, tok, []any{}); status != http.StatusNoContent {
		t.Fatalf("clear = %d", status)
	}
	if status, _ := e.Do(http.MethodPost, path, tok, []any{}); status != http.StatusNoContent {
		t.Fatalf("second clear = %d", status)
	}
	got = stub.attrUpdates()
	if len(got) != 3 || got[2].Attrs["dilla.rtt"] != "" {
		t.Fatalf("attribute updates = %+v, want 24, 30, then one clear", got)
	}
}

func TestCallRTTValidatesTheValue(t *testing.T) {
	e, stub, callID, tok, _ := shareEnv(t, 10, 0)
	path := "/v1/calls/" + callID.String() + "/rtt"
	for name, body := range map[string][]any{
		"over 5000":    {5001},
		"negative":     {-1},
		"a fraction":   {2.5},
		"a text":       {"24"},
		"two elements": {24, 25},
	} {
		if status, out := e.Do(http.MethodPost, path, tok, body); status != http.StatusBadRequest || e.ErrCode(out) != "E_INVALID_REQUEST" {
			t.Errorf("%s: %d %s, want 400 E_INVALID_REQUEST", name, status, e.ErrCode(out))
		}
	}
	if status, _ := e.Do(http.MethodPost, path, tok, []any{5000}); status != http.StatusNoContent {
		t.Errorf("5000 = %d, want 204", status)
	}
	if n := len(stub.attrUpdates()); n != 1 {
		t.Errorf("%d attribute updates, want only the valid one", n)
	}
}

func TestCallRTTRefusesNonMembers(t *testing.T) {
	e, stub, callID, _, devs := shareEnv(t, 10, 1)
	path := "/v1/calls/" + callID.String() + "/rtt"
	// A device of another user: not in the channel's audience nor a leaf of the call group.
	_, stranger := e.NewUser("stranger-rtt")
	if status, _ := e.Do(http.MethodPost, path, stranger, []any{24}); status != http.StatusNotFound && status != http.StatusForbidden {
		t.Errorf("a stranger = %d, want 404 or 403", status)
	}
	// A device of the owner that is not a current leaf of the call group.
	notLeaf := seedDevices(t, e, userOf(t, e, "dev0"), 1)[0]
	e.sess["notleaf"] = auth.Session{UserID: userOf(t, e, "dev0"), DeviceID: notLeaf, Scope: auth.ScopeEnrolled}
	if status, out := e.Do(http.MethodPost, path, "notleaf", []any{24}); status == http.StatusNoContent {
		t.Errorf("a device that is not a leaf of the call = 204 (%s)", e.ErrCode(out))
	}
	// A leaf the SFU room does not hold: 404.
	stub.mu.Lock()
	if stub.absent == nil {
		stub.absent = map[string]bool{}
	}
	stub.absent[devs[0].String()] = true
	stub.mu.Unlock()
	if status, _ := e.Do(http.MethodPost, path, "dev0", []any{24}); status != http.StatusNotFound {
		t.Errorf("a device the room does not hold = %d, want 404", status)
	}
	if n := len(stub.attrUpdates()); n != 0 {
		t.Errorf("%d attribute updates, want none", n)
	}
	// An unknown call.
	if status, _ := e.Do(http.MethodPost, "/v1/calls/"+id.New().String()+"/rtt", "dev0", []any{24}); status != http.StatusNotFound {
		t.Errorf("an unknown call = %d, want 404", status)
	}
}

// No call token may update its own name, metadata or attributes (the attribute flood the security review found): every
// minted permission, human and bot, lacks CanUpdateMetadata.
func TestCallTokensCarryNoOwnMetadataRight(t *testing.T) {
	e, ch, ownerToken, group, stub, _ := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL, MaxPublishers: 1})
	seedLeaf(t, e, group, deviceOf(t, e, ownerToken), 3, nil)
	if status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", ownerToken, []any{}); status != http.StatusCreated {
		t.Fatalf("start = %d %s", status, e.ErrCode(body))
	}
	if p := stub.lastMint().Perm; p == nil || p.GetCanUpdateMetadata() {
		t.Fatalf("a human call token's permission = %v, want no own metadata", p)
	}
	_, _, botToken := readyCallBot(t, e, ch, group, ownerToken)
	if status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", botToken, []any{}); status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("bot join = %d %s", status, e.ErrCode(body))
	}
	if p := stub.lastMint().Perm; p == nil || p.GetCanUpdateMetadata() {
		t.Fatalf("a bot call token's permission = %v, want no own metadata", p)
	}
	for _, u := range stub.permUpdates() {
		if u.Perm.GetCanUpdateMetadata() {
			t.Fatalf("a pushed permission grants own metadata: %v", u.Perm)
		}
	}
}
