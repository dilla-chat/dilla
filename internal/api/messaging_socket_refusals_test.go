package api_test

// Bot v2 task 5 (review M3): the weaker callers are refused on the socket as they were over HTTP. Every request here
// goes through the socket dispatcher (the groupsAPI harness bridges the socket-only routes), with the real session
// middleware, rate meter and handlers.

import (
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

func TestSocketRefusesANonMemberOfAGroup(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	h.memberToken(t) // the group's leaf exists; the caller below is not it
	_, _, outsider := seedAPISession(t, h.deps)
	g := h.groupID.String()
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/groups/" + g + "/messages?from=0"},
		{http.MethodPost, "/v1/groups/" + g + "/commit"},
		{http.MethodPost, "/v1/groups/" + g + "/message"},
	} {
		body := []byte(nil)
		switch {
		case c.method == http.MethodPost && c.path[len(c.path)-6:] == "commit":
			body = mustCBOR(t, []any{h.groupEpoch(t), []byte{0}, []byte{0}, []any{}, nil})
		case c.method == http.MethodPost:
			body = mustCBOR(t, []any{h.groupEpoch(t), []byte{1}, uint64(0)})
		}
		res := h.do(t, c.method, c.path, outsider, body)
		if res.Code != http.StatusForbidden && res.Code != http.StatusNotFound {
			t.Errorf("%s %s by a non-member = %d, want 403 or 404: %s", c.method, c.path, res.Code, res.Body.String())
		}
	}
}

// memberUser is the account behind memberToken: leaf 0 of the committed fixture.
func (h *groupsAPI) memberUser(t *testing.T) id.ID {
	t.Helper()
	members, err := h.deps.Repo.ListMembers(t.Context(), h.groupID)
	if err != nil || len(members) == 0 {
		t.Fatalf("ListMembers: %v (%d)", err, len(members))
	}
	return members[0].UserID
}

func TestSocketRefusesABotOutsideItsAllowList(t *testing.T) {
	h := newGroupsAPI(t)
	token, _, _ := readyBotSession(t, h)
	// GET /v1/groups/{id}/heal-state is a delivery-service route the Bot allow-list does not name.
	res := h.do(t, http.MethodGet, "/v1/groups/"+id.New().String()+"/heal-state", token, nil)
	if res.Code != http.StatusForbidden {
		t.Fatalf("a Bot session on GET /v1/groups/{id}/heal-state = %d, want 403: %s", res.Code, res.Body.String())
	}
}

func TestSocketRefusesAnOwedFactorSession(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	member := h.memberToken(t)
	if res := h.do(t, http.MethodGet, "/v1/groups/"+h.groupID.String()+"/messages?from=0", member, nil); res.Code != http.StatusOK {
		t.Fatalf("control read = %d, want 200: %s", res.Code, res.Body.String())
	}
	if err := h.deps.Repo.SetFactorOwed(t.Context(), h.memberUser(t), true); err != nil {
		t.Fatal(err)
	}
	res := h.do(t, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/message", member, h.uploadBody(t, 64))
	if res.Code != http.StatusForbidden || errorCode(t, res) != "E_FACTOR_REQUIRED" {
		t.Fatalf("a send by an account that owes a factor = %d %s, want 403 E_FACTOR_REQUIRED", res.Code, res.Body.String())
	}
}

func TestSocketRefusesASecondConcurrentCommitFromOneDevice(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	member := h.memberToken(t)
	release, err := h.groups.BeginCommitForTest(h.memberDevice(t))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	body := mustCBOR(t, []any{h.groupEpoch(t), []byte{0}, []byte{0}, []any{}, nil})
	res := h.do(t, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/commit", member, body)
	if res.Code != http.StatusTooManyRequests || errorCode(t, res) != "E_RATE_LIMITED" {
		t.Fatalf("a second commit while one runs = %d %s, want 429 E_RATE_LIMITED", res.Code, res.Body.String())
	}
}
