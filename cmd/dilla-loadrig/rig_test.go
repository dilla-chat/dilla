package main

import (
	"context"
	"encoding/json"

	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dilla-chat/dilla/internal/media"
	"github.com/dilla-chat/dilla/internal/sframe"
	"github.com/dilla-chat/dilla/internal/sfu"
	"github.com/dilla-chat/dilla/internal/sfu/sfutest"
)

func TestTheMatrixExpandsToTheStatedCells(t *testing.T) {
	cells := Matrix()
	if len(cells) != 26 {
		t.Fatalf("Matrix() = %d cells, want 24 video + 2 voice = 26", len(cells))
	}
	names := map[string]Cell{}
	for _, c := range cells {
		names[c.Name] = c
		if c.Participants > 25 {
			t.Errorf("%s has %d participants; the call cap is 25", c.Name, c.Participants)
		}
	}
	for _, want := range []string{"video-s1-v3-q", "video-s1-v24-f", "video-s3-v12-h", "video-s10-v24-f", "voice-3t-22dtx", "voice-25t"} {
		if _, ok := names[want]; !ok {
			t.Errorf("missing cell %s", want)
		}
	}
	// 10 sharers cannot fit in a call of 4 (3 viewers + the sharer).
	for _, l := range Layers {
		if _, ok := names["video-s10-v3-"+string(l)]; ok {
			t.Errorf("video-s10-v3-%s exists: more sharers than participants", l)
		}
	}
	if c := names["video-s10-v24-f"]; c.Participants != 25 || c.Sharers != 10 || c.Layer != LayerF {
		t.Errorf("video-s10-v24-f = %+v", c)
	}
	if c := names["voice-3t-22dtx"]; c.Participants != 25 || c.Talkers != 3 || c.DTX != 22 {
		t.Errorf("voice-3t-22dtx = %+v", c)
	}
	if LayerBitrate[LayerQ] != 150_000 || LayerBitrate[LayerH] != 625_000 || LayerBitrate[LayerF] != 2_500_000 {
		t.Errorf("LayerBitrate = %v", LayerBitrate)
	}
}

func TestSelectNamesCells(t *testing.T) {
	all, err := Select("all")
	if err != nil || len(all) != 26 {
		t.Fatalf("Select(all) = %d, %v", len(all), err)
	}
	two, err := Select("voice-25t,video-s1-v3-q")
	if err != nil || len(two) != 2 || two[0].Name != "voice-25t" {
		t.Fatalf("Select(two) = %+v, %v", two, err)
	}
	if _, err := Select("video-s9-v9-z"); err == nil || !strings.Contains(err.Error(), `unknown cell "video-s9-v9-z"`) {
		t.Fatalf("Select(unknown) = %v", err)
	}
}

func TestThePromQLTextIsTheCapacityCardsDefinition(t *testing.T) {
	if PromQLEgress != `8 * sum(rate(livekit_packet_bytes{direction="outgoing"}[30s]))` {
		t.Fatalf("PromQLEgress = %s", PromQLEgress)
	}
}

const scrape0 = `# HELP livekit_packet_bytes packet bytes
# TYPE livekit_packet_bytes counter
livekit_packet_bytes{country="",direction="outgoing",node_id="ND_x",node_type="SERVER",transmission="initial"} 1.0e+06
livekit_packet_bytes{country="",direction="outgoing",node_id="ND_x",node_type="SERVER",transmission="retransmit"} 50000
livekit_packet_bytes{country="",direction="incoming",node_id="ND_x",node_type="SERVER",transmission="initial"} 200000
livekit_track_subscribed_total{kind="audio",node_id="ND_x",node_type="SERVER"} 0
dilla_note{text="a \"quoted\" value"} 1
`

const scrape1 = `livekit_packet_bytes{country="",direction="outgoing",node_id="ND_x",node_type="SERVER",transmission="initial"} 3.25e+07
livekit_packet_bytes{country="",direction="outgoing",node_id="ND_x",node_type="SERVER",transmission="retransmit"} 800000
livekit_packet_bytes{country="",direction="incoming",node_id="ND_x",node_type="SERVER",transmission="initial"} 3950000
livekit_track_subscribed_total{kind="audio",node_id="ND_x",node_type="SERVER"} 600
`

func TestEgressFromTwoScrapes(t *testing.T) {
	s0, err := ParseExposition(strings.NewReader(scrape0))
	if err != nil {
		t.Fatalf("ParseExposition(0): %v", err)
	}
	s1, err := ParseExposition(strings.NewReader(scrape1))
	if err != nil {
		t.Fatalf("ParseExposition(1): %v", err)
	}
	out := map[string]string{"direction": "outgoing"}
	delta := Sum(s1, "livekit_packet_bytes", out) - Sum(s0, "livekit_packet_bytes", out)
	// (32.5e6 + 0.8e6 − 1e6 − 0.05e6) B over 30 s → 8 × 32.25e6 / 30 / 1e6 = 8.6 Mbit/s, both transmissions summed.
	if got := 8 * delta / 30 / 1e6; got < 8.599 || got > 8.601 {
		t.Fatalf("egress = %.4f Mbit/s, want 8.6", got)
	}
	in := map[string]string{"direction": "INCOMING"} // label values compare case-insensitively
	if got := 8 * (Sum(s1, "livekit_packet_bytes", in) - Sum(s0, "livekit_packet_bytes", in)) / 30 / 1e6; got < 0.999 || got > 1.001 {
		t.Fatalf("ingress = %.4f Mbit/s, want 1.0", got)
	}
	if n := Sum(s1, "livekit_track_subscribed_total", map[string]string{"kind": "audio"}); n != 600 {
		t.Fatalf("audio subscriptions = %v, want 600 (25 × 24)", n)
	}
	for _, s := range s0 {
		if s.Name == "dilla_note" && s.Labels["text"] != `a "quoted" value` {
			t.Fatalf("escaped label = %q", s.Labels["text"])
		}
	}
	if _, err := ParseExposition(strings.NewReader("broken{a=\"1\" 2\n")); err == nil {
		t.Fatal("a sample without its closing brace parsed")
	}
}

func TestAudioSubscriptionsReadTheCurrentGaugeAfterAPreviousRoom(t *testing.T) {
	// LiveKit's subscribed_total is a gauge: the prior voice room had 600
	// subscriptions, then closed; the next room also has 600. Subtracting
	// the previous scrape would report zero for a full 25-person call.
	current := []Sample{{Name: "livekit_track_subscribed_total", Labels: map[string]string{"kind": "audio"}, Value: 600}}
	if got := audioSubscriptions(current); got != 600 {
		t.Fatalf("current audio subscriptions = %d, want 600", got)
	}
}

// decrypt counts n frames of publisher j's track at viewer v, through the decryptor's own path: n
// frames sealed by j and opened on v's ring.
func decrypt(t *testing.T, v *viewer, j int, n int) {
	t.Helper()
	key := [16]byte{0x0a}
	roster := []sframe.RosterEntry{{Leaf: 0, Device: deviceOf(0)}, {Leaf: 1, Device: deviceOf(1)}, {Leaf: 2, Device: deviceOf(2)}, {Leaf: 9, Device: deviceOf(9)}}
	ring := sframe.NewKeyRing(nil)
	ring.InstallEpoch(rigEpoch, key, roster, 9)
	sender, err := sframe.NewSender(key, uint16(j), rigEpoch, rigEpoch)
	if err != nil {
		t.Fatal(err)
	}
	dec := media.NewFrameDecryptor(ring, sframe.Opus, deviceOf(j), sframe.Mic, nil, v.counters(deviceOf(j)))
	for range n {
		sealed, err := sender.Encrypt(sframe.Opus, sframe.Mic, 0, []byte{0xfc, 1})
		if err != nil {
			t.Fatal(err)
		}
		if out, _ := dec.DecryptFrame(sealed); out == nil {
			t.Fatal("the test frame did not decrypt")
		}
	}
}

func baselines(viewers []*viewer) []tally {
	out := make([]tally, len(viewers))
	for i, v := range viewers {
		out[i] = v.tally()
	}
	return out
}

func TestEveryExpectedViewerMustDecryptDuringTheHold(t *testing.T) {
	c := Cell{Name: "one-share", Participants: 3, Sharers: 1}
	viewers := []*viewer{newViewer(), newViewer(), newViewer()}
	decrypt(t, viewers[2], 0, 1)
	if err := verifyViewerActivity(c, viewers, baselines([]*viewer{newViewer(), newViewer(), newViewer()})); err == nil || !strings.Contains(err.Error(), "participant 1") {
		t.Fatalf("a viewer with no decrypted frames was accepted: %v", err)
	}
	if err := verifyViewerActivity(Cell{Name: "empty", Participants: 1}, viewers[:1], baselines(viewers[:1])); err != nil {
		t.Fatalf("a room without a published track needs no viewer frames: %v", err)
	}
}

// The parked per-track item: with two publishers, a viewer that decrypts only one of their tracks
// fails the cell, and frames decrypted before the hold's baseline do not count.
func TestEveryViewerMustDecryptEveryPublishersTrackDuringTheHold(t *testing.T) {
	c := Cell{Name: "two-talkers", Participants: 3, Talkers: 2}
	viewers := []*viewer{newViewer(), newViewer(), newViewer()}
	decrypt(t, viewers[0], 1, 3)
	decrypt(t, viewers[1], 0, 3)
	decrypt(t, viewers[2], 0, 3) // viewer 2 hears talker 0 only
	before := baselines([]*viewer{newViewer(), newViewer(), newViewer()})
	err := verifyViewerActivity(c, viewers, before)
	if err == nil || !strings.Contains(err.Error(), "participant 2 decrypted no frames of participant 1's track") {
		t.Fatalf("a viewer missing one publisher's track was accepted: %v", err)
	}
	decrypt(t, viewers[2], 1, 1)
	if err := verifyViewerActivity(c, viewers, before); err != nil {
		t.Fatalf("every track moved: %v", err)
	}
	// Taken now, the baseline already holds every frame: nothing moved during the "hold".
	if err := verifyViewerActivity(c, viewers, baselines(viewers)); err == nil {
		t.Fatal("frames from before the hold counted")
	}
}

// The parked drop item: a non-sif drop during the hold fails the cell and names the code; SIF frames
// and drops from before the hold do not.
func TestADropDuringTheHoldFailsTheCellByCode(t *testing.T) {
	before := map[string]uint64{"sif": 3, "E_SFRAME_AUTH": 1}
	if err := holdDrops("c", before, map[string]uint64{"sif": 40, "E_SFRAME_AUTH": 1}); err != nil {
		t.Fatalf("only SIF frames and an earlier drop: %v", err)
	}
	err := holdDrops("c", before, map[string]uint64{"sif": 40, "E_SFRAME_AUTH": 3, "E_SFRAME_REPLAY": 1})
	if err == nil || !strings.Contains(err.Error(), "E_SFRAME_AUTH ×2") || !strings.Contains(err.Error(), "E_SFRAME_REPLAY ×1") || strings.Contains(err.Error(), "sif") {
		t.Fatalf("holdDrops = %v", err)
	}
}

// Up to two key frames that lost their parameter-set packet ("incomplete", a transport loss) are
// tolerated during the hold; a third, or any E_SFRAME_* code, fails the cell.
func TestAFewIncompleteKeyFramesDuringTheHoldAreTolerated(t *testing.T) {
	if err := holdDrops("c", nil, map[string]uint64{"incomplete": 2}); err != nil {
		t.Fatalf("two incomplete: %v", err)
	}
	if err := holdDrops("c", nil, map[string]uint64{"incomplete": 3}); err == nil {
		t.Fatal("three incomplete passed")
	}
	if err := holdDrops("c", nil, map[string]uint64{"incomplete": 1, "E_SFRAME_AUTH": 1}); err == nil || !strings.Contains(err.Error(), "E_SFRAME_AUTH") {
		t.Fatalf("an auth failure beside an incomplete one: %v", err)
	}
}

// RIGS-02: the scrape sends the token as a Bearer token; a 401 is an error that names the flag and
// never the token.
func TestTheMetricsScrapeSendsTheTokenAndNamesTheFlagOn401(t *testing.T) {
	const token = "scrape-token-for-the-test-0123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(scrape0))
	}))
	t.Cleanup(srv.Close)
	if s, err := HTTPMetrics(srv.URL, token)(t.Context()); err != nil || len(s) == 0 {
		t.Fatalf("with the token: %d samples, %v", len(s), err)
	}
	for _, wrong := range []string{"", "not-the-token"} {
		_, err := HTTPMetrics(srv.URL, wrong)(t.Context())
		if err == nil || !strings.Contains(err.Error(), "-metrics-token") || !strings.Contains(err.Error(), "401") {
			t.Fatalf("token %q: %v, want a 401 naming -metrics-token", wrong, err)
		}
		if wrong != "" && strings.Contains(err.Error(), wrong) {
			t.Fatalf("the error repeats the token: %v", err)
		}
	}
}

// RIGS-01: against a dilla-testhost the rig opens its room and mints every token through the
// control listener's POST /debug/sfu/token (the room created once, as a debug room the call-room
// sweep never sees), and joins the URL the host reports.
func TestTestHostRoomsOpenAndJoinThroughTheControlListener(t *testing.T) {
	type call struct {
		Room, Identity string
		Create         bool
	}
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/debug/sfu/token" {
			http.NotFound(w, r)
			return
		}
		var c call
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		calls = append(calls, c)
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "ws://127.0.0.1:7880", "http_url": "http://127.0.0.1:7880", "token": "jwt-" + c.Identity})
	}))
	t.Cleanup(srv.Close)
	rooms := TestHostRooms{Control: srv.URL + "/"}
	closeRoom, err := rooms.Open(t.Context(), "rig-x-1", 5)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	closeRoom()
	url, token, err := rooms.Join(t.Context(), "rig-x-1", "0123456789abcdef0123456789abcdef")
	if err != nil || url != "ws://127.0.0.1:7880" || token != "jwt-0123456789abcdef0123456789abcdef" {
		t.Fatalf("Join = %q, %q, %v", url, token, err)
	}
	if len(calls) != 2 || !calls[0].Create || calls[0].Room != "rig-x-1" || calls[1].Create || calls[1].Room != "rig-x-1" {
		t.Fatalf("calls = %+v, want one create then one join", calls)
	}
	if _, _, err := (TestHostRooms{Control: srv.URL + "/nothing-here"}).Join(t.Context(), "r", "i"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a wrong control URL: %v", err)
	}
}

// RIGS-11: every cell draws its own random base key; the rig has no fixed key and no key flag.
func TestEachCellDrawsAFreshRandomBaseKey(t *testing.T) {
	a, err := newCellKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newCellKey()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || a == ([16]byte{}) || a == [16]byte{0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a} {
		t.Fatalf("two cell keys %x and %x", a, b)
	}
}

// RIGS-06: the in-process LiveKit names the loopback sentinel as its STUN server, so it hands its
// participants no public STUN host.
func TestTheLocalSFUHandsOutNoPublicSTUNServer(t *testing.T) {
	yaml, err := localSFUConfig().YAML()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(yaml, "stun_servers:\n") || !strings.Contains(yaml, stunSentinel) {
		t.Fatalf("the -local-sfu YAML names no STUN sentinel:\n%s", yaml)
	}
}

// The parked -out item: the table header goes only into a new or empty file, so a re-run appends
// rows under the header that is already there.
func TestOutWritesTheHeaderOnlyIntoANewFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rows.md")
	for i, want := range []bool{true, false} {
		f, header, err := openOut(p)
		if err != nil {
			t.Fatal(err)
		}
		if header != want {
			t.Fatalf("open %d: header %v, want %v", i, header, want)
		}
		if _, err := f.WriteString("| row |\n"); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	if b, _ := os.ReadFile(p); string(b) != "| row |\n| row |\n" {
		t.Fatalf("file = %q", b)
	}
}

func TestParseNetDevAndProc(t *testing.T) {
	const netdev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 123456     100    0    0    0     0          0         0   654321     200    0    0    0     0       0          0
  eth0: 9000000   7000    0    0    0     0          0         0  1800000000 900000    0    0    0     0       0          0
`
	rx, tx, err := ParseNetDev(strings.NewReader(netdev), "eth0")
	if err != nil || rx != 9_000_000 || tx != 1_800_000_000 {
		t.Fatalf("ParseNetDev(eth0) = %d, %d, %v", rx, tx, err)
	}
	if _, _, err := ParseNetDev(strings.NewReader(netdev), "wlan0"); err == nil || !strings.Contains(err.Error(), `no interface "wlan0"`) {
		t.Fatalf("ParseNetDev(wlan0) = %v", err)
	}
	// comm may contain spaces and parentheses; utime and stime are fields 14 and 15.
	stat := []byte("4242 (dillad (x) y) S 1 4242 4242 0 -1 4194560 100 0 0 0 1500 250 0 0 20 0 30 0 100 2000000 5000 18446744073709551615")
	if ticks, err := ParseProcStat(stat); err != nil || ticks != 1750 {
		t.Fatalf("ParseProcStat = %d, %v, want 1750", ticks, err)
	}
	if rss, err := ParseVMRSS([]byte("Name:\tdillad\nVmRSS:\t  204800 kB\nThreads:\t40\n")); err != nil || rss != 204800*1024 {
		t.Fatalf("ParseVmRSS = %d, %v", rss, err)
	}
}

func TestRunCellRefusesAHoldShorterThanThirtySeconds(t *testing.T) {
	_, err := RunCell(t.Context(), Options{Hold: 29 * time.Second}, Cell{Name: "short", Participants: 1})
	if err == nil || !strings.Contains(err.Error(), "shorter than 30s") {
		t.Fatalf("RunCell(hold 29s) = %v, want the MinHold refusal", err)
	}
}

// 2 talkers + 1 sharer + 2 pure viewers through the real in-process LiveKit, decrypting everything.
// The ports come from sfutest.FreePorts (every LiveKit-booting package test binary runs concurrently
// under `go test ./...`). The hold is 6 s with AllowShortHold: livekit_packet_bytes moves in 5 s
// steps, so a window of at least 6 s always contains one step and the egress delta is never 0. The
// sharer publishes VP8 through the SFU, so the test runs in CI's non-race step (ruling I8).
func TestALoopbackCellDecryptsEverything(t *testing.T) {
	if raceEnabled {
		t.Skip("upstream livekit-server data race in updateRidsFromSDP; runs in the non-race step")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg := sfu.DefaultConfig()
	cfg.Port, cfg.UDPPort = sfutest.FreePorts(t)
	cfg.APISecret = strings.Repeat("r", 32)
	cfg.STUNServers = []string{stunSentinel} // RIGS-06
	srv, err := sfu.Start(ctx, cfg)
	if err != nil {
		t.Fatalf("sfu.Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	metrics := &http.Server{Handler: promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{})}
	go func() { _ = metrics.Serve(ln) }()
	t.Cleanup(func() { _ = metrics.Close() })

	testdata := filepath.Join("..", "..", "internal", "media", "testdata")
	o := Options{
		Rooms:   LocalRooms{URL: srv.URL(), HTTPURL: srv.HTTPURL(), APIKey: cfg.APIKey, APISecret: cfg.APISecret},
		Metrics: HTTPMetrics("http://"+ln.Addr().String()+"/metrics", ""), Files: LocalReader, Iface: "lo", DilladPID: os.Getpid(),
		VoiceFile:  filepath.Join(testdata, "tone.ogg"),
		ShareFiles: map[Layer]string{LayerQ: filepath.Join(testdata, "bars.ivf"), LayerH: filepath.Join(testdata, "bars.ivf"), LayerF: filepath.Join(testdata, "bars.ivf")},
		Warmup:     2 * time.Second, Hold: 6 * time.Second, AllowShortHold: true,
	}
	res, err := RunCell(ctx, o, Cell{Name: "loopback", Participants: 5, Talkers: 2, Sharers: 1, Layer: LayerF})
	if err != nil {
		t.Fatalf("RunCell: %v", err)
	}
	if res.Decrypted == 0 || res.DecryptOKPercent != 100 {
		t.Fatalf("decrypted %d, decrypt-ok %.2f %%, dropped %v", res.Decrypted, res.DecryptOKPercent, res.Dropped)
	}
	if res.EgressMbps <= 0 || res.RSSMiB <= 0 {
		t.Fatalf("egress %.3f Mbit/s, RSS %.1f MiB: the counters did not move", res.EgressMbps, res.RSSMiB)
	}
	// Every talker's mic is subscribed by the four others: talkers × (participants − 1).
	if res.AudioSubscriptions != 2*4 {
		t.Fatalf("audio subscriptions = %d, want 8", res.AudioSubscriptions)
	}
	if !strings.HasPrefix(res.Row("loopback", "2026-10-01", "abc1234"), "| loopback | 5 | ") {
		t.Fatalf("row = %s", res.Row("loopback", "2026-10-01", "abc1234"))
	}
}
