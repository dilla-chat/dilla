package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestBotWarningReceiptIsBoundToItsReadyInstallationAndExactHumanAction(t *testing.T) {
	f := newBotModerationFixture(t, 0)
	request := id.New()
	path := "/v1/bot-runtime/installations/" + f.installation.String() + "/moderation/warnings/" + request.String()
	if got := doCBORAuth(f.h, http.MethodGet, path, nil, f.token); got.Code != http.StatusNotFound {
		t.Fatalf("missing warning receipt = %d: %s", got.Code, got.Body.String())
	}
	actor, actorDevice, target, action := f.bot, f.device, f.target, id.New()
	row := store.ManualWarningActionRow{CommunityID: f.community, RequestID: request, ActionID: action,
		ActorUserID: actor, ActorDeviceID: actorDevice, TargetUserID: target, Revision: 13, AcceptedAt: f.deps.Clock.Now().Unix()}
	if err := f.deps.Repo.Tx(context.Background(), func(tx store.Repository) error { return tx.PutManualWarningAction(context.Background(), row) }); err != nil {
		t.Fatal(err)
	}
	got := doCBORAuth(f.h, http.MethodGet, path, nil, f.token)
	if got.Code != http.StatusOK {
		t.Fatalf("accepted warning receipt = %d: %s", got.Code, got.Body.String())
	}
	var wire []any
	if err := cborx.Unmarshal(got.Body.Bytes(), &wire); err != nil || len(wire) != 8 {
		t.Fatalf("receipt wire = %#v, %v", wire, err)
	}
	if string(wire[0].([]byte)) != string(request[:]) || string(wire[1].([]byte)) != string(action[:]) ||
		string(wire[2].([]byte)) != string(f.community[:]) || string(wire[3].([]byte)) != string(actor[:]) ||
		string(wire[4].([]byte)) != string(actorDevice[:]) || string(wire[5].([]byte)) != string(target[:]) || wire[6] != uint64(13) {
		t.Fatalf("receipt fields = %#v", wire)
	}
	wrong := "/v1/bot-runtime/installations/" + id.New().String() + "/moderation/warnings/" + request.String()
	if got := doCBORAuth(f.h, http.MethodGet, wrong, nil, f.token); got.Code != http.StatusForbidden {
		t.Fatalf("foreign installation = %d", got.Code)
	}
}
