package dillad_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// presentSFU is upstreamSFU that reports one device connected in every room it is asked about.
type presentSFU struct {
	upstreamSFU
	mu     sync.Mutex
	device string
}

func (s *presentSFU) setDevice(d id.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.device = d.String()
}

func (s *presentSFU) Participants(context.Context, string) ([]*livekit.ParticipantInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.device == "" {
		return nil, nil
	}
	return []*livekit.ParticipantInfo{{Identity: s.device, State: livekit.ParticipantInfo_ACTIVE}}, nil
}

// web-3 task 3 (fix round 1): through the composition root, a voice_self the gateway admits reaches
// the call events' self-flag store, and GET /v1/channels/{id}/call shows the device self-muted.
func TestVoiceSelfReachesTheCallSnapshot(t *testing.T) {
	cfg, code := testConfigInvite(t)
	cfg.Log.Level = "warn"
	sfu := &presentSFU{}
	srv, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t), SFU: sfu,
	})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	h := srv.Handler()
	tok := accountToken(t, srv, code)
	repo, ctx := srv.Repo(), t.Context()

	user, err := repo.GetUserByUsername(ctx, "jonas")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	devices, err := repo.ListDevicesByUser(ctx, user.ID)
	if err != nil || len(devices) != 1 {
		t.Fatalf("ListDevicesByUser = %v, %v", devices, err)
	}
	dev := devices[0].ID
	sfu.setDevice(dev)

	rec := call(t, h, http.MethodPost, "/v1/communities", tok, []any{"lounge", []byte("{}"), uint64(0), uint64(0)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/communities = %d", rec.Code)
	}
	cid := idOf(t, decodeArray(t, rec)[0])
	rec = call(t, h, http.MethodPost, "/v1/communities/"+cid.String()+"/channels", tok,
		[]any{uint64(api.ChannelVoice), uint64(1), uint64(2), nil, "voice", "", uint64(0), uint64(0)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST voice channel = %d %s", rec.Code, errorCode(rec))
	}
	ch := idOf(t, decodeArray(t, rec)[0])
	group := store.GroupRow{
		GroupID: id.New(), Binding: []byte{0x80}, Kind: api.GroupCall, CommunityID: &cid, TargetID: ch,
		CallID: &ch, Ciphersuite: 1, Epoch: 7, ExternalSenderKeyID: id.New(),
		E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: srv.Now().Unix(),
	}
	if err := repo.CreateGroup(ctx, group); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := repo.ReplaceMembers(ctx, group.GroupID, 7, []store.MemberRow{{
		GroupID: group.GroupID, LeafIndex: 0, UserID: user.ID, DeviceID: dev, SignatureKey: make([]byte, 32), AddedEpoch: 7,
	}}); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
	rec = call(t, h, http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST calls = %d %s", rec.Code, errorCode(rec))
	}
	row, err := repo.GetVoiceSession(ctx, ch)
	if err != nil {
		t.Fatalf("GetVoiceSession: %v", err)
	}
	srv.CallEvents().Handle(ctx, &livekit.WebhookEvent{Event: webhook.EventParticipantJoined, Id: "EV_" + id.New().String(),
		Room: &livekit.Room{Name: row.LivekitRoom}, Participant: &livekit.ParticipantInfo{Identity: dev.String()}})

	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := dialReady(t, wctx, ts, tok)
	frame, err := cborx.Marshal([]any{uint64(52), uint64(0), nil, []any{ch[:], uint64(1)}})
	if err != nil {
		t.Fatalf("marshal voice_self: %v", err)
	}
	if err := c.Write(wctx, websocket.MessageBinary, frame); err != nil {
		t.Fatalf("write voice_self: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		rec = call(t, h, http.MethodGet, "/v1/channels/"+ch.String()+"/call", tok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET call = %d %s", rec.Code, errorCode(rec))
		}
		snap := decodeArray(t, rec)
		parts, _ := snap[2].([]any)
		if len(parts) == 1 {
			if p, _ := parts[0].([]any); len(p) == 3 && p[2] == api.VoiceInCall|api.VoiceSelfMute {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the snapshot never showed the device self-muted: %v", snap)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
