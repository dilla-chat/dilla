package api_test

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

func TestHumanTimeoutRoutesRequireModeratorAndExpireAutomatically(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewMemberTimeouts(e.Repo, e.Clk).Register(e.Mux)
	target, targetTok := e.NewUser("timeout-target")
	joinCommunity(t, e, cid, targetTok)
	path := "/v1/communities/" + cid.String() + "/members/" + target.String() + "/timeout"
	requestID := id.New()
	until := uint64(e.Clk.Now().Add(time.Minute).Unix())
	if status, _ := e.Do(http.MethodPut, path, targetTok, []any{requestID, until}); status != http.StatusForbidden {
		t.Fatalf("member PUT timeout = %d, want forbidden", status)
	}
	if status, body := e.Do(http.MethodPut, path, ownerTok, []any{requestID, until}); status != http.StatusOK {
		t.Fatalf("owner PUT timeout = %d (%x), want 200", status, body)
	}
	if row, err := e.Repo.GetMemberTimeout(t.Context(), cid, target, e.Clk.Now().Unix()); err != nil || row.Expires != int64(until) {
		t.Fatalf("timeout row = %+v, %v", row, err)
	}
	if status, _ := e.Do(http.MethodGet, path, targetTok, nil); status != http.StatusOK {
		t.Fatalf("member GET own timeout = %d, want 200", status)
	}
	if status, _ := e.Do(http.MethodGet, path, ownerTok, nil); status != http.StatusOK {
		t.Fatalf("owner GET timeout = %d, want 200", status)
	}
	e.Clk.Advance(2 * time.Minute)
	if _, err := e.Repo.GetMemberTimeout(t.Context(), cid, target, e.Clk.Now().Unix()); err == nil {
		t.Fatal("expired timeout still active")
	}
	clearID := id.New()
	if status, body := e.Do(http.MethodDelete, path, ownerTok, []any{clearID}); status != http.StatusNoContent {
		t.Fatalf("owner DELETE timeout = %d (%x), want 204", status, body)
	}
	if status, body := e.Do(http.MethodDelete, path, ownerTok, []any{clearID}); status != http.StatusNoContent {
		t.Fatalf("retry after accepted DELETE = %d (%x), want 204", status, body)
	}
	if status, body := e.Do(http.MethodDelete, path, ownerTok, []any{id.New()}); status != http.StatusNoContent {
		t.Fatalf("DELETE when no timeout remains = %d (%x), want 204", status, body)
	}
}

func TestHumanTimeoutCannotTargetEqualOrHigherRole(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewMemberTimeouts(e.Repo, e.Clk).Register(e.Mux)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	moderator, modTok := e.NewUser("timeout-mod")
	joinCommunity(t, e, cid, modTok)
	target, targetTok := e.NewUser("timeout-peer")
	joinCommunity(t, e, cid, targetTok)
	// Both users receive the same highest role carrying moderate_members.
	role := createRole(t, e, cid, ownerTok, "peer-moderators", 5, api.PermModerateMembers)
	grant(t, e, cid, ownerTok, moderator, role, http.StatusNoContent)
	grant(t, e, cid, ownerTok, target, role, http.StatusNoContent)
	path := "/v1/communities/" + cid.String() + "/members/" + target.String() + "/timeout"
	if status, _ := e.Do(http.MethodPut, path, modTok, []any{id.New(), uint64(e.Clk.Now().Add(time.Minute).Unix())}); status != http.StatusForbidden {
		t.Fatalf("equal rank PUT timeout = %d, want forbidden", status)
	}
}

// The slash handler reconciles this authoritative expiry-only GET contract after
// losing a PUT response; the endpoint does not provide a replay receipt.
func TestTimeoutReadPreservesAcceptedDeadlineAndLaterModeration(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewMemberTimeouts(e.Repo, e.Clk).Register(e.Mux)
	target, targetTok := e.NewUser("timeout-reconcile")
	joinCommunity(t, e, cid, targetTok)
	path := "/v1/communities/" + cid.String() + "/members/" + target.String() + "/timeout"
	original := uint64(e.Clk.Now().Add(10 * time.Minute).Unix())
	request := id.New()
	// Discard the accepted PUT body, as if the caller lost its response.
	if status, _ := e.Do(http.MethodPut, path, ownerTok, []any{request, original}); status != http.StatusOK {
		t.Fatalf("PUT=%d", status)
	}
	e.Clk.Advance(5 * time.Minute)
	read := func(want uint64) {
		t.Helper()
		status, body := e.Do(http.MethodGet, path, ownerTok, nil)
		var row []any
		if status != http.StatusOK || cborx.Unmarshal(body, &row) != nil || len(row) != 1 || row[0] != want {
			t.Fatalf("GET status=%d row=%v want=%d", status, row, want)
		}
	}
	read(original)
	changed := uint64(e.Clk.Now().Add(15 * time.Minute).Unix())
	if status, _ := e.Do(http.MethodPut, path, ownerTok, []any{id.New(), changed}); status != http.StatusOK {
		t.Fatalf("later PUT=%d", status)
	}
	read(changed)
	if status, _ := e.Do(http.MethodDelete, path, ownerTok, []any{id.New()}); status != http.StatusNoContent {
		t.Fatalf("DELETE=%d", status)
	}
	status, body := e.Do(http.MethodGet, path, ownerTok, nil)
	var row []any
	if status != http.StatusOK || cborx.Unmarshal(body, &row) != nil || len(row) != 1 || row[0] != nil {
		t.Fatalf("removed GET status=%d row=%v", status, row)
	}
}
