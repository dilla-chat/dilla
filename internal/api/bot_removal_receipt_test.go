package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestBotRemovalReceiptRequiresAcceptedActionAndCurrentCapability(t *testing.T) {
	f := newBotModerationFixture(t, 0)
	request := id.New()
	path := "/v1/bot-runtime/installations/" + f.installation.String() + "/moderation/removals/" + request.String()
	if got := doCBORAuth(f.h, http.MethodGet, path, nil, f.token); got.Code != http.StatusForbidden {
		t.Fatalf("without removal capability = %d: %s", got.Code, got.Body.String())
	}
	ctx := context.Background()
	if err := f.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		row, err := tx.GetBotInstallation(ctx, f.installation)
		if err != nil {
			return err
		}
		row.Capabilities |= 1 << 5
		row.Revision++
		return tx.UpdateBotInstallation(ctx, row.ID, row.Revision-1, row)
	}); err != nil {
		t.Fatal(err)
	}
	if got := doCBORAuth(f.h, http.MethodGet, path, nil, f.token); got.Code != http.StatusNotFound {
		t.Fatalf("unaccepted request = %d: %s", got.Code, got.Body.String())
	}
	actor, actorDevice, action := id.New(), id.New(), id.New()
	proof, err := cborx.Marshal([]any{uint64(1), request, actor, actorDevice, f.community, f.group, uint64(1), uint64(f.deps.Clock.Now().Unix() + 30)})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		_, err := tx.AcceptModerationRemoval(ctx, store.ModerationRemovalRow{RequestID: request, ActionID: action,
			CommunityID: f.community, GroupID: f.group, TargetSequence: 1, Proof: proof, Signature: make([]byte, 64), AcceptedAt: f.deps.Clock.Now().Unix()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := doCBORAuth(f.h, http.MethodGet, path, nil, f.token)
	if got.Code != http.StatusOK {
		t.Fatalf("accepted receipt = %d: %s", got.Code, got.Body.String())
	}
	var wire []any
	if err := cborx.Unmarshal(got.Body.Bytes(), &wire); err != nil || len(wire) != 8 {
		t.Fatalf("receipt wire = %v, %v", wire, err)
	}
	wrong := "/v1/bot-runtime/installations/" + id.New().String() + "/moderation/removals/" + request.String()
	if got := doCBORAuth(f.h, http.MethodGet, wrong, nil, f.token); got.Code != http.StatusForbidden {
		t.Fatalf("foreign installation = %d", got.Code)
	}
}
