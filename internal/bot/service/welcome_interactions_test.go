package service

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/community/welcome"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestWelcomePreviewRequiresCurrentManagerAndOnlyRendersText(t *testing.T) {
	h, auth := announcementInteractionsFixture(t)
	preview := WelcomeInteractions{Authority: auth, CommunityID: h.CommunityID}
	actor := InteractionActor{User: interactionID(6)}
	request := protocol.Request{RequestID: interactionID(30), CommunityID: h.CommunityID, Operation: protocol.OpWelcomePreview, Args: []any{"Hi {member} from {server} in {channel}"}}
	result, err := preview.Handlers()[protocol.OpWelcomePreview].Handle(context.Background(), actor, request)
	if err != nil || result.Status != 0 || auth.user != actor.User {
		t.Fatalf("preview result=%+v auth=%v err=%v", result, auth.user, err)
	}
	var fields []any
	if err := cborx.Unmarshal(result.Data, &fields); err != nil || len(fields) != 2 || fields[0] != uint64(1) || fields[1] != "Hi member from server in channel" {
		t.Fatalf("preview data=%x fields=%#v err=%v", result.Data, fields, err)
	}
	auth.allowed = false
	result, err = preview.Handlers()[protocol.OpWelcomePreview].Handle(context.Background(), actor, request)
	if err != nil || result.Status != interactionStatusForbidden || len(result.Data) != 0 {
		t.Fatalf("revoked preview result=%+v err=%v", result, err)
	}
	request.CommunityID = interactionID(99)
	result, err = preview.Handlers()[protocol.OpWelcomePreview].Handle(context.Background(), actor, request)
	if err != nil || result.Status != interactionStatusForbidden || len(result.Data) != 0 {
		t.Fatalf("foreign preview result=%+v err=%v", result, err)
	}
}

type welcomeSettingsNative struct {
	binding   native.Binding
	state     native.StateValue
	changes   []native.StateChange
	writeErr  error
	receipts  map[id.ID]native.StateChange
	revisions map[id.ID]uint64
}

func (n *welcomeSettingsNative) Binding() native.Binding { return n.binding }
func (n *welcomeSettingsNative) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	if area != 8 || string(key) != "dilla.bot.welcome.config.v1" {
		return native.StateValue{}, errors.New("unexpected welcome state key")
	}
	return n.state, nil
}
func (n *welcomeSettingsNative) StateTransaction(_ context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	n.changes = append(n.changes, changes...)
	if n.writeErr != nil {
		return nil, n.writeErr
	}
	if prior, ok := n.receipts[tx]; ok {
		if len(changes) != 1 || prior.Area != changes[0].Area || prior.ExpectedRevision != changes[0].ExpectedRevision || string(prior.Key) != string(changes[0].Key) || !bytes.Equal(prior.Value, changes[0].Value) {
			return nil, errors.New("transaction id reused with changed input")
		}
		return []uint64{n.revisions[tx]}, nil
	}
	if len(changes) != 1 || changes[0].ExpectedRevision != n.state.Revision {
		return nil, errors.New("revision conflict")
	}
	n.state = native.StateValue{Revision: n.state.Revision + 1, Value: changes[0].Value}
	if n.receipts == nil {
		n.receipts = make(map[id.ID]native.StateChange)
		n.revisions = make(map[id.ID]uint64)
	}
	n.receipts[tx] = changes[0]
	n.revisions[tx] = n.state.Revision
	return []uint64{n.state.Revision}, nil
}

func TestWelcomeSettingsGetAndPutUseEncryptedStateRevision(t *testing.T) {
	h, auth := announcementInteractionsFixture(t)
	n := &welcomeSettingsNative{binding: native.Binding{Instance: interactionID(1), Community: h.CommunityID, Installation: interactionID(3)}}
	settings := WelcomeInteractions{Authority: auth, CommunityID: h.CommunityID, Native: n}
	actor := InteractionActor{User: interactionID(6)}
	get := settings.Handlers()[protocol.OpSettingsGet]
	result, err := get.Handle(context.Background(), actor, protocol.Request{CommunityID: h.CommunityID, Operation: protocol.OpSettingsGet, Args: []any{welcomeSettingsSection}})
	if err != nil || result.Status != 0 || result.Revision == nil || *result.Revision != 0 || string(result.Data) != "\x80" {
		t.Fatalf("empty welcome settings=%+v err=%v", result, err)
	}
	cfg := welcome.Config{ChannelID: interactionID(22), Template: "Welcome {member}", ServerName: "Dilla"}
	change, err := welcome.ConfigStateChange(cfg)
	if err != nil {
		t.Fatal(err)
	}
	requestRevision, expires := uint64(0), uint64(120)
	put := settings.Handlers()[protocol.OpSettingsPut]
	request := protocol.Request{RequestID: interactionID(30), CommunityID: h.CommunityID, Operation: protocol.OpSettingsPut, Revision: &requestRevision, Expires: &expires, Args: []any{welcomeSettingsSection, change.Value}}
	result, err = put.Handle(context.Background(), actor, request)
	if err != nil || result.Status != 0 || result.Revision == nil || *result.Revision != 1 || n.state.Revision != 1 {
		t.Fatalf("welcome settings put=%+v stored=%+v err=%v", result, n.state, err)
	}
	replay, err := put.Handle(context.Background(), actor, request)
	if err != nil || replay.Status != 0 || replay.Revision == nil || *replay.Revision != 1 || n.state.Revision != 1 {
		t.Fatalf("welcome settings replay=%+v stored=%+v err=%v, want original receipt", replay, n.state, err)
	}
	result, err = get.Handle(context.Background(), actor, protocol.Request{CommunityID: h.CommunityID, Operation: protocol.OpSettingsGet, Args: []any{welcomeSettingsSection}})
	if err != nil || result.Status != 0 || result.Revision == nil || *result.Revision != 1 || string(result.Data) != string(n.state.Value) {
		t.Fatalf("configured welcome settings=%+v err=%v", result, err)
	}
	var fields []any
	if err := cborx.Unmarshal(result.Data, &fields); err != nil || len(fields) != 4 || fields[2] != cfg.Template {
		t.Fatalf("welcome settings document=%#v err=%v", fields, err)
	}
}

func TestWelcomeSettingsPutRejectsStaleRevisionAndUnauthorizedActor(t *testing.T) {
	h, auth := announcementInteractionsFixture(t)
	n := &welcomeSettingsNative{binding: native.Binding{Instance: interactionID(1), Community: h.CommunityID, Installation: interactionID(3)}, state: native.StateValue{Revision: 4}}
	settings := WelcomeInteractions{Authority: auth, CommunityID: h.CommunityID, Native: n}
	cfg, err := welcome.ConfigStateChange(welcome.Config{ChannelID: interactionID(22), Template: "Welcome {member}", ServerName: "Dilla"})
	if err != nil {
		t.Fatal(err)
	}
	requestRevision, expires := uint64(3), uint64(120)
	request := protocol.Request{RequestID: interactionID(31), CommunityID: h.CommunityID, Operation: protocol.OpSettingsPut, Revision: &requestRevision, Expires: &expires, Args: []any{welcomeSettingsSection, cfg.Value}}
	put := settings.Handlers()[protocol.OpSettingsPut]
	result, err := put.Handle(context.Background(), InteractionActor{User: interactionID(6)}, request)
	if err != nil || result.Status != interactionStatusConflict || result.Revision == nil || *result.Revision != 4 || len(n.changes) != 1 || n.changes[0].ExpectedRevision != 3 {
		t.Fatalf("stale welcome put=%+v transactions=%+v err=%v", result, n.changes, err)
	}
	auth.allowed = false
	request.Revision = ptrUint64(4)
	result, err = put.Handle(context.Background(), InteractionActor{User: interactionID(6)}, request)
	if err != nil || result.Status != interactionStatusForbidden || len(n.changes) != 1 {
		t.Fatalf("unauthorized welcome put=%+v transactions=%+v err=%v", result, n.changes, err)
	}
}

func ptrUint64(v uint64) *uint64 { return &v }
