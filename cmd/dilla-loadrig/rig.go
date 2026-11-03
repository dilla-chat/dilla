package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	pmedia "github.com/pion/webrtc/v4/pkg/media"

	"github.com/dilla-chat/dilla/internal/media"
	"github.com/dilla-chat/dilla/internal/sframe"
)

// PromQLEgress is SFU egress as the capacity card defines it (G37): RTP-level bytes counted once —
// ciphertext, prefix, SFrame header and tag, the RTP header without the pacer's extensions, padding,
// probes and RTX — and no SRTP tag, RTCP, UDP/IP or TURN framing. It is node-wide, so every cell runs
// on an otherwise idle dillad. The rig computes the same rate from two scrapes of dillad's /metrics
// at least 30 s apart (the counter moves in 5 s steps): no Prometheus server runs beside the rig.
const PromQLEgress = `8 * sum(rate(livekit_packet_bytes{direction="outgoing"}[30s]))`

// PromQLIngress is the publishers' side of the same counter.
const PromQLIngress = `8 * sum(rate(livekit_packet_bytes{direction="incoming"}[30s]))`

// TableHeader is the result document's table head; Row writes one line of it.
const TableHeader = "| cell | participants | SFU egress Mbps (RTP) | SFU ingress Mbps (RTP) | wire tx Mbps | dillad CPU % | dillad RSS MiB | loss % | audio subscriptions | decrypt-ok % | location | date | commit |\n" +
	"|---|---|---|---|---|---|---|---|---|---|---|---|---|"

const (
	rigEpoch = 1
	userHZ   = 100 // USER_HZ, the unit of /proc/<pid>/stat utime and stime on Linux amd64 and arm64
)

// opusSilence is one 20 ms CELT silence frame: what a silent Opus encoder with DTX sends between
// its pauses, here once per 400 ms.
var opusSilence = []byte{0xf8, 0xff, 0xfe}

// Sample is one line of the Prometheus text exposition format.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// ParseExposition reads the text format dillad's /metrics serves. Comments and blank lines are
// skipped; a trailing timestamp is ignored.
func ParseExposition(r io.Reader) ([]Sample, error) {
	var out []Sample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var name, rest string
		labels := map[string]string{}
		if i := strings.IndexByte(line, '{'); i >= 0 {
			j := strings.LastIndexByte(line, '}')
			if j < i {
				return nil, fmt.Errorf("malformed sample %q", line)
			}
			var err error
			if labels, err = parseLabels(line[i+1 : j]); err != nil {
				return nil, fmt.Errorf("%q: %w", line, err)
			}
			name, rest = line[:i], strings.TrimSpace(line[j+1:])
		} else {
			f := strings.Fields(line)
			if len(f) < 2 {
				return nil, fmt.Errorf("malformed sample %q", line)
			}
			name, rest = f[0], strings.Join(f[1:], " ")
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			return nil, fmt.Errorf("sample %q has no value", line)
		}
		v, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			return nil, fmt.Errorf("sample %q: %w", line, err)
		}
		out = append(out, Sample{Name: name, Labels: labels, Value: v})
	}
	return out, sc.Err()
}

func parseLabels(s string) (map[string]string, error) {
	labels := map[string]string{}
	for s = strings.TrimSpace(s); s != ""; {
		eq := strings.IndexByte(s, '=')
		if eq < 0 || eq+1 >= len(s) || s[eq+1] != '"' {
			return nil, errors.New("a label is not key=\"value\"")
		}
		key := strings.TrimSpace(s[:eq])
		s = s[eq+2:]
		var b strings.Builder
		i := 0
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				if s[i] == 'n' {
					b.WriteByte('\n')
					continue
				}
			}
			b.WriteByte(s[i])
		}
		if i == len(s) {
			return nil, errors.New("an unterminated label value")
		}
		labels[key] = b.String()
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s[i+1:]), ","))
	}
	return labels, nil
}

// Sum adds every sample of name whose labels match; label values compare case-insensitively.
func Sum(samples []Sample, name string, match map[string]string) float64 {
	var total float64
next:
	for _, s := range samples {
		if s.Name != name {
			continue
		}
		for k, v := range match {
			if !strings.EqualFold(s.Labels[k], v) {
				continue next
			}
		}
		total += s.Value
	}
	return total
}

// ParseNetDev reads iface's receive and transmit byte counters from /proc/net/dev.
func ParseNetDev(r io.Reader, iface string) (rx, tx uint64, err error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(name) != iface {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 16 {
			return 0, 0, fmt.Errorf("/proc/net/dev: %s has %d fields, want 16", iface, len(f))
		}
		if rx, err = strconv.ParseUint(f[0], 10, 64); err != nil {
			return 0, 0, err
		}
		tx, err = strconv.ParseUint(f[8], 10, 64)
		return rx, tx, err
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	return 0, 0, fmt.Errorf("/proc/net/dev has no interface %q", iface)
}

// ParseProcStat returns utime + stime (fields 14 and 15) from /proc/<pid>/stat, in USER_HZ ticks.
func ParseProcStat(b []byte) (uint64, error) {
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, errors.New("/proc/<pid>/stat: no ')' after comm")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 13 {
		return 0, fmt.Errorf("/proc/<pid>/stat: %d fields after comm, want at least 13", len(f))
	}
	u, err := strconv.ParseUint(f[11], 10, 64)
	if err != nil {
		return 0, err
	}
	st, err := strconv.ParseUint(f[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return u + st, nil
}

// ParseVMRSS returns VmRSS from /proc/<pid>/status, in bytes.
func ParseVMRSS(b []byte) (uint64, error) {
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			break
		}
		kb, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb * 1024, nil
	}
	return 0, errors.New("/proc/<pid>/status has no VmRSS line")
}

// Reader fetches a file on the SFU host.
type Reader func(ctx context.Context, path string) ([]byte, error)

// LocalReader reads the rig host's own files: the SFU runs here (-local-sfu, or the dev box).
func LocalReader(_ context.Context, path string) ([]byte, error) {
	// #nosec G304 -- the rig passes fixed /proc paths from takeSnapshot.
	return os.ReadFile(path)
}

// SSHReader reads a file on the SFU host with `ssh <target> cat <path>`.
func SSHReader(target string) Reader {
	return func(ctx context.Context, path string) ([]byte, error) {
		// #nosec G204 -- target is an operator CLI value; path is a fixed /proc path.
		return exec.CommandContext(ctx, "ssh", target, "cat", path).Output()
	}
}

// MetricsSource returns dillad's current metrics.
type MetricsSource func(ctx context.Context) ([]Sample, error)

// HTTPMetrics scrapes a /metrics URL. A non-empty token is sent as `Authorization: Bearer <token>`
// (metrics.require_admin is on by default, and dilla-testhost takes the token from
// DILLA_METRICS_TOKEN); no error or log line ever carries it.
func HTTPMetrics(url, token string) MetricsSource {
	return func(ctx context.Context) ([]Sample, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		switch resp.StatusCode {
		case http.StatusOK:
			return ParseExposition(resp.Body)
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, fmt.Errorf("GET %s: %s: the scrape needs the host's scrape token (-metrics-token, or DILLA_METRICS_TOKEN in the rig's environment)", url, resp.Status)
		}
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
}

// RoomSource opens each cell's room and mints each participant's join token.
type RoomSource interface {
	// Open makes room exist for participants; the returned function removes it where it can.
	Open(ctx context.Context, room string, participants int) (func(), error)
	// Join returns the signalling URL and a token that lets identity publish and subscribe in room.
	Join(ctx context.Context, room, identity string) (url, token string, err error)
}

// LocalRooms is the in-process LiveKit of a -local-sfu run (and of the tests): RoomService with the
// SFU's own key and secret.
type LocalRooms struct {
	URL, HTTPURL      string
	APIKey, APISecret string
}

func (l LocalRooms) Open(ctx context.Context, room string, participants int) (func(), error) {
	rs := lksdk.NewRoomServiceClient(l.HTTPURL, l.APIKey, l.APISecret)
	// #nosec G115 -- RunCell bounds participants to 1..25.
	if _, err := rs.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: room, MaxParticipants: uint32(participants), EmptyTimeout: 300, DepartureTimeout: 20}); err != nil {
		return nil, fmt.Errorf("create room: %w", err)
	}
	return func() { _, _ = rs.DeleteRoom(context.WithoutCancel(ctx), &livekit.DeleteRoomRequest{Room: room}) }, nil
}

func (l LocalRooms) Join(_ context.Context, room, identity string) (string, string, error) {
	token, err := mintToken(l.APIKey, l.APISecret, room, identity)
	return l.URL, token, err
}

// TestHostRooms opens rooms through a dilla-testhost's control listener (POST /debug/sfu/token),
// reached through an ssh tunnel to its loopback. Those debug rooms are hidden from the instance's
// call-room sweep, which deletes every room a call did not open within 30 s — so a production dillad
// can never host a capacity cell (RIGS-01). The host's in-process LiveKit secret never leaves it:
// the route mints each token. A debug room is not deleted at the end of a cell (the route has no
// delete); LiveKit closes it once it is empty.
type TestHostRooms struct {
	Control string // e.g. http://127.0.0.1:8444, the ssh-tunnelled control listener
	Client  *http.Client
}

func (h TestHostRooms) token(ctx context.Context, room, identity string, create bool) (string, string, error) {
	body, err := json.Marshal(map[string]any{"room": room, "identity": identity, "create": create})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(h.Control, "/")+"/debug/sfu/token", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("test host: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", fmt.Errorf("test host: POST /debug/sfu/token: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("test host: %w", err)
	}
	if out.URL == "" || out.Token == "" {
		return "", "", errors.New("test host: POST /debug/sfu/token answered no url or token (was it started with -sfu?)")
	}
	return out.URL, out.Token, nil
}

func (h TestHostRooms) Open(ctx context.Context, room string, _ int) (func(), error) {
	if _, _, err := h.token(ctx, room, "dilla-loadrig-open", true); err != nil {
		return nil, err
	}
	return func() {}, nil
}

func (h TestHostRooms) Join(ctx context.Context, room, identity string) (string, string, error) {
	return h.token(ctx, room, identity, false)
}

// Options is one rig run's targets and inputs.
type Options struct {
	Rooms        RoomSource
	Metrics      MetricsSource
	Files        Reader
	Iface        string
	DilladPID    int
	VoiceFile    string
	ShareFiles   map[Layer]string
	Warmup, Hold time.Duration
	// AllowShortHold lets a test sample across less than MinHold. Only rig_test.go sets it: a real
	// measurement over a shorter window can fall between two of LiveKit's 5 s counter steps.
	AllowShortHold bool
}

// MinHold is the shortest sampling window a measurement may use: the capacity card reads
// sum(rate(…[30s])) and livekit_packet_bytes moves in 5 s steps.
const MinHold = 30 * time.Second

// Result is one cell's measured row.
type Result struct {
	Cell               Cell
	EgressMbps         float64
	IngressMbps        float64
	WireTxMbps         float64
	CPUPercent         float64
	RSSMiB             float64
	LossPercent        float64
	AudioSubscriptions int
	Decrypted          uint64
	EmptyFrames        uint64
	Dropped            map[string]uint64
	// DecryptOKPercent counts from each participant's join, warm-up included.
	DecryptOKPercent float64
	// Measured is true once both samples were taken: the row is real even when RunCell also
	// returns an error (a drop during the hold, a viewer that decrypted nothing).
	Measured bool
}

// Row is the result as one line of TableHeader's table.
func (r Result) Row(location, date, commit string) string {
	return fmt.Sprintf("| %s | %d | %.1f | %.1f | %.1f | %.0f | %.0f | %.2f | %d | %.2f | %s | %s | %s |",
		r.Cell.Name, r.Cell.Participants, r.EgressMbps, r.IngressMbps, r.WireTxMbps, r.CPUPercent, r.RSSMiB,
		r.LossPercent, r.AudioSubscriptions, r.DecryptOKPercent, location, date, commit)
}

type snapshot struct {
	at      time.Time
	metrics []Sample
	tx      uint64
	ticks   uint64
	rss     uint64
}

func takeSnapshot(ctx context.Context, o Options) (snapshot, error) {
	m, err := o.Metrics(ctx)
	if err != nil {
		return snapshot{}, fmt.Errorf("metrics: %w", err)
	}
	netdev, err := o.Files(ctx, "/proc/net/dev")
	if err != nil {
		return snapshot{}, err
	}
	_, tx, err := ParseNetDev(bytes.NewReader(netdev), o.Iface)
	if err != nil {
		return snapshot{}, err
	}
	stat, err := o.Files(ctx, fmt.Sprintf("/proc/%d/stat", o.DilladPID))
	if err != nil {
		return snapshot{}, err
	}
	ticks, err := ParseProcStat(stat)
	if err != nil {
		return snapshot{}, err
	}
	status, err := o.Files(ctx, fmt.Sprintf("/proc/%d/status", o.DilladPID))
	if err != nil {
		return snapshot{}, err
	}
	rss, err := ParseVMRSS(status)
	if err != nil {
		return snapshot{}, err
	}
	return snapshot{at: time.Now(), metrics: m, tx: tx, ticks: ticks, rss: rss}, nil
}

// audioSubscriptions reads LiveKit's current subscribed_total gauge. It is
// already the number of downtracks in the active room on an otherwise idle SFU.
func audioSubscriptions(samples []Sample) int {
	return int(Sum(samples, "livekit_track_subscribed_total", map[string]string{"kind": "audio"}))
}

// viewer holds one participant's decrypt counters, one media.Counters per publisher it decrypts
// (every rig publisher publishes exactly one track), keyed by the publisher's device.
type viewer struct {
	mu     sync.Mutex
	tracks map[[16]byte]*media.Counters
}

func newViewer() *viewer { return &viewer{tracks: map[[16]byte]*media.Counters{}} }

// counters is the publisher's Counters, made on first use.
func (v *viewer) counters(publisher [16]byte) *media.Counters {
	v.mu.Lock()
	defer v.mu.Unlock()
	c, ok := v.tracks[publisher]
	if !ok {
		c = &media.Counters{}
		v.tracks[publisher] = c
	}
	return c
}

// tally is a viewer's (or a cell's) decrypted frames, empty frames and drops by code.
type tally struct {
	decrypted, empty uint64
	perPublisher     map[[16]byte]uint64
	dropped          map[string]uint64
}

func (v *viewer) tally() tally {
	v.mu.Lock()
	defer v.mu.Unlock()
	t := tally{perPublisher: map[[16]byte]uint64{}, dropped: map[string]uint64{}}
	for dev, c := range v.tracks {
		d, dropped := c.Snapshot()
		t.decrypted += d
		t.empty += c.EmptyFrames()
		t.perPublisher[dev] = d
		for code, n := range dropped {
			t.dropped[code] += n
		}
	}
	return t
}

// verifyViewerActivity requires every participant to decrypt at least one frame of every other
// participant's published track during the measured hold, not only during warm-up and not just
// one track of several. What it still does not prove: that delivery was continuous across the hold
// (one frame per track suffices), and for a DTX publisher it is its encrypted 3-byte silence frames
// that move the count.
func verifyViewerActivity(c Cell, viewers []*viewer, before []tally) error {
	publishers := c.Talkers + c.DTX + c.Sharers
	for i, v := range viewers {
		now := v.tally()
		for j := range publishers {
			if j == i {
				continue
			}
			dev := deviceOf(j)
			if now.perPublisher[dev] <= before[i].perPublisher[dev] {
				return fmt.Errorf("%s: participant %d decrypted no frames of participant %d's track during the hold", c.Name, i, j)
			}
		}
	}
	return nil
}

// maxIncompleteDrops is how many key frames that lost their parameter-set packet (media.DropIncomplete,
// a transport loss and not a crypto failure) a hold tolerates; they stay in Result.Dropped.
const maxIncompleteDrops = 2

// holdDrops is an error naming every drop code other than "sif" (LiveKit's own injected frames)
// whose count grew between before and after: a frame the rig published that a viewer could not
// decrypt during the hold. Up to maxIncompleteDrops "incomplete" frames are tolerated; every
// E_SFRAME_* code still fails.
func holdDrops(name string, before, after map[string]uint64) error {
	var codes []string
	for code, n := range after {
		grew := n - before[code]
		if code == media.DropIncomplete && grew <= maxIncompleteDrops {
			continue
		}
		if code != "sif" && n > before[code] {
			codes = append(codes, fmt.Sprintf("%s ×%d", code, n-before[code]))
		}
	}
	if len(codes) == 0 {
		return nil
	}
	slices.Sort(codes)
	return fmt.Errorf("%s: frames dropped during the hold: %s", name, strings.Join(codes, ", "))
}

// newCellKey draws a fresh random base key for one cell (RIGS-11): every participant lives in this
// process, so no key is shared with anything else, and no (key, nonce) pair repeats across cells or
// runs although every cell's senders start their counters at 0.
func newCellKey() ([16]byte, error) {
	var k [16]byte
	if _, err := rand.Read(k[:]); err != nil {
		return k, fmt.Errorf("draw the cell's base key: %w", err)
	}
	return k, nil
}

func deviceOf(i int) [16]byte {
	var d [16]byte
	copy(d[:], "dilla-loadrig")
	// #nosec G115 -- a cell has at most 25 participants.
	d[14], d[15] = byte(i>>8), byte(i)
	return d
}

func mintToken(apiKey, apiSecret, room, identity string) (string, error) {
	yes, no := true, false
	at := auth.NewAccessToken(apiKey, apiSecret)
	at.SetIdentity(identity).SetValidFor(2 * time.Hour).SetVideoGrant(&auth.VideoGrant{
		RoomJoin: true, Room: room, CanPublish: &yes, CanSubscribe: &yes, CanPublishData: &no,
	})
	return at.ToJWT()
}

// join connects participant i, decrypting every track it is subscribed to with its own key ring
// and that track's own Counters.
func join(ctx context.Context, o Options, room string, i int, key [16]byte, roster []sframe.RosterEntry, v *viewer) (*lksdk.Room, error) {
	dev := deviceOf(i)
	url, token, err := o.Rooms.Join(ctx, room, hex.EncodeToString(dev[:]))
	if err != nil {
		return nil, err
	}
	ring := sframe.NewKeyRing(time.Now)
	ring.InstallEpoch(rigEpoch, key, roster, i)
	var mu sync.Mutex
	var r *lksdk.Room
	ready := make(chan struct{})
	cb := &lksdk.RoomCallback{ParticipantCallback: lksdk.ParticipantCallback{
		OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
			// protocol/05 receiver rules: a strict device_id identity (another participant in a
			// shared room must not end the run) and a kind that matches the source.
			from, codec, slot, ok := media.TrackBinding(rp.Identity(), track.Kind(), track.Codec().MimeType, pub.Source())
			if !ok {
				return
			}
			counters := v.counters(from)
			go func() {
				select {
				case <-ready:
				case <-ctx.Done():
					return
				}
				mu.Lock()
				sif := r.SifTrailer()
				mu.Unlock()
				_ = media.DecryptLoop(ctx, track, media.NewFrameDecryptor(ring, codec, from, slot, sif, counters))
			}()
		},
	}}
	rr, err := lksdk.ConnectToRoomWithToken(url, token, cb, lksdk.WithAutoSubscribe(true))
	if err != nil {
		return nil, fmt.Errorf("participant %d: %w", i, err)
	}
	mu.Lock()
	r = rr
	mu.Unlock()
	close(ready)
	return rr, nil
}

func publishDTX(ctx context.Context, r *lksdk.Room, enc *media.FrameEncryptor) error {
	track, err := lksdk.NewLocalTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, lksdk.WithFrameEncryptor(enc))
	if err != nil {
		return err
	}
	if _, err := r.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: "dtx", Source: livekit.TrackSource_MICROPHONE, Encryption: livekit.Encryption_CUSTOM,
	}); err != nil {
		return err
	}
	level := uint8(127) // RFC 6464: −127 dBov, silence
	go func() {
		t := time.NewTicker(400 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = track.WriteSample(pmedia.Sample{Data: opusSilence, Duration: 400 * time.Millisecond}, &lksdk.SampleWriteOptions{AudioLevel: &level})
			}
		}
	}()
	return nil
}

// RunCell measures one cell: create the room, connect and publish every participant, warm up, then
// sample the SFU host across the hold.
func RunCell(ctx context.Context, o Options, c Cell) (Result, error) {
	if o.Hold < MinHold && !o.AllowShortHold {
		return Result{}, fmt.Errorf("%s: hold %s is shorter than %s", c.Name, o.Hold, MinHold)
	}
	if c.Participants < 1 || c.Participants > 25 {
		return Result{}, fmt.Errorf("%s: participants must be between 1 and 25", c.Name)
	}
	if c.Talkers+c.DTX+c.Sharers > c.Participants {
		return Result{}, fmt.Errorf("%s: %d roles for %d participants", c.Name, c.Talkers+c.DTX+c.Sharers, c.Participants)
	}
	room := fmt.Sprintf("rig-%s-%d", c.Name, time.Now().UnixNano())
	closeRoom, err := o.Rooms.Open(ctx, room, c.Participants)
	if err != nil {
		return Result{}, err
	}
	defer closeRoom()
	key, err := newCellKey()
	if err != nil {
		return Result{}, err
	}
	defer clear(key[:])

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	roster := make([]sframe.RosterEntry, c.Participants)
	for i := range roster {
		roster[i] = sframe.RosterEntry{Leaf: uint16(i), Device: deviceOf(i)}
	}
	viewers := make([]*viewer, c.Participants)
	rooms := make([]*lksdk.Room, 0, c.Participants)
	defer func() {
		for _, r := range rooms {
			r.Disconnect()
		}
	}()
	for i := range c.Participants {
		viewers[i] = newViewer()
		r, err := join(cctx, o, room, i, key, roster, viewers[i])
		if err != nil {
			return Result{}, err
		}
		rooms = append(rooms, r)
	}
	for i, r := range rooms {
		sender, err := sframe.NewSender(key, uint16(i), rigEpoch, rigEpoch)
		if err != nil {
			return Result{}, err
		}
		switch {
		case i < c.Talkers:
			_, err = media.PublishOpusOgg(r, o.VoiceFile, media.NewFrameEncryptor(sender, sframe.Opus, sframe.Mic, 0))
		case i < c.Talkers+c.DTX:
			err = publishDTX(cctx, r, media.NewFrameEncryptor(sender, sframe.Opus, sframe.Mic, 0))
		case i < c.Talkers+c.DTX+c.Sharers:
			_, err = media.PublishVP8IVF(r, o.ShareFiles[c.Layer], media.NewFrameEncryptor(sender, sframe.VP8, sframe.ScreenVideo, 0), livekit.TrackSource_SCREEN_SHARE)
		}
		if err != nil {
			return Result{}, fmt.Errorf("participant %d publish: %w", i, err)
		}
	}

	time.Sleep(o.Warmup)
	baseline := make([]tally, len(viewers))
	held := map[string]uint64{}
	for i, v := range viewers {
		baseline[i] = v.tally()
		for code, n := range baseline[i].dropped {
			held[code] += n
		}
	}
	s0, err := takeSnapshot(ctx, o)
	if err != nil {
		return Result{}, err
	}
	time.Sleep(o.Hold)
	s1, err := takeSnapshot(ctx, o)
	if err != nil {
		return Result{}, err
	}

	dt := s1.at.Sub(s0.at).Seconds()
	delta := func(name string, match map[string]string) float64 {
		return Sum(s1.metrics, name, match) - Sum(s0.metrics, name, match)
	}
	res := Result{Cell: c, Dropped: map[string]uint64{}, Measured: true}
	res.EgressMbps = 8 * delta("livekit_packet_bytes", map[string]string{"direction": "outgoing"}) / dt / 1e6
	res.IngressMbps = 8 * delta("livekit_packet_bytes", map[string]string{"direction": "incoming"}) / dt / 1e6
	res.WireTxMbps = 8 * float64(s1.tx-s0.tx) / dt / 1e6
	res.CPUPercent = 100 * float64(s1.ticks-s0.ticks) / userHZ / dt
	res.RSSMiB = float64(s1.rss) / (1 << 20)
	if packets := delta("livekit_packet_total", nil); packets > 0 {
		res.LossPercent = 100 * delta("livekit_packet_loss_total", nil) / packets
	}
	res.AudioSubscriptions = audioSubscriptions(s0.metrics)
	var bad uint64
	for _, v := range viewers {
		t := v.tally()
		res.Decrypted += t.decrypted
		res.EmptyFrames += t.empty
		for code, n := range t.dropped {
			res.Dropped[code] += n
			if code != "sif" {
				bad += n
			}
		}
	}
	if res.Decrypted+bad > 0 {
		res.DecryptOKPercent = 100 * float64(res.Decrypted) / float64(res.Decrypted+bad)
	}
	if err := verifyViewerActivity(c, viewers, baseline); err != nil {
		return res, err
	}
	if err := holdDrops(c.Name, held, res.Dropped); err != nil {
		return res, err
	}
	return res, nil
}
