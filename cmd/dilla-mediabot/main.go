// Command dilla-mediabot is the Go peer of the browser E2EE suite (task 21): it joins a LiveKit room
// as one call-group leaf, publishes the committed test media encrypted with dilla-sframe/1 through
// internal/media, decrypts what one expected device publishes, and prints one JSON report. It is a
// test tool: the release job builds cmd/dillad only, and TestDilladDoesNotImportTheMediaPackages keeps
// internal/media and internal/sframe out of dillad's graph.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"

	"github.com/dilla-chat/dilla/internal/media"
	"github.com/dilla-chat/dilla/internal/sframe"
)

type config struct {
	URL, Token   string
	BaseKey      [16]byte
	Leaf         uint16
	Epoch        uint64
	MinEpoch     uint64
	Roster       []sframe.RosterEntry
	Publish      []string
	Subscribe    bool
	ExpectDevice [16]byte
	Duration     time.Duration
	MediaDir     string
}

type report struct {
	Published       map[string]string `json:"published"`
	Decrypted       uint64            `json:"decrypted"`
	DecryptedByKind map[string]uint64 `json:"decrypted_by_kind"`
	EmptyFrames     uint64            `json:"empty_frames"`
	Dropped         map[string]uint64 `json:"dropped"`
}

const maxTokenBytes = 1 << 20

// readTokenFile reads a bounded token from a private regular file. It deliberately returns only
// static errors: neither the token nor its path belongs in diagnostics or test output.
func readTokenFile(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", errors.New("token file is unavailable or unsafe")
	}
	f := os.NewFile(uintptr(fd), "token")
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", errors.New("token file is unavailable or unsafe")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int64(st.Uid) != int64(os.Geteuid()) || info.Mode().Perm() != 0o600 {
		return "", errors.New("token file must be an owner-only 0600 regular file owned by the current user")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxTokenBytes+1))
	if err != nil {
		clear(raw)
		return "", errors.New("token file cannot be read")
	}
	defer clear(raw)
	if len(raw) > maxTokenBytes {
		return "", errors.New("token file is too large")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("token file must contain one non-empty token")
	}
	return token, nil
}

func deviceID(s string) ([16]byte, error) {
	d, ok := media.DeviceID(s)
	if !ok {
		return d, fmt.Errorf("%q is not 32 lowercase hex", s)
	}
	return d, nil
}

// readBaseKey reads the base key from path, or from stdin for "-": 32 lowercase hex and nothing
// else but surrounding white space. A file must be readable by its owner only (RIGS-10: a key on
// the command line is readable by every local user in /proc/<pid>/cmdline). No error names the key.
func readBaseKey(path string, stdin io.Reader) ([16]byte, error) {
	var key [16]byte
	var raw []byte
	switch path {
	case "":
		return key, errors.New(`-base-key-file is required ("-" reads the key from stdin)`)
	case "-":
		b, err := io.ReadAll(io.LimitReader(stdin, 256))
		if err != nil {
			clear(b)
			return key, fmt.Errorf("-base-key-file -: %w", err)
		}
		raw = b
	default:
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return key, fmt.Errorf("-base-key-file: %w", err)
		}
		f := os.NewFile(uintptr(fd), path)
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return key, fmt.Errorf("-base-key-file: %w", err)
		}
		if !info.Mode().IsRegular() {
			return key, errors.New("-base-key-file must be a regular file")
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || int64(st.Uid) != int64(os.Geteuid()) {
			return key, errors.New("-base-key-file must be owned by the current user")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return key, fmt.Errorf("-base-key-file %s is mode %#o: it must be readable by its owner only (chmod 600)", path, info.Mode().Perm())
		}
		if raw, err = io.ReadAll(io.LimitReader(f, 256)); err != nil {
			clear(raw)
			return key, fmt.Errorf("-base-key-file: %w", err)
		}
	}
	defer clear(raw)
	s := bytes.TrimSpace(raw)
	if len(s) != 32 {
		return key, errors.New("-base-key-file must hold the base key as 32 lowercase hex")
	}
	for _, b := range s {
		if b >= 'A' && b <= 'F' {
			return key, errors.New("-base-key-file must hold the base key as 32 lowercase hex")
		}
	}
	if _, err := hex.Decode(key[:], s); err != nil {
		return [16]byte{}, errors.New("-base-key-file must hold the base key as 32 lowercase hex")
	}
	return key, nil
}

func parseFlags(args []string, stdin io.Reader, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("dilla-mediabot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "LiveKit signalling URL (the calls route's livekit_url)")
	token := fs.String("token", "", "the room token")
	tokenFile := fs.String("token-file", "", "private file containing the room token (owner-only mode 0600)")
	baseKeyFile := fs.String("base-key-file", "", `file holding the call group's dilla-sframe/1 base key as 32 lowercase hex, mode 0600; "-" reads it from stdin`)
	leaf := fs.Int("leaf", -1, "this device's leaf index")
	epoch := fs.Uint64("epoch", 0, "the epoch the base key belongs to")
	minEpoch := fs.Uint64("min-epoch", 0, "N1: encrypt only at or after this epoch (default: -epoch)")
	roster := fs.String("roster", "", "the epoch's roster as leaf:device_hex,…")
	publish := fs.String("publish", "", "comma list of opus, vp8, h264")
	subscribe := fs.Bool("subscribe", false, "decrypt what -expect-device publishes")
	expect := fs.String("expect-device", "", "the device whose tracks are decrypted, 32 lowercase hex")
	duration := fs.Duration("duration", 10*time.Second, "how long to stay in the room after publishing")
	mediaDir := fs.String("media", filepath.Join("internal", "media", "testdata"), "directory with tone.ogg, bars.ivf and bars.h264")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	c := config{URL: *url, Epoch: *epoch, MinEpoch: *minEpoch, Subscribe: *subscribe, Duration: *duration, MediaDir: *mediaDir}
	if *token != "" && *tokenFile != "" {
		return config{}, errors.New("-token and -token-file are mutually exclusive")
	}
	if c.URL == "" {
		return config{}, errors.New("-url and -token are required")
	}
	if *tokenFile != "" {
		var err error
		c.Token, err = readTokenFile(*tokenFile)
		if err != nil {
			return config{}, err
		}
	} else {
		c.Token = *token
	}
	if c.Token == "" {
		return config{}, errors.New("-token or -token-file is required")
	}
	key, err := readBaseKey(*baseKeyFile, stdin)
	if err != nil {
		return config{}, err
	}
	c.BaseKey = key
	clear(key[:])
	if *leaf < 0 || *leaf > 65535 {
		return config{}, errors.New("-leaf must be 0…65535")
	}
	c.Leaf = uint16(*leaf)
	if c.MinEpoch == 0 {
		c.MinEpoch = c.Epoch
	}
	if c.MinEpoch > c.Epoch {
		return config{}, errors.New("-min-epoch must not exceed -epoch")
	}
	for _, entry := range strings.Split(*roster, ",") {
		if entry == "" {
			continue
		}
		l, d, ok := strings.Cut(entry, ":")
		n, nerr := strconv.ParseUint(l, 10, 16)
		dev, derr := deviceID(d)
		if !ok || nerr != nil || derr != nil {
			return config{}, fmt.Errorf("-roster entry %q is not leaf:device_hex", entry)
		}
		c.Roster = append(c.Roster, sframe.RosterEntry{Leaf: uint16(n), Device: dev})
	}
	if len(c.Roster) == 0 {
		return config{}, errors.New("-roster is required")
	}
	for _, p := range strings.Split(*publish, ",") {
		switch p {
		case "":
		case "opus", "vp8", "h264":
			c.Publish = append(c.Publish, p)
		default:
			return config{}, fmt.Errorf("-publish: unknown %q (opus, vp8, h264)", p)
		}
	}
	if c.Subscribe {
		if *expect == "" {
			return config{}, errors.New("-subscribe needs -expect-device")
		}
		if c.ExpectDevice, err = deviceID(*expect); err != nil {
			return config{}, fmt.Errorf("-expect-device: %w", err)
		}
	}
	return c, nil
}

// publishRetrying retries a video publication for up to 20 s: camera and screen need the call's
// publisher lease (task 10), which the test takes for this device after it has connected.
func publishRetrying(ctx context.Context, publish func() (*lksdk.LocalTrackPublication, error)) (*lksdk.LocalTrackPublication, error) {
	var last error
	for range 40 {
		p, err := publish()
		if err == nil {
			return p, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, last
}

// The /calls token contains only the base grant. The browser driver POSTs /share after the bot
// appears in the room; wait for dillad's permission push before attempting a video publication.
func waitPublishSource(ctx context.Context, room *lksdk.Room, source livekit.TrackSource) error {
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		perm := room.LocalParticipant.Permissions()
		for _, allowed := range perm.GetCanPublishSources() {
			if allowed == source {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("no permission to publish %s after 15s", source)
		case <-tick.C:
		}
	}
}

func run(ctx context.Context, c *config) (report, error) {
	defer clear(c.BaseKey[:])
	sender, err := sframe.NewSender(c.BaseKey, c.Leaf, c.Epoch, c.MinEpoch)
	if err != nil {
		return report{}, fmt.Errorf("sender: %w", err)
	}
	ring := sframe.NewKeyRing(time.Now)
	ring.InstallEpoch(c.Epoch, c.BaseKey, c.Roster, int(c.Leaf))
	clear(c.BaseKey[:])
	counters := map[livekit.TrackSource]*media.Counters{
		livekit.TrackSource_MICROPHONE:         {},
		livekit.TrackSource_CAMERA:             {},
		livekit.TrackSource_SCREEN_SHARE:       {},
		livekit.TrackSource_SCREEN_SHARE_AUDIO: {},
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var mu sync.Mutex
	var room *lksdk.Room
	ready := make(chan struct{})
	cb := &lksdk.RoomCallback{ParticipantCallback: lksdk.ParticipantCallback{
		OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
			if !c.Subscribe {
				return
			}
			// protocol/05 receiver rules: a strict device_id identity, and a kind that matches
			// the source (media.TrackBinding, shared with the load rig).
			dev, codec, slot, ok := media.TrackBinding(rp.Identity(), track.Kind(), track.Codec().MimeType, pub.Source())
			if !ok || dev != c.ExpectDevice {
				return
			}
			go func() {
				// The SIF trailer is the room's, known once the join completed.
				select {
				case <-ready:
				case <-ctx.Done():
					return
				}
				mu.Lock()
				sif := room.SifTrailer()
				mu.Unlock()
				_ = media.DecryptLoop(ctx, track, media.NewFrameDecryptor(ring, codec, dev, slot, sif, counters[pub.Source()]))
			}()
		},
	}}
	r, err := lksdk.ConnectToRoomWithToken(c.URL, c.Token, cb, lksdk.WithAutoSubscribe(c.Subscribe))
	if err != nil {
		return report{}, fmt.Errorf("connect: %w", err)
	}
	mu.Lock()
	room = r
	mu.Unlock()
	close(ready)
	defer r.Disconnect()

	published := map[string]string{}
	for _, what := range c.Publish {
		var p *lksdk.LocalTrackPublication
		switch what {
		case "opus":
			p, err = media.PublishOpusOgg(r, filepath.Join(c.MediaDir, "tone.ogg"),
				media.NewFrameEncryptor(sender, sframe.Opus, sframe.Mic, 0))
		case "vp8":
			if err = waitPublishSource(ctx, r, livekit.TrackSource_CAMERA); err != nil {
				return report{}, err
			}
			p, err = publishRetrying(ctx, func() (*lksdk.LocalTrackPublication, error) {
				return media.PublishVP8IVF(r, filepath.Join(c.MediaDir, "bars.ivf"),
					media.NewFrameEncryptor(sender, sframe.VP8, sframe.Camera, 0), livekit.TrackSource_CAMERA)
			})
		case "h264":
			if err = waitPublishSource(ctx, r, livekit.TrackSource_SCREEN_SHARE); err != nil {
				return report{}, err
			}
			p, err = publishRetrying(ctx, func() (*lksdk.LocalTrackPublication, error) {
				return media.PublishH264(r, filepath.Join(c.MediaDir, "bars.h264"),
					media.NewFrameEncryptor(sender, sframe.H264, sframe.ScreenVideo, 0), livekit.TrackSource_SCREEN_SHARE)
			})
		}
		if err != nil {
			return report{}, fmt.Errorf("publish %s: %w", what, err)
		}
		published[what] = p.SID()
	}

	select {
	case <-ctx.Done():
	case <-time.After(c.Duration):
	}
	byKind := map[string]uint64{}
	dropped := map[string]uint64{}
	var decrypted, emptyFrames uint64
	for source, counter := range counters {
		n, losses := counter.Snapshot()
		byKind[strings.ToLower(strings.TrimPrefix(source.String(), "SOURCE_"))] = n
		decrypted += n
		emptyFrames += counter.EmptyFrames()
		for reason, count := range losses {
			dropped[reason] += count
		}
	}
	return report{Published: published, Decrypted: decrypted, DecryptedByKind: byKind, EmptyFrames: emptyFrames, Dropped: dropped}, nil
}

func main() {
	c, err := parseFlags(os.Args[1:], os.Stdin, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dilla-mediabot:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep, err := run(ctx, &c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dilla-mediabot:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(rep); err != nil {
		fmt.Fprintln(os.Stderr, "dilla-mediabot:", err)
		os.Exit(1)
	}
}
