//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	botaccess "github.com/dilla-chat/dilla/internal/bot/access"
	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// tokenAuth resolves each bearer to its own session.
type tokenAuth map[string]auth.Session

func (a tokenAuth) Resolve(_ context.Context, token string) (auth.Session, error) {
	s, ok := a[token]
	if !ok {
		return auth.Session{}, errors.New("unknown token")
	}
	return s, nil
}
func (tokenAuth) RecordCapabilities(context.Context, []byte, uint64, uint64, uint64) error {
	return nil
}

// channelGroupStore resolves every group to a text channel group of community, on a current-wire community.
type channelGroupStore struct {
	presenceTestStore
	community id.ID
}

func (s channelGroupStore) GroupResource(context.Context, id.ID) (gateway.GroupResource, error) {
	c := s.community
	return gateway.GroupResource{Kind: 0, CommunityID: &c}, nil
}
func (s channelGroupStore) ChannelCommunity(context.Context, id.ID) (*id.ID, error) {
	c := s.community
	return &c, nil
}
func (channelGroupStore) Enabled(context.Context, id.ID) (bool, error) { return true, nil }

// fanoutSend is the Bot's send path ending in dillad's fan-out: the accepted message goes to the group's members as
// op 31 message_ct, as the delivery service sends an accepted Bot message.
type fanoutSend struct {
	gw     *gateway.Gateway
	device id.ID
	seq    atomic.Uint64
}

func (s *fanoutSend) Send(_ context.Context, group, _, _ id.ID, body []byte, now uint64) (protocol.SendReceipt, error) {
	n := s.seq.Add(1)
	payload, err := gateway.MessageCTPayload(n, 1, s.device, body, make([]byte, 32), now, nil)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	s.gw.DeliverGroup(group, gateway.Frame{Op: gateway.OpMessageCT, GroupID: &group, Payload: payload, Replay: true})
	return protocol.SendReceipt{Outcome: protocol.ReceiptAccepted, SendResponse: &protocol.OriginalSendResponse{Sequence: n}}, nil
}

// Task 17 fix round 2 (I3): POST /hooks → visible to a connected member within 500 ms at p95, over 100 runs, through a
// real dillad gateway: the fake dillad's /hooks hands op 66 to the Bot's live connection (gateway.DeliverBotWebhook)
// and waits for op 67; the Bot's real delivery loop and webhook poster post it through a send path ending in the
// gateway's fan-out; the member's own gateway connection receives the message_ct. Every POST must be answered 204.
func TestWebhookPostVisibleWithin500msP95(t *testing.T) {
	ctx := t.Context()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	instance, community, installation := id.New(), id.New(), id.New()
	botUser, botDevice, member, memberDevice := id.New(), id.New(), id.New(), id.New()
	channel, group, webhook := id.New(), id.New(), id.New()
	gw := gateway.New(gateway.Options{InstanceID: instance, Generation: 1, WireVersions: []uint64{6}, E2EEVersions: []uint64{1}, MediaVersions: []uint64{1},
		Store: channelGroupStore{community: community}, ScopeResolver: channelGroupStore{community: community}, CommunityCompatibility: channelGroupStore{community: community},
		TrustedOrigins: []string{"*"}, Log: log, FramesPerSecond: 1 << 20, FrameBurst: 1 << 20, BotLaunch: admitAllLaunch{},
		Auth: tokenAuth{
			"bot": {UserID: botUser, DeviceID: botDevice, Scope: auth.ScopeEnrolled, TokenHash: make([]byte, 32), WireVersion: auth.WireVersion,
				Bot: &botaccess.Binding{InstallationID: installation, CommunityID: community, State: store.BotInstallationReady}},
			"member": {UserID: member, DeviceID: memberDevice, Scope: auth.ScopeEnrolled, TokenHash: make([]byte, 32), WireVersion: auth.WireVersion},
		}})
	gwServer := httptest.NewServer(gw.Handler())
	t.Cleanup(gwServer.Close)
	t.Cleanup(func() { gw.Shutdown(context.Background()) })
	gw.SetGroupMembers(group, []id.ID{memberDevice})

	// The fake dillad's /hooks: the checks are internal/api's (webhooks_test.go); here it delivers and waits.
	hooks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/hooks/") {
			http.NotFound(w, r)
			return
		}
		text, _ := io.ReadAll(r.Body)
		delivery := id.New()
		payload, err := gateway.BotWebhookPayload(delivery, webhook, community, channel, "Terraria", nil, string(text), false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		waitCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if status, err := gw.DeliverBotWebhook(waitCtx, botDevice, delivery, payload); err != nil || status != 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hooks.Close)

	api, err := client.New(client.Config{BaseURL: gwServer.URL, InstanceID: instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	// The Bot: its real gateway runtime, delivery loop and webhook poster, on its Unix socket (the public /gateway
	// refuses a managed Bot, final review I3).
	socket := t.TempDir() + "/gateway.sock"
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gw.ServeBot(ln) }()
	t.Cleanup(func() { _ = ln.Close() })
	botAPI, err := client.New(client.Config{BaseURL: supervisedOrigin, InstanceID: instance, AllowLoopbackHTTP: true,
		HTTPClient: &http.Client{Transport: unixTransport(socket)}})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newBotGatewayRuntime(ctx, botAPI, true, log)
	runtime.httpClient = &http.Client{Transport: unixTransport(socket)}
	t.Cleanup(runtime.Close)
	runtime.SetWebhookPoster(installation, newWebhookPoster(community, &whDestination{group: group}, &fanoutSend{gw: gw, device: botDevice}, nil, log))
	if err := runtime.Ensure(installation, community, botDevice, botUser, "bot"); err != nil {
		t.Fatal(err)
	}

	// The member's own connection.
	memberClient, err := gatewayclient.New(gatewayclient.Config{Client: api, AllowLoopbackHTTP: true, ExpectedDeviceID: memberDevice, ExpectedUserID: member, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = memberClient.Run(ctx, func(context.Context) (string, error) { return "member", nil }) }()
	visible := make(chan string, 4)
	go func() {
		for d := range memberClient.Events() {
			if d.Frame.Op == gatewayclient.OpMessageCT && d.Frame.MessageCT != nil {
				visible <- string(d.Frame.MessageCT.Blob)
			}
			if d.Frame.N > 0 {
				_ = d.Ack()
			}
		}
	}()
	waitUntil(t, func() bool { return gw.PresenceStatus(botUser, 0) != 0 && gw.PresenceStatus(member, 0) != 0 })

	post := func(text string) (int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, hooks.URL+"/hooks/"+webhook.String()+"/secret", strings.NewReader(text))
		if err != nil {
			return 0, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	// The member shows the channel (subscribe); a warm-up post confirms the subscription took before the runs.
	waitUntil(t, func() bool { return memberClient.Subscribe(ctx, []id.ID{group}) == nil })
	warm := time.Now().Add(5 * time.Second)
	for warmed := false; !warmed; {
		if time.Now().After(warm) {
			t.Fatal("the member never saw a warm-up post")
		}
		if _, err := post("warm-up"); err != nil {
			t.Fatal(err)
		}
		select {
		case <-visible:
			warmed = true
		case <-time.After(200 * time.Millisecond):
		}
	}
	for len(visible) > 0 {
		<-visible
	}
	latencies := make([]time.Duration, 0, 100)
	for i := range 100 {
		text := "tick " + id.New().String()
		start := time.Now()
		status, err := post(text)
		if err != nil || status != http.StatusNoContent {
			t.Fatalf("run %d: POST = %d %v, want 204", i, status, err)
		}
		deadline := time.After(2 * time.Second)
	wait:
		for {
			select {
			case body := <-visible:
				if strings.Contains(body, text) {
					latencies = append(latencies, time.Since(start))
					break wait
				}
			case <-deadline:
				t.Fatalf("run %d: the member never saw the post", i)
			}
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := latencies[94]
	t.Logf("webhook POST → member message_ct: p50 %v, p95 %v", latencies[49], p95)
	if p95 > 500*time.Millisecond {
		t.Fatalf("webhook post visible p95 = %v, want <= 500ms", p95)
	}
}

// admitAllLaunch takes this test process for the current, authenticated Bot child on the Bot socket.
type admitAllLaunch struct{}

func (admitAllLaunch) Peer(int32, uint32) bool  { return true }
func (admitAllLaunch) Spend(int32, []byte) bool { return true }
func (admitAllLaunch) Authenticated(int32) bool { return true }
