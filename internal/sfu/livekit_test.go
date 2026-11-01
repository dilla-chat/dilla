package sfu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	pmedia "github.com/pion/webrtc/v4/pkg/media"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/jonasthim/dilla/internal/obs"
	"github.com/jonasthim/dilla/internal/sfu/sfutest"
)

func testConfig() Config {
	c := DefaultConfig()
	c.APISecret = "dilla-spike-secret-0123456789abcdef"
	return c
}

// The YAML must carry exactly the keys gap-20 4 traced through NewConfig,
// ValidateKeys, LoadTURNSecrets and NewLocalNode.
func TestYAMLCarriesTheLoopbackKeys(t *testing.T) {
	y, err := testConfig().YAML()
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}
	for _, want := range []string{
		"port: 7880",
		"bind_addresses:",
		"- \"127.0.0.1\"",
		"keys:",
		"node_ip: 127.0.0.1",
		"use_external_ip: false",
		"enable_loopback_candidate: true",
		"udp_port: 7882",
		"tcp_port: 0",
		// R18 and the spec's "System architecture" item 4 (spec line 415) both
		// name turn.enabled: false explicitly. LiveKit's own DefaultConfig
		// already has it false (config.go:597-603), so the key changes no
		// behaviour — it exists so a reader checking spec line 415 against the
		// rendered YAML can find it (deviation B18).
		"turn:",
		"enabled: false",
	} {
		if !strings.Contains(y, want) {
			t.Errorf("YAML is missing %q:\n%s", want, y)
		}
	}
	if strings.Contains(y, "turn_servers") {
		t.Error("the spike must not declare rtc.turn_servers: each entry then needs credentials " +
			"or LoadTURNSecrets fails with ErrTURNServerNoCredentials")
	}
}

func TestYAMLRejectsAShortSecret(t *testing.T) {
	c := testConfig()
	c.APISecret = "short"
	if _, err := c.YAML(); err == nil {
		t.Fatal("YAML accepted a 5-character API secret; LiveKit only logs about it, so this " +
			"check has to be ours")
	}
}

// strictMode true means every key must exist on LiveKit's structs. This is the
// test that catches a key renamed upstream on a version bump.
func TestNewConfigAcceptsTheYAMLInStrictMode(t *testing.T) {
	y, err := testConfig().YAML()
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}
	conf, err := config.NewConfig(y, true, nil, nil)
	if err != nil {
		t.Fatalf("config.NewConfig(yaml, true, nil, nil): %v", err)
	}
	if err := conf.ValidateKeys(); err != nil {
		t.Errorf("ValidateKeys: %v", err)
	}
	if err := conf.LoadTURNSecrets(); err != nil {
		t.Errorf("LoadTURNSecrets: %v", err)
	}
	node, err := routing.NewLocalNode(conf)
	if err != nil {
		t.Fatalf("routing.NewLocalNode: %v (ErrIPNotSet means node_ip did not reach the config)", err)
	}
	if node.NodeID() == "" {
		t.Error("NewLocalNode produced an empty node id")
	}
	if conf.RTC.NodeIP.IsEmpty() {
		t.Error("rtc.node_ip is empty; resolveNodeIP would then run and pull in the default public STUN list")
	}
	if conf.RTC.UseExternalIP {
		t.Error("use_external_ip is true; that forces a STUN round trip on a loopback-only box")
	}
	if !conf.RTC.EnableLoopbackCandidate {
		t.Error("enable_loopback_candidate is false; CreateUDPMuxesFromPorts then yields zero host " +
			"candidates on a machine whose only interface is lo, silently and with no error")
	}
	if conf.TURN.Enabled {
		t.Error("turn.enabled is true; R18 and the spec's System architecture item 4 require it off")
	}
}

// I13 (fix wave): the livekit.* keys dilla.toml carries reach LiveKit's parsed config.
// advertise_internal_ip keeps the local host candidate beside node_ip's public one (so relay
// pairing stays on-host), stun_servers replaces LiveKit's Google/Twilio fallback in every join
// response, and max_voice_participants is the room cap. A zero cap renders no room key at all, so
// LiveKit's room defaults are rendered explicitly; only the cap is conditional.
func TestTheRTCKeysReachLiveKitsConfig(t *testing.T) {
	c := testConfig()
	c.NodeIP = "203.0.113.7"
	c.AdvertiseInternalIP = true
	c.STUNServers = []string{"chat.example:3478", "[2001:db8::1]:3478"}
	c.MaxParticipants = 25
	y, err := c.YAML()
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}
	conf, err := config.NewConfig(y, true, nil, nil)
	if err != nil {
		t.Fatalf("config.NewConfig(strict): %v\n%s", err, y)
	}
	if !conf.RTC.AdvertiseInternalIP {
		t.Error("rtc.advertise_internal_ip did not reach LiveKit")
	}
	if !reflect.DeepEqual(conf.RTC.STUNServers, c.STUNServers) {
		t.Errorf("rtc.stun_servers = %v, want %v", conf.RTC.STUNServers, c.STUNServers)
	}
	if conf.Room.MaxParticipants != 25 {
		t.Errorf("room.max_participants = %d, want 25", conf.Room.MaxParticipants)
	}
	if conf.Room.AutoCreate {
		t.Error("room.auto_create is true: rooms must exist only once CreateRoom opened them (MD-16)")
	}

	c.AdvertiseInternalIP, c.STUNServers, c.MaxParticipants = false, nil, 0
	y, err = c.YAML()
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}
	if strings.Contains(y, "stun_servers") || strings.Contains(y, "max_participants") {
		t.Errorf("an empty STUN list or a zero cap rendered a key:\n%s", y)
	}
	conf, err = config.NewConfig(y, true, nil, nil)
	if err != nil {
		t.Fatalf("config.NewConfig(strict): %v", err)
	}
	if conf.RTC.AdvertiseInternalIP || conf.Room.MaxParticipants != 0 {
		t.Errorf("advertise_internal_ip = %t, max_participants = %d, want false and 0",
			conf.RTC.AdvertiseInternalIP, conf.Room.MaxParticipants)
	}
}

// A STUN entry is written into YAML, so one that could break out of its list item is refused.
func TestYAMLRejectsAMalformedSTUNServer(t *testing.T) {
	for _, bad := range []string{"", "host:3478\n  node_ip: 10.0.0.1", "a b:1"} {
		c := testConfig()
		c.STUNServers = []string{bad}
		if _, err := c.YAML(); err == nil {
			t.Errorf("YAML accepted the STUN server %q", bad)
		}
	}
}

func TestValidateKeysFailsWithoutKeys(t *testing.T) {
	// The same shape with an empty keys map: ValidateKeys is the only call that
	// refuses an under-specified config, and there is no development escape.
	const noKeys = "port: 7880\n" +
		"bind_addresses:\n  - 127.0.0.1\n" +
		"keys: {}\n" +
		"rtc:\n" +
		"  node_ip: 127.0.0.1\n" +
		"  use_external_ip: false\n" +
		"  enable_loopback_candidate: true\n" +
		"  udp_port: 7882\n" +
		"  tcp_port: 0\n"
	conf, err := config.NewConfig(noKeys, true, nil, nil)
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	err = conf.ValidateKeys()
	if err == nil {
		t.Fatal("ValidateKeys accepted an empty keys map")
	}
	if !strings.Contains(err.Error(), "one of key-file or keys must be provided") {
		t.Errorf("ValidateKeys error = %q, want it to name the missing keys", err)
	}
}

// prometheus.Init registers on the global default registry and the package's
// metric vectors are nil until it runs, so it must happen before
// service.InitializeServer, exactly as LiveKit's own cmd/server/main.go does.
// Ordering cannot be observed at run time, so assert it in the source.
func TestPrometheusInitPrecedesInitializeServer(t *testing.T) {
	src, err := os.ReadFile("livekit.go")
	if err != nil {
		t.Fatalf("read livekit.go: %v", err)
	}
	text := string(src)
	initIdx := strings.Index(text, "prometheus.Init(")
	serverIdx := strings.Index(text, "service.InitializeServer(")
	if initIdx < 0 {
		t.Fatal("livekit.go never calls prometheus.Init")
	}
	if serverIdx < 0 {
		t.Fatal("livekit.go never calls service.InitializeServer")
	}
	if initIdx > serverIdx {
		t.Error("prometheus.Init is called after service.InitializeServer")
	}
	if !strings.Contains(text, "go s.server.Start()") && !strings.Contains(text, "go func()") {
		t.Error("LivekitServer.Start blocks on <-s.doneChan; it must run in its own goroutine")
	}
	if setIdx := strings.Index(text, "installLiveKitLogger("); setIdx < 0 || setIdx > serverIdx {
		t.Error("the log bridge must be installed before service.InitializeServer")
	}
	if strings.Contains(text, "logger.SetLogger(") {
		t.Error("Start must not write LiveKit's global logger itself: installLiveKitLogger does it once")
	}
	bridge, err := os.ReadFile("logbridge.go")
	if err != nil {
		t.Fatalf("read logbridge.go: %v", err)
	}
	if strings.Count(string(bridge), "logger.SetLogger(") != 1 || !strings.Contains(string(bridge), "liveKitOnce.Do(") {
		t.Error("logbridge.go must call logger.SetLogger exactly once, inside liveKitOnce.Do")
	}
	if strings.Contains(text, "InitLoggerFromConfig(") {
		t.Error("config.InitLoggerFromConfig replaces slog.Default")
	}
}

// pkg/media imports github.com/livekit/media-sdk/opus, which is cgo and libopus
// with no build-tag fallback (gap-21 §3). NOTHING in dillad may import it — not
// internal/sfu, not cmd/dillad, not a package that does not exist yet. A scan of
// this directory's .go files would only prove it about this directory, so ask the
// toolchain for the whole module's transitive import set instead.
func TestNothingInTheModuleImportsTheCgoMediaPackage(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		// runtime.GOROOT is deprecated; the environment variable is what `go test` itself is run
		// under when the tool is not on PATH. (`go env GOROOT` would need the tool it is looking for.)
		if root := os.Getenv("GOROOT"); root != "" {
			goTool = filepath.Join(root, "bin", "go")
		} else {
			t.Fatalf("cannot locate the go tool to list the module's dependencies: %v", err)
		}
	}
	// -test is required, not optional: without it `go list -deps ./...` walks
	// only the non-test import graph, and server-sdk-go/v2 is imported from this
	// very file, so the plain form reports neither the SDK nor anything it would
	// pull in — the check would pass vacuously. With -test the set covers every
	// package dillad compiles, tests included.
	cmd := exec.CommandContext(t.Context(), goTool, "list", "-deps", "-test", "./...")
	cmd.Dir = "../.." // the module root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps -test ./...: %v\n%s", err, out)
	}
	forbidden := []string{
		"github.com/livekit/server-sdk-go/v2/pkg/media",
		"github.com/livekit/media-sdk",
		"gopkg.in/hraban/opus.v2",
	}
	for _, line := range strings.Split(string(out), "\n") {
		// Under -test a package built for a test binary is listed as
		// "<import path> [<test package>.test]"; compare the import path alone.
		path, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		for _, f := range forbidden {
			if path == f || strings.HasPrefix(path, f+"/") {
				t.Fatalf("%s is in the module's transitive import set; it needs cgo and libopus "+
					"headers, so CGO_ENABLED=0 go build ./cmd/dillad would stop working", path)
			}
		}
	}
	if !strings.Contains(string(out), "github.com/livekit/server-sdk-go/v2\n") {
		t.Error("go list -deps -test ./... does not mention server-sdk-go/v2 at all; the check " +
			"passed vacuously, so the package list or the working directory is wrong")
	}
}

// The release binary never holds frame keys or imports the media adapters.
func TestDilladDoesNotImportTheMediaPackages(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		root := os.Getenv("GOROOT")
		if root == "" {
			t.Fatalf("cannot locate the go tool: %v", err)
		}
		goTool = filepath.Join(root, "bin", "go")
	}
	cmd := exec.CommandContext(t.Context(), goTool, "list", "-deps", "./cmd/dillad")
	cmd.Dir = "../.."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/dillad: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		switch strings.TrimSpace(line) {
		case "github.com/jonasthim/dilla/internal/sframe", "github.com/jonasthim/dilla/internal/media":
			t.Errorf("cmd/dillad imports %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(string(out), "github.com/jonasthim/dilla/internal/sfu\n") {
		t.Error("go list -deps ./cmd/dillad does not list internal/sfu; the check passed vacuously")
	}
}

// The spike itself: boot the SFU on loopback and have two Go-SDK participants
// exchange a data message. No livekit-client, no media package.
func TestTwoParticipantsExchangeADataMessage(t *testing.T) {
	ctx := context.Background()
	c := testConfig()

	startedAt := time.Now()
	srv, err := Start(ctx, c)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	startup := time.Since(startedAt)
	t.Logf("SPIKE startup=%s url=%s http=%s", startup, srv.URL(), srv.HTTPURL())
	defer func() {
		if err := srv.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	const room = "dilla-spike"
	if err := srv.CreateRoom(ctx, room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	// Test-only: call tokens never carry a data grant (DEV-61, TestTheCallTokenCannotPublishData);
	// this permission exists so the data path itself stays proven.
	dataPerm := &livekit.ParticipantPermission{CanSubscribe: true, CanPublish: true, CanPublishData: true}
	aliceToken, err := srv.Token(room, "alice", dataPerm, nil)
	if err != nil {
		t.Fatalf("Token(alice): %v", err)
	}
	bobToken, err := srv.Token(room, "bob", dataPerm, nil)
	if err != nil {
		t.Fatalf("Token(bob): %v", err)
	}

	received := make(chan []byte, 4)
	bobCB := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnDataPacket: func(data lksdk.DataPacket, params lksdk.DataReceiveParams) {
				if u, ok := data.(*lksdk.UserDataPacket); ok {
					select {
					case received <- append([]byte(nil), u.Payload...):
					default:
					}
				}
			},
		},
	}
	bob, err := lksdk.ConnectToRoomWithToken(srv.URL(), bobToken, bobCB)
	if err != nil {
		t.Fatalf("bob join: %v", err)
	}
	defer bob.Disconnect()

	alice, err := lksdk.ConnectToRoomWithToken(srv.URL(), aliceToken, &lksdk.RoomCallback{})
	if err != nil {
		t.Fatalf("alice join: %v", err)
	}
	defer alice.Disconnect()

	payload := []byte("dilla spike: mls handshake would go here")
	deadline := time.After(20 * time.Second)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	firstTry := time.Now()
	for {
		if err := alice.LocalParticipant.PublishDataPacket(
			lksdk.UserData(payload), lksdk.WithDataPublishReliable(true)); err != nil {
			t.Logf("publish retry: %v", err)
		}
		select {
		case got := <-received:
			if string(got) != string(payload) {
				t.Fatalf("bob received %q, want %q", got, payload)
			}
			t.Logf("SPIKE data_delivered_after=%s", time.Since(firstTry))
			return
		case <-deadline:
			t.Fatalf("bob received no data packet within 20s; alice sees %d remote participants",
				len(alice.GetRemoteParticipants()))
		case <-tick.C:
		}
	}
}

// nothingListensOn fails the test if addr accepts a TCP connection at any point
// during window. The window has to outlast the boot the aborted Start left
// running: returning before the listeners are even bound is not success, it is
// the leak one moment earlier.
//
// An aborted Start must leave no listener behind: LiveKit binds
// its listeners (server.go:246) a deliberate 100 ms before it sets the running
// flag (server.go:333), and Stop returns immediately while that flag is false
// (server.go:373, `if !s.running.Swap(false) { return }`). A Stop issued inside
// that window does nothing, so the boot finishes, the goroutine parks on
// <-doneChan forever, and the port stays bound with the *Server handle already
// discarded — nothing can ever stop it.
func nothingListensOn(t *testing.T, addr string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext(t.Context(), "tcp", addr)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("%s accepted a connection after Start aborted: the server was stopped "+
				"while its running flag was false, so Stop did nothing and the boot goroutine "+
				"leaked with the port bound", addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Abort path 1: the caller's context is already cancelled. cmd/dillad passes a
// signal.NotifyContext, so this is the SIGINT-during-boot case.
func TestStartAbortedByAContextCancelLeavesNothingListening(t *testing.T) {
	c := testConfig()
	c.Port = 7890
	c.UDPPort = 7892

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	srv, err := Start(ctx, c)
	if err == nil {
		_ = srv.Stop(context.Background())
		t.Fatal("Start returned no error for an already-cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Start error = %v, want it to wrap context.Canceled", err)
	}
	if srv != nil {
		t.Error("Start returned a non-nil *Server together with an error")
	}
	nothingListensOn(t, net.JoinHostPort(c.BindAddress, fmt.Sprint(c.Port)), 3*time.Second)
}

// Abort path 2: the startup deadline expires. Same window, same no-op Stop.
func TestStartAbortedByTheDeadlineLeavesNothingListening(t *testing.T) {
	restore := startTimeout
	startTimeout = time.Millisecond
	defer func() { startTimeout = restore }()

	c := testConfig()
	c.Port = 7894
	c.UDPPort = 7896

	srv, err := Start(context.Background(), c)
	if err == nil {
		_ = srv.Stop(context.Background())
		t.Fatal("Start returned no error although the startup deadline was 1ms")
	}
	if !strings.Contains(err.Error(), "did not accept a connection") {
		t.Errorf("Start error = %v, want the startup-deadline error", err)
	}
	nothingListensOn(t, net.JoinHostPort(c.BindAddress, fmt.Sprint(c.Port)), 3*time.Second)
}

// I11 (fix wave): ending a call closes its room. DeleteRoom disconnects a participant still in the
// room, and a room LiveKit does not know (never opened, or already closed) is not an error: the call
// is over either way.
func TestDeleteRoomDisconnectsItsParticipants(t *testing.T) {
	c := testConfig()
	c.Port = 7900
	c.UDPPort = 7902
	srv, err := Start(context.Background(), c)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := srv.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	if err := srv.DeleteRoom(t.Context(), "never-opened"); err != nil {
		t.Fatalf("DeleteRoom of a room LiveKit never had = %v, want nil", err)
	}

	const room = "dilla-call"
	if err := srv.CreateRoom(t.Context(), room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	tok, err := srv.Token(room, "bob", PublishGrant(true, false, false), nil)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	gone := make(chan struct{})
	var once sync.Once
	bob, err := lksdk.ConnectToRoomWithToken(srv.URL(), tok, &lksdk.RoomCallback{
		OnDisconnected: func() { once.Do(func() { close(gone) }) },
	})
	if err != nil {
		t.Fatalf("bob join: %v", err)
	}
	defer bob.Disconnect()

	if err := srv.DeleteRoom(t.Context(), room); err != nil {
		t.Fatalf("DeleteRoom: %v", err)
	}
	select {
	case <-gone:
	case <-time.After(15 * time.Second):
		t.Fatal("bob is still connected 15s after his room was deleted")
	}
	if err := srv.DeleteRoom(t.Context(), room); err != nil {
		t.Fatalf("a second DeleteRoom of the closed room = %v, want nil", err)
	}
}

// The default configuration renders exactly interfaces.md b.9 with no conditional key: the golden is
// the contract a LiveKit upgrade is read against.
func TestTheDefaultYAMLIsTheVerifiedShape(t *testing.T) {
	y, err := testConfig().YAML()
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}
	const want = `port: 7880
bind_addresses:
  - "127.0.0.1"
keys:
  dilla: "dilla-spike-secret-0123456789abcdef"
rtc:
  node_ip: 127.0.0.1
  use_external_ip: false
  enable_loopback_candidate: true
  udp_port: 7882
  tcp_port: 0
  advertise_internal_ip: true
  use_ice_lite: false
  allow_tcp_fallback: false
audio:
  active_level: 35
  min_percentile: 40
  update_interval: 400
  smooth_intervals: 2
  active_red_encoding: false
room:
  auto_create: false
  empty_timeout: 300
  departure_timeout: 20
  enabled_codecs:
    - mime: audio/opus
    - mime: video/VP8
    - mime: video/H264
    - mime: video/rtx
limit:
  max_participant_name_length: 32
  max_metadata_size: 1
  max_attributes_size: 64
turn:
  enabled: false
`
	if y != want {
		t.Fatalf("default YAML:\n%s\nwant:\n%s", y, want)
	}
}

// Every conditional key, rendered and read back through LiveKit's strict parser.
func TestEveryConditionalKeyReachesLiveKit(t *testing.T) {
	c := testConfig()
	c.MaxParticipants = 25
	c.VP9 = true
	c.WebhookURL = "http://127.0.0.1:7883" + WebhookPath
	c.LimitNumTracks = 4000
	c.LimitBytesPerSec = 125_000_000
	c.IPsExcludes = []string{"172.17.0.0/16", "fd00::/8"}
	y, err := c.YAML()
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}
	for _, block := range []string{
		"room:\n  auto_create: false\n  max_participants: 25\n",
		"    - mime: video/VP9\n      fmtp_line: \"profile-id=0\"\n    - mime: video/rtx\n",
		"  ips:\n    excludes:\n      - \"172.17.0.0/16\"\n      - \"fd00::/8\"\n",
		"limit:\n  num_tracks: 4000\n  bytes_per_sec: 125000000\n  max_participant_name_length: 32\n  max_metadata_size: 1\n  max_attributes_size: 64\n",
		"webhook:\n  api_key: dilla\n  urls:\n    - \"http://127.0.0.1:7883/livekit/webhook\"\n  filter_params:\n    include_events:\n" +
			"      - room_started\n      - room_finished\n      - participant_joined\n      - participant_left\n" +
			"      - participant_connection_aborted\n      - track_published\n      - track_unpublished\n",
	} {
		if !strings.Contains(y, block) {
			t.Errorf("YAML lacks\n%s\nin\n%s", block, y)
		}
	}
	conf, err := config.NewConfig(y, true, nil, nil)
	if err != nil {
		t.Fatalf("config.NewConfig(strict): %v\n%s", err, y)
	}
	if err := conf.ValidateKeys(); err != nil {
		t.Fatalf("ValidateKeys: %v", err)
	}
	// The webhook is signed with a key of the keys map, the same field, or InitializeServer fails
	// with ErrWebHookMissingAPIKey.
	if conf.WebHook.APIKey != c.APIKey || conf.Keys[c.APIKey] == "" {
		t.Errorf("webhook.api_key = %q, keys = %v; want the keys map's own key %q", conf.WebHook.APIKey, conf.Keys, c.APIKey)
	}
	if conf.Room.AutoCreate || conf.Room.EmptyTimeout != 300 || conf.Room.DepartureTimeout != 20 {
		t.Errorf("room = %+v", conf.Room)
	}
	if conf.Limit.NumTracks != 4000 || conf.Limit.BytesPerSec != 125_000_000 {
		t.Errorf("limit = %+v", conf.Limit)
	}
	if len(conf.Room.EnabledCodecs) != 5 {
		t.Errorf("enabled_codecs = %+v, want opus, VP8, H264, VP9, rtx", conf.Room.EnabledCodecs)
	}
	if conf.RTC.AllowTCPFallback == nil || *conf.RTC.AllowTCPFallback {
		t.Error("rtc.allow_tcp_fallback must be rendered false: unset is true, and tcp_port 0 leaves no TCP pair")
	}
	for _, refused := range []func(*Config){
		func(c *Config) { c.WebhookURL = "http://127.0.0.1:7883/x\"\n  urls: [evil]" },
		func(c *Config) { c.IPsExcludes = []string{"172.17.0.0/16\n  - x"} },
	} {
		bad := testConfig()
		refused(&bad)
		if _, err := bad.YAML(); err == nil {
			t.Errorf("YAML accepted a value that breaks out of its line: %+v", bad)
		}
	}
}

func TestTestOnlyAV1CodecCanBeOfferedByTheRealSFU(t *testing.T) {
	c := testConfig()
	c.TestAV1 = true
	y, err := c.YAML()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(y, "    - mime: video/AV1\n") {
		t.Fatalf("AV1 not enabled in test SFU:\n%s", y)
	}
}

// LiveKit's strict mode refuses the six spellings G22 probed; the renderer must use the real keys.
func TestStrictModeRefusesTheSixWrongSpellings(t *testing.T) {
	for _, c := range []struct{ yaml, want string }{
		{"rtc:\n  ice_lite: true\n", "field ice_lite not found in type config.RTCConfig"},
		{"audio:\n  red_encoding: true\n", "field red_encoding not found in type sfu.AudioConfig"},
		{"room:\n  enabled_codec: []\n", "field enabled_codec not found in type config.RoomConfig"},
		{"room:\n  enabled_codecs:\n    - mime: video/H264\n      fmtp: x\n", "field fmtp not found in type config.CodecSpec"},
		{"limit:\n  max_tracks: 1\n", "field max_tracks not found in type config.LimitConfig"},
		{"webhook:\n  client_timeout: 5s\n", "field client_timeout not found in type webhook.WebHookConfig"},
	} {
		_, err := config.NewConfig(c.yaml, true, nil, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want an error containing %q", c.yaml, err, c.want)
		}
	}
}

// G38: LiveKit's own HTTP-serve failure path calls Stop(true) on itself; Done must report that,
// and must report nothing when dillad stops the server.
func TestDoneReportsAnExitNobodyAskedFor(t *testing.T) {
	c := testConfig()
	c.Port, c.UDPPort = 7904, 7906
	srv, err := Start(t.Context(), c)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	srv.server.Stop(true)
	select {
	case err := <-srv.Done():
		if err == nil {
			t.Fatal("Done delivered a nil error")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Done delivered nothing 20 s after LiveKit stopped itself")
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Errorf("Stop after the exit: %v", err)
	}

	c.Port, c.UDPPort = 7908, 7910
	srv, err = Start(t.Context(), c)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-srv.Done():
		t.Fatalf("Done delivered %v after a deliberate Stop", err)
	case <-time.After(time.Second):
	}
}

// everyCodec is what a publisher offers so the SFU's answer shows exactly what it accepts.
var everyCodec = []livekit.Codec{
	{Mime: "audio/opus"}, {Mime: "audio/red"}, {Mime: "audio/PCMU"}, {Mime: "audio/PCMA"},
	{Mime: "video/VP8"}, {Mime: "video/VP9", FmtpLine: "profile-id=0"}, {Mime: "video/VP9", FmtpLine: "profile-id=1"},
	{Mime: "video/AV1"}, {Mime: "video/H265"},
	// First of the H.264 entries on purpose: the SDK moves the first codec whose mime matches the
	// track's to the front of the transceiver's preferences (localparticipant.go:116-123), and LiveKit
	// answers an m-section that leads with packetization-mode 0 with VP8 (plan review 2026-10-03).
	{Mime: "video/H264", FmtpLine: h264Fmtp},
	{Mime: "video/H264", FmtpLine: "packetization-mode=0"},
	{Mime: "video/H264", FmtpLine: "profile-level-id=640032"},
}

type sdpCodec struct{ kind, name, fmtp string }

// answerCodecs reads every rtpmap of an SDP with its media kind and its fmtp line.
func answerCodecs(sdp string) []sdpCodec {
	type rtpmap struct{ kind, pt, name string }
	var maps []rtpmap
	fmtp := map[string]string{}
	kind := ""
	for _, line := range strings.Split(sdp, "\r\n") {
		switch {
		case strings.HasPrefix(line, "m="):
			kind, _, _ = strings.Cut(strings.TrimPrefix(line, "m="), " ")
		case strings.HasPrefix(line, "a=rtpmap:"):
			pt, rest, _ := strings.Cut(strings.TrimPrefix(line, "a=rtpmap:"), " ")
			name, _, _ := strings.Cut(rest, "/")
			maps = append(maps, rtpmap{kind: kind, pt: pt, name: strings.ToLower(name)})
		case strings.HasPrefix(line, "a=fmtp:"):
			pt, rest, _ := strings.Cut(strings.TrimPrefix(line, "a=fmtp:"), " ")
			fmtp[kind+"/"+pt] = rest
		}
	}
	out := make([]sdpCodec, 0, len(maps))
	for _, m := range maps {
		out = append(out, sdpCodec{kind: m.kind, name: m.name, fmtp: fmtp[m.kind+"/"+m.pt]})
	}
	return out
}

// SP-31 server half: whatever a publisher offers, the SFU answers only with the dilla codecs —
// Opus without RED, PCMU or PCMA; VP8; H.264 (packetization-mode 1, 42e01f, for a publisher that
// prefers it — MD-29); VP9 profile 0 only with the flag; never AV1 or H.265 — and an H.264
// publication stays H.264 in LiveKit's own track state instead of falling back to VP8.
func TestTheSFUOfferCarriesOnlyTheDillaCodecs(t *testing.T) {
	if raceEnabled {
		t.Skip("upstream livekit-server data race in updateRidsFromSDP; runs in the non-race step")
	}
	for _, vp9 := range []bool{false, true} {
		t.Run(fmt.Sprintf("vp9=%t", vp9), func(t *testing.T) {
			c := testConfig()
			c.VP9 = vp9
			c.Port, c.UDPPort = 7912, 7914
			if vp9 {
				c.Port, c.UDPPort = 7916, 7918
			}
			srv, err := Start(t.Context(), c)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() { _ = srv.Stop(context.Background()) }()
			if err := srv.CreateRoom(t.Context(), "sp31"); err != nil {
				t.Fatalf("CreateRoom: %v", err)
			}
			tok, err := srv.Token("sp31", "publisher", PublishGrant(true, true, true), nil)
			if err != nil {
				t.Fatal(err)
			}
			room, err := lksdk.ConnectToRoomWithToken(srv.URL(), tok, &lksdk.RoomCallback{}, lksdk.WithCodecs(everyCodec))
			if err != nil {
				t.Fatalf("join: %v", err)
			}
			defer room.Disconnect()
			publish := func(name string, capability webrtc.RTPCodecCapability, source livekit.TrackSource) *lksdk.LocalTrackPublication {
				t.Helper()
				track, err := lksdk.NewLocalTrack(capability)
				if err != nil {
					t.Fatal(err)
				}
				for range 20 {
					pub, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name: name, Source: source})
					if err == nil {
						return pub
					}
					t.Logf("publish %s: %v (retrying)", name, err)
					time.Sleep(500 * time.Millisecond)
				}
				t.Fatalf("publish %s: no success in 20 tries", name)
				return nil
			}
			publish("mic", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, livekit.TrackSource_MICROPHONE)
			publish("camera", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, livekit.TrackSource_CAMERA)
			screen := publish("screen", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264Fmtp}, livekit.TrackSource_SCREEN_SHARE)
			want := 3
			if vp9 {
				publish("vp9", webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0"}, livekit.TrackSource_UNKNOWN)
				want = 4
			}
			var got []sdpCodec
			deadline := time.Now().Add(15 * time.Second)
			for {
				if d := room.LocalParticipant.GetPublisherPeerConnection().RemoteDescription(); d != nil &&
					strings.Count(d.SDP, "\r\nm=audio")+strings.Count(d.SDP, "\r\nm=video") >= want {
					got = answerCodecs(d.SDP)
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("no answer with %d media sections within 15 s", want)
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Logf("SP-31 server half vp9=%t answer codecs: %v", vp9, got)
			seen := map[string]bool{}
			for _, cd := range got {
				seen[cd.kind+"/"+cd.name] = true
				switch cd.kind + "/" + cd.name {
				case "audio/opus", "video/vp8", "video/rtx":
				case "video/h264":
					if !strings.Contains(cd.fmtp, "packetization-mode=1") || !strings.Contains(cd.fmtp, "profile-level-id=42e01f") {
						t.Errorf("H.264 answered with %q, want packetization-mode=1 and 42e01f only", cd.fmtp)
					}
				case "video/vp9":
					if !vp9 || !strings.Contains(cd.fmtp, "profile-id=0") {
						t.Errorf("VP9 %q answered with the flag %t", cd.fmtp, vp9)
					}
				default:
					t.Errorf("the SFU answered %s/%s %q", cd.kind, cd.name, cd.fmtp)
				}
			}
			for _, need := range []string{"audio/opus", "video/vp8", "video/h264"} {
				if !seen[need] {
					t.Errorf("the answer lacks %s", need)
				}
			}
			if vp9 && !seen["video/vp9"] {
				t.Error("the VP9 flag is on and the answer lacks VP9")
			}
			// MD-29: AddTrack must not have fallen back to VP8 for the H.264 screen track (an
			// fmtp-restricted H.264 entry makes it). The TrackInfo is LiveKit's AddTrack answer.
			if ti := screen.TrackInfo(); ti == nil || len(ti.Codecs) == 0 || !strings.EqualFold(ti.Codecs[0].MimeType, "video/H264") {
				t.Errorf("LiveKit took the H.264 screen track as %v: AddTrack fell back to another codec", ti.GetCodecs())
			}
		})
	}
}

// countingHandler counts records at INFO and above by the first two segments of their logger.
type countingHandler struct {
	mu     *sync.Mutex
	counts map[string]int
	attrs  []slog.Attr
}

func (h countingHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }
func (h countingHandler) WithAttrs(as []slog.Attr) slog.Handler {
	h.attrs = append(slices.Clone(h.attrs), as...)
	return h
}
func (h countingHandler) WithGroup(string) slog.Handler { return h }
func (h countingHandler) Handle(_ context.Context, r slog.Record) error {
	name := "-"
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "logger" {
			parts := strings.SplitN(a.Value.String(), "/", 3)
			name = strings.Join(parts[:min(2, len(parts))], "/")
		}
		return true
	})
	h.mu.Lock()
	h.counts[name]++
	h.mu.Unlock()
	return nil
}

// SP-37: with three publishing participants, LiveKit's log volume through the bridge stays bounded.
// 15 s rather than the task map's 20 keeps this package's slowest test inside the go job's per-test
// budget; the counts per logger are printed for the commit's measurement file.
func TestTheLiveKitLogVolumeStaysBounded(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	c := testConfig()
	c.Port, c.UDPPort = 7920, 7922
	c.Log = slog.New(countingHandler{mu: &mu, counts: counts})
	srv, err := Start(t.Context(), c)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Stop(context.Background()) }()
	silence := []byte{0xf8, 0xff, 0xfe}
	var stop atomic.Bool
	defer stop.Store(true)
	if err := srv.CreateRoom(t.Context(), "sp37"); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	for i := range 3 {
		tok, err := srv.Token("sp37", fmt.Sprintf("p%d", i), PublishGrant(true, true, true), nil)
		if err != nil {
			t.Fatal(err)
		}
		room, err := lksdk.ConnectToRoomWithToken(srv.URL(), tok, &lksdk.RoomCallback{})
		if err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
		defer room.Disconnect()
		track, err := lksdk.NewLocalTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name: "mic", Source: livekit.TrackSource_MICROPHONE}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		go func() {
			for !stop.Load() {
				_ = track.WriteSample(pmedia.Sample{Data: silence, Duration: 20 * time.Millisecond}, nil)
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
	time.Sleep(15 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	names := make([]string, 0, len(counts))
	total := 0
	for name, n := range counts {
		names = append(names, name)
		total += n
	}
	sort.Strings(names)
	for _, name := range names {
		t.Logf("SP-37 logger=%s lines=%d", name, counts[name])
	}
	t.Logf("SP-37 total=%d over 15s with 3 publishers", total)
	if total > 900 {
		t.Errorf("%d log lines at INFO and above in 15 s (ceiling 900, 20 a second)", total)
	}
}

// DEV-58: one /metrics over dillad's registry and the default one LiveKit registers on, served
// through LiveKitGatherer as serve wires it, with no family clash (a clash would answer 500 under
// promhttp's default HTTPErrorOnError).
func TestOneMetricsEndpointServesLiveKitAndDilla(t *testing.T) {
	c := testConfig()
	c.Port, c.UDPPort = sfutest.FreePorts(t)
	srv, err := Start(t.Context(), c)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Stop(context.Background()) }()
	reg := prometheus.NewRegistry()
	m := obs.NewMetrics(reg, prometheus.Gatherers{reg, LiveKitGatherer(prometheus.DefaultGatherer)})
	ts := httptest.NewServer(m.Handler(false, ""))
	defer ts.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics = %d: %s", resp.StatusCode, body)
	}
	for _, family := range []string{"livekit_room_total", "dilla_gateway_connections", "go_goroutines"} {
		if !bytes.Contains(body, []byte("# TYPE "+family+" ")) {
			t.Errorf("/metrics lacks %s", family)
		}
	}
}
