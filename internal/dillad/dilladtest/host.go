// Package dilladtest is the test-only control surface: an initialised instance, seeding, and a
// clock and a database the harness can move.
//
// It is never imported by cmd/dillad. `go list -deps ./cmd/dillad` asserts that in CI, because the
// alternative — a --insecure-test-bootstrap flag on `dillad serve` — is a production back door
// whose only protection is that nobody passes the flag.
package dilladtest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/pressly/goose/v3"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/dillad"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/obs"
	"github.com/jonasthim/dilla/internal/ops"
	"github.com/jonasthim/dilla/internal/sfu"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/jonasthim/dilla/internal/store/sqlite/migrations"
)

// InviteUses is how many accounts each of the harness's invites admits: the schema's own ceiling
// (`max_uses BETWEEN 1 AND 1000`). A scenario enrols every client through the invites (the instance
// is invite-only and has no other way in), and join_storm_256_batched alone enrols 1,001, so the
// harness mints Invites of them and the runner moves to the next when one is spent.
const (
	InviteUses = 1000
	Invites    = 4
)

// HostOptions configures NewHost. DataDir and CorePath are required.
type HostOptions struct {
	// DataDir holds the SQLite file, the wazero cache and the snapshots; use t.TempDir().
	DataDir string
	// CorePath is the wasm32-wasip1 build of dilla-core-wasi. Ignored when Wasm is set.
	CorePath string
	// Wasm is a runtime the caller owns and keeps across hosts; nil means NewHost compiles one
	// and Close closes it.
	Wasm *mlswasi.Runtime
	// Clock is the instance clock the control listener moves. nil means a fake clock at the
	// current wall-clock second: the clients mint their KeyPackages against the wall clock, and
	// an instance clock years behind it would find every KeyPackage not yet valid.
	Clock *clock.Fake
	// LogLevel is the instance's slog level; "" means warn.
	LogLevel string
	// LogOutput receives the instance log; nil means os.Stderr.
	LogOutput io.Writer
	// ProductionACL builds the instance with the seams production runs: dillad.New gets no ACL and
	// no Channels, so it wires api.ResolverACL (roles and channel overwrites) and
	// api.StructureChannels (the channels, communities and members tables). A scenario then has to
	// build real community structure (`channel … community=`) for any group it registers, and a
	// kick or a revocation (`kick … community=`, `join_many … revoke=`) goes through the /v1
	// routes. The default keeps AllowEveryone and ChannelModes, which the Plan 1 scenarios need
	// because their groups are bound to targets they invent.
	ProductionACL bool
	// SFU starts an in-process LiveKit beside the instance (testSFUConfig) and hands it to
	// dillad.Options.SFU, so the call routes mint real tokens and /rtc is proxied. SFUPort and
	// SFUUDPPort are its signalling and media ports; 0 means 7880 and 7882.
	SFU                 bool
	SFUPort, SFUUDPPort int
	// SFUNoInternalIP is SP-27's negative leg: Firefox cannot pair its non-loopback
	// local candidate with the loopback-only SFU candidate. Zero keeps the test default.
	SFUNoInternalIP bool
	SFUEnableAV1    bool
	// SFUDepartureTimeout is the rooms' departure_timeout in seconds (task-10 fix review N3: the
	// voice suite's host gives its rooms longer, so a test that reloads a browser alone in a room is
	// not raced by the room's own end on a slow runner); 0 keeps the instance default of 20 s.
	SFUDepartureTimeout uint32
	// ScrapeToken is the bearer token /metrics accepts (dillad.Options.ScrapeToken; dilla-testhost
	// takes it from DILLA_METRICS_TOKEN, as dillad does). Empty keeps dillad's default: a random
	// token nobody holds, so every scrape is refused.
	ScrapeToken string
	// WebRoot is a built client directory, its dilla-manifest.json included, served at the
	// instance's own origin instead of the embedded placeholder (dillad.Options.Web =
	// os.DirFS(WebRoot)); "" keeps the placeholder. A directory its manifest does not describe
	// fails NewHost and Restore with dillad's "dillad: web client: " error.
	WebRoot string
	// LiveKitURL is the signalling URL the call routes hand a client instead of the instance
	// domain's wss:// URL (dillad.Options.LiveKitURL); "" keeps that. dilla-testhost passes
	// ws://<listen> when it serves a web root beside an SFU, so a browser on loopback reaches /rtc.
	LiveKitURL string
	// WebAuthnOrigin is the origin a browser (or a virtual authenticator) reaches this host from,
	// for example "http://localhost:9999". When set, the relying party id is its hostname, rp_origins
	// is exactly [origin], and its host is added to http.trusted_origins so the gateway's upgrade
	// accepts it too. "" keeps the instance domain's own relying party.
	WebAuthnOrigin string
	// WireVersions is the wire version list the hello offers and GET /v1/instance advertises
	// (dillad.Options.WireVersions, web3.8 S1); nil keeps dillad's own list, {5, 4, 3, 2} since wire 5.
	// dilla-testhost sets it from -wire, so a browser test can meet an instance its client is too
	// old for.
	WireVersions []uint64
}

// AllowEveryone is the harness's channel ACL: every enrolled user is eligible for every group.
//
// Production injects api.ResolverACL (Plan 2 task 3), which answers from roles and channel
// overwrites and, for a group with no community, keeps Plan 1's ds.DenyUnlessMember: a user is
// eligible only where the instance can already see them in the group. The scenarios' groups are
// not bound to community channels, so under it no scenario could add a second user to anything;
// the harness — and only the harness — answers that one question itself. It is not a mock of the
// delivery service: invariant 4's other clauses (the signed device list, the structural
// validation, the GroupInfo epoch) still run.
type AllowEveryone struct{}

func (AllowEveryone) Eligible(context.Context, id.ID, id.ID) (bool, error) { return true, nil }

// Channel visibilities and text modes as ds.Channels reports them: invariant 1 refuses a text group
// for any visibility other than private, and for the readable mode.
const (
	VisibilityPrivate      uint8 = 0
	VisibilityInvite       uint8 = 1
	VisibilityDiscoverable uint8 = 2
	ModeE2EE               uint8 = 0
	ModeReadable           uint8 = 1
)

// ChannelModes is the harness's channel source (ds.Channels): the channels a scenario named with
// `channel <target> …`, and "no channel row" for every other target. It is how invariant 1's
// E_MODE_READABLE is reachable end to end from a scenario, which registers groups against
// targets it invents rather than against channels created through the API; the rule itself,
// ds.checkChannelMode, is the delivery service's own. Production injects api.StructureChannels
// over the channels table instead (Plan 2 task 2).
type ChannelModes struct {
	mu       sync.RWMutex
	channels map[id.ID][2]uint8
}

// Set records the channel under target.
func (c *ChannelModes) Set(target id.ID, visibility, mode uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.channels == nil {
		c.channels = map[id.ID][2]uint8{}
	}
	c.channels[target] = [2]uint8{visibility, mode}
}

// Channel is ds.Channels.
func (c *ChannelModes) Channel(_ context.Context, target id.ID) (visibility, mode uint8, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ch, ok := c.channels[target]
	if !ok {
		return 0, 0, ds.ErrNoChannel
	}
	return ch[0], ch[1], nil
}

// MayRegister is ds.Channels' registration ACL, and admits everyone, as AllowEveryone does for
// invariant 4: a scenario's groups are bound to targets it invents, with no community or
// membership rows behind them, and what the scenarios exercise is the delivery service's
// protocol, not the community structure. internal/api/dschannels_test.go pins the real ACL.
func (c *ChannelModes) MayRegister(context.Context, id.ID, ds.Binding) error { return nil }

// MayRecreate admits every re-creation of an epoch-unknown group, as MayRegister admits every
// registration (ds.GroupRecreation).
func (c *ChannelModes) MayRecreate(context.Context, id.ID, ds.Binding) error { return nil }

// Host is one initialised instance behind a stable public handler. Restore replaces the server
// underneath the handler, as `dillad restore` followed by a restart replaces the process, so the
// clients keep their base URL across it.
type Host struct {
	o       HostOptions
	cfg     *config.Config
	clk     *clock.Fake
	wasm    *mlswasi.Runtime
	owns    bool
	invites []string

	mu     sync.RWMutex
	server *dillad.Server

	// sfu is the in-process LiveKit when HostOptions.SFU; it outlives a Restore, as the SFU
	// outlives a database restore inside one dillad process.
	sfu *sfu.Server
	// debugRooms are the rooms the control listener's POST /debug/sfu/token opened, hidden from
	// the room sweep (harnessSFU).
	debugRooms debugRooms
	// webhookURL is where the in-process LiveKit posts its webhooks ("" without an SFU); hook serves
	// it and hookHandler verifies each event (both stopped with the SFU). current is the instance the
	// events go to: an atomic, so the receiver's workers never wait on mu while Close holds it.
	webhookURL                 string
	hook                       *http.Server
	hookHandler                http.Handler
	current                    atomic.Pointer[dillad.Server]
	botSendRequests            atomic.Uint64
	botSendAccepted            atomic.Uint64
	botSendRefused             atomic.Uint64
	botSendMu                  sync.Mutex
	botSendAttempts            []BotSendAttempt
	groupHealMu                sync.Mutex
	groupHealAttempts          []GroupHealAttempt
	botRuntimeMu               sync.Mutex
	botRuntimeErrors           []BotRuntimeError
	botInteractionHealAttempts []BotInteractionHealAttempt
	// withhold is the set of webhook event names (a map[string]struct{}, replaced whole) the receiver
	// drops instead of delivering: POST /debug/sfu/withhold, on the control listener only.
	withhold   atomic.Value
	withholdMu sync.Mutex

	// channels outlives a Restore, as the channels table outlives a restart.
	channels *ChannelModes

	// kicked remembers, per target device, the leaves the last kick of it named.
	kickMu sync.Mutex
	kicked map[id.ID][]kicked
}

// NewHost does what `dillad init` does — migrate, write the instance row with both instance
// keys, mint the invites — and then builds the server over that database with a fake clock.
func NewHost(ctx context.Context, o HostOptions) (*Host, error) {
	if o.DataDir == "" {
		return nil, errors.New("dilladtest: HostOptions.DataDir is required")
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.NewFake(time.Now().Truncate(time.Second))
	}
	h := &Host{
		o: o, clk: clk, cfg: hostConfig(o), kicked: map[id.ID][]kicked{}, channels: &ChannelModes{},
	}
	invites, err := bootstrap(ctx, h.cfg, clk.Now())
	if err != nil {
		return nil, err
	}
	h.invites = invites

	h.wasm = o.Wasm
	if h.wasm == nil {
		module, err := os.ReadFile(o.CorePath)
		if err != nil {
			return nil, fmt.Errorf("dilladtest: read the wasi core %q: %w", o.CorePath, err)
		}
		h.wasm, err = mlswasi.New(ctx, module, mlswasi.Options{
			CacheDir: filepath.Join(o.DataDir, "wazero-cache"),
			Now:      clk.Now,
		})
		if err != nil {
			return nil, fmt.Errorf("dilladtest: build the wasi runtime: %w", err)
		}
		h.owns = true
	}
	if o.SFU {
		if err := h.startSFU(ctx); err != nil {
			h.closeWasm(ctx)
			return nil, err
		}
	}
	server, err := h.newServer(ctx)
	if err != nil {
		h.stopSFU(ctx)
		h.closeWasm(ctx)
		return nil, err
	}
	h.server = server
	h.current.Store(server)
	return h, nil
}

// hostConfig is config.Default with what `dillad init` would write, and the one change a harness
// needs: rate limiting is off, because a scenario enrols up to a thousand devices from one
// loopback address and the per-IP registration bucket holds three.
func hostConfig(o HostOptions) *config.Config {
	c := config.Default()
	c.Instance.Domain = "dilla.test"
	c.Instance.DataDir = o.DataDir
	c.DB.Path = filepath.Join(o.DataDir, "dilla.db")
	c.Blobs.Dir = filepath.Join(o.DataDir, "blobs")
	c.TLS.Agreed = true
	c.Log.Level = o.LogLevel
	if c.Log.Level == "" {
		c.Log.Level = "warn"
	}
	c.Limits.Rate.Enabled = false
	// The harness runs no TURN relay, and writes no turn.shared_secret_file for one: the call
	// routes New mounts would otherwise read that file to mint relay credentials.
	c.TURN.Enabled = false
	if u, err := url.Parse(o.WebAuthnOrigin); o.WebAuthnOrigin != "" && err == nil && u.Hostname() != "" {
		c.Auth.WebAuthn.RPID = u.Hostname()
		c.Auth.WebAuthn.RPOrigins = []string{o.WebAuthnOrigin}
		c.HTTP.TrustedOrigins = append(c.HTTP.TrustedOrigins, u.Host)
	}
	c.Derive()
	return c
}

// bootstrap migrates the database and inserts the instance row and Invites invites, as `dillad init`
// does, except that each invite admits InviteUses accounts and none expires within a scenario.
func bootstrap(ctx context.Context, c *config.Config, now time.Time) ([]string, error) {
	if err := os.MkdirAll(c.Instance.DataDir, 0o700); err != nil {
		return nil, err
	}
	write, err := sqlite.OpenWrite(c.DB.Path)
	if err != nil {
		return nil, fmt.Errorf("dilladtest: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
	if err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("dilladtest: migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("dilladtest: migrate: %w", err)
	}
	read, err := sqlite.OpenRead(c.DB.Path)
	if err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("dilladtest: %w", err)
	}
	repo := sqlite.New(write, read)
	defer func() { _ = repo.Close() }()

	esPub, esPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	frank := make([]byte, 32)
	if _, err := rand.Read(frank); err != nil {
		return nil, err
	}
	unix := now.Unix()
	senderKeyID, frankingKeyID := id.New(), id.New()
	history, err := cborx.Marshal([]any{uint64(1), []any{
		[]any{uint64(0), senderKeyID, []byte(esPub), esPriv.Seed(), uint64(unix), nil}, //nolint:gosec // G115: a unix second or row id this server wrote, never negative
		[]any{uint64(1), frankingKeyID, []byte{}, frank, uint64(unix), nil},            //nolint:gosec // G115: a unix second or row id this server wrote, never negative
	}})
	if err != nil {
		return nil, err
	}
	codes := make([]string, 0, Invites)
	err = repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateInstance(ctx, store.InstanceRow{
			InstanceID: id.New(), ExternalSenderKeyID: senderKeyID, KeyHistory: history,
			FrankingKeyID: frankingKeyID, Generation: 1, PolicyVersion: 1, Created: unix,
		}); err != nil {
			return err
		}
		for range Invites {
			code, hash := auth.NewInviteCode()
			if err := tx.CreateInvite(ctx, store.InviteRow{
				ID: id.New(), CodeHash: hash, MaxUses: InviteUses, Created: unix,
				ExpiresAt: unix + int64(10*365*24*time.Hour/time.Second),
			}); err != nil {
				return err
			}
			codes = append(codes, code)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("dilladtest: bootstrap: %w", err)
	}
	return codes, nil
}

func (h *Host) newServer(ctx context.Context) (*dillad.Server, error) {
	out := h.o.LogOutput
	if out == nil {
		out = os.Stderr
	}
	o := dillad.Options{
		Config:      h.cfg,
		Clock:       h.clk,
		Wasm:        h.wasm,
		Log:         obs.NewLogger(h.cfg.Log, out),
		ScrapeToken: h.o.ScrapeToken,
		LiveKitURL:  h.o.LiveKitURL,
		// nil keeps dillad's own default list, {5, 4, 3, 2} since wire 5.
		WireVersions: h.o.WireVersions,
	}
	// Left nil under ProductionACL, so New wires api.ResolverACL and api.StructureChannels. (An
	// interface holding a nil *ChannelModes would not be nil; the fields are simply not set.)
	if !h.o.ProductionACL {
		o.ACL, o.Channels = AllowEveryone{}, h.channels
	}
	if h.o.WebRoot != "" {
		o.Web = os.DirFS(h.o.WebRoot)
	}
	// Set only when there is one: an interface holding a nil *sfu.Server is not a nil interface.
	// The instance sees it without the debug rooms, which its room sweep would otherwise delete.
	if h.sfu != nil {
		o.SFU = harnessSFU{Server: h.sfu, debug: &h.debugRooms}
	}
	return dillad.New(ctx, o)
}

// startSFU starts the harness LiveKit with the instance's own livekit.api_key and a fresh secret.
func (h *Host) startSFU(ctx context.Context) error {
	port, udp := h.o.SFUPort, h.o.SFUUDPPort
	if port == 0 {
		port = DefaultSFUPort
	}
	if udp == 0 {
		udp = DefaultSFUUDPPort
	}
	secret, err := newSFUSecret()
	if err != nil {
		return err
	}
	cfg := h.o.sfuConfig(port, udp, h.cfg.LiveKit.APIKey, secret)
	// The webhook receiver, as dillad's livekit.webhook_listen (cmd/dillad/front.go): bound before the
	// SFU starts, so LiveKit renders its URL; each verified event goes to the instance current then
	// (a Restore replaces it).
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("dilladtest: listen for the SFU's webhooks: %w", err)
	}
	cfg.WebhookURL = "http://" + ln.Addr().String() + sfu.WebhookPath
	s, err := sfu.Start(ctx, cfg)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("dilladtest: start the SFU on 127.0.0.1:%d (udp %d): %w", port, udp, err)
	}
	h.sfu = s
	h.webhookURL = cfg.WebhookURL
	h.hookHandler = sfu.NewWebhookHandler(cfg.APIKey, secret, func(ctx context.Context, ev *livekit.WebhookEvent) {
		if h.withheld(ev.GetEvent()) {
			return
		}
		if srv := h.current.Load(); srv != nil {
			srv.CallEvents().Handle(ctx, ev)
		}
	}, nil)
	mux := http.NewServeMux()
	mux.Handle("POST "+sfu.WebhookPath, h.hookHandler)
	h.hook = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = h.hook.Serve(ln) }()
	return nil
}

// stopSFU stops the SFU first, so nothing posts to the receiver as it shuts down and drains.
func (h *Host) stopSFU(ctx context.Context) {
	if h.sfu != nil {
		_ = h.sfu.Stop(ctx)
		h.sfu = nil
	}
	if h.hook != nil {
		_ = h.hook.Shutdown(ctx)
		if c, ok := h.hookHandler.(io.Closer); ok {
			_ = c.Close()
		}
		h.hook, h.hookHandler = nil, nil
	}
}

// PutCommunityChannel writes a text channel of community under target, with visibility and mode,
// and makes every user in members a member of the community. The community is created the first
// time, with members[0] as its owner and the @everyone role POST /v1/communities writes; the
// channel is created the first time too. Under ProductionACL this is what the resolver, and so
// invariant 1's registration ACL and invariant 4's eligibility, answer from. No proposal is
// issued: joining a community enters no group (protocol/01, a joiner enters by external commit).
func (h *Host) PutCommunityChannel(ctx context.Context, community, target id.ID, visibility, mode uint8, members []id.ID) error {
	if len(members) == 0 {
		return errors.New("dilladtest: a community channel needs at least its owner as a member")
	}
	repo := h.Server().Repo()
	now := h.clk.Now().Unix()
	return repo.Tx(ctx, func(tx store.Repository) error {
		if _, err := tx.GetCommunity(ctx, community); errors.Is(err, store.ErrNotFound) {
			if err := tx.CreateCommunity(ctx, store.CommunityRow{
				ID: community, Owner: members[0], Name: "harness", PolicyJSON: []byte("{}"),
				PolicyVersion: 1, Created: now,
			}); err != nil {
				return fmt.Errorf("dilladtest: community %s: %w", community, err)
			}
			if err := tx.PutRole(ctx, store.RoleRow{
				ID: id.New(), CommunityID: community, Name: "@everyone", Position: 0,
				Allow: uint64(api.DefaultEveryoneAllow), Created: now,
			}); err != nil {
				return fmt.Errorf("dilladtest: community %s @everyone: %w", community, err)
			}
		} else if err != nil {
			return err
		}
		for _, u := range members {
			if _, err := tx.GetMember(ctx, community, u); err == nil {
				continue
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err := tx.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: u, Joined: now}); err != nil {
				return fmt.Errorf("dilladtest: community %s member %s: %w", community, u, err)
			}
		}
		if _, err := tx.GetChannel(ctx, target); errors.Is(err, store.ErrNotFound) {
			c := community
			if err := tx.CreateChannel(ctx, store.ChannelRow{
				ID: target, CommunityID: &c, Kind: api.ChannelText, Mode: mode, Visibility: visibility,
				Name: "harness", SettingsJSON: []byte("{}"), HostPolicyVersion: 1, Created: now,
			}); err != nil {
				return fmt.Errorf("dilladtest: channel %s: %w", target, err)
			}
		} else if err != nil {
			return err
		}
		return nil
	})
}

// AsDevice sends one /v1 request through the public handler in device's name, with a session
// minted for it the way POST /v1/accounts mints one, and answers the status and the body. It is how
// the control listener drives a production route (a kick, an overwrite) on a scenario client's
// authority without that client's own session. Each call mints a session; a device holds at most
// auth's per-device cap of them, so a scenario uses this a handful of times per device, not in a
// loop.
func (h *Host) AsDevice(ctx context.Context, device id.ID, method, path string, body any) (int, []byte, error) {
	s := h.Server()
	dev, err := s.Repo().GetDevice(ctx, device)
	if err != nil {
		return 0, nil, fmt.Errorf("dilladtest: device %s: %w", device, err)
	}
	tok, err := s.Sessions().NewDeviceSession(ctx, s.Repo(), dev.UserID, dev.ID, dev.Tier)
	if err != nil {
		return 0, nil, fmt.Errorf("dilladtest: a session for %s: %w", device, err)
	}
	var rd io.Reader
	if body != nil {
		b, err := cborx.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequestWithContext(ctx, method, path, rd)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/cbor")
	}
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes(), nil
}

// RouteError is a /v1 refusal AsDevice's caller relays: the HTTP status and the E_* code of the
// CBOR error body.
type RouteError struct {
	Status int
	Code   string
	Detail string
}

func (e *RouteError) Error() string { return e.Code + ": " + e.Detail }

// routeResult turns an AsDevice answer into nil for a 2xx and a *RouteError otherwise.
func routeResult(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	var e []any
	if err := cborx.Unmarshal(body, &e); err != nil || len(e) < 2 {
		return &RouteError{Status: status, Code: "E_UNKNOWN", Detail: fmt.Sprintf("%x", body)}
	}
	code, _ := e[0].(string)
	detail, _ := e[1].(string)
	return &RouteError{Status: status, Code: code, Detail: detail}
}

// KickFromCommunity is `kick <actor> <target> community=`: DELETE
// /v1/communities/{community}/members/{target's user} on actor's authority, so the membership
// transaction and the Removes and voids after it are production's.
func (h *Host) KickFromCommunity(ctx context.Context, community, actor, target id.ID) error {
	dev, err := h.Server().Repo().GetDevice(ctx, target)
	if err != nil {
		return fmt.Errorf("dilladtest: kick target %s: %w", target, err)
	}
	status, body, err := h.AsDevice(ctx, actor, http.MethodDelete,
		"/v1/communities/"+community.String()+"/members/"+dev.UserID.String(), nil)
	if err != nil {
		return err
	}
	return routeResult(status, body)
}

// DenyView is `join_many … revoke=`: PUT /v1/channels/{channel}/overwrites/1/{target's user} with
// view_channel denied, on actor's authority, so the resolver's verdict changes and the route's own
// re-derivation (MaterialiseChannelMembers, and through it the delivery service) runs.
func (h *Host) DenyView(ctx context.Context, channel, actor, target id.ID) error {
	dev, err := h.Server().Repo().GetDevice(ctx, target)
	if err != nil {
		return fmt.Errorf("dilladtest: deny-view target %s: %w", target, err)
	}
	status, body, err := h.AsDevice(ctx, actor, http.MethodPut,
		"/v1/channels/"+channel.String()+"/overwrites/1/"+dev.UserID.String(),
		[]any{uint64(0), uint64(api.PermViewChannel)})
	if err != nil {
		return err
	}
	return routeResult(status, body)
}

// Channels is the channel-mode source the instance's invariant 1 reads; `channel <target> …`
// writes it through POST /debug/channel.
func (h *Host) Channels() *ChannelModes { return h.channels }

// PutChannel writes a community-less channel row under target (a group DM's shape, which is what a
// channel with no community is) and puts every member in its channel_members. It is what
// `channel … members=` asks for: api.SyncRegisteredGroup, which the composition root runs after a
// registration, reads the channels table rather than the ChannelModes source, and it populates the
// registered text group with the members' devices. A channel already written keeps its row and
// gains any member it lacked.
func (h *Host) PutChannel(ctx context.Context, target id.ID, visibility, mode uint8, members []id.ID) error {
	repo := h.Server().Repo()
	now := h.clk.Now().Unix()
	return repo.Tx(ctx, func(tx store.Repository) error {
		if _, err := tx.GetChannel(ctx, target); errors.Is(err, store.ErrNotFound) {
			if err := tx.CreateChannel(ctx, store.ChannelRow{
				ID: target, Kind: api.ChannelGroupDM, Mode: mode, Visibility: visibility,
				SettingsJSON: []byte("{}"), HostPolicyVersion: 1, Created: now,
			}); err != nil {
				return fmt.Errorf("dilladtest: channel %s: %w", target, err)
			}
		} else if err != nil {
			return err
		}
		for _, u := range members {
			if err := tx.PutChannelMember(ctx, target, u, now); err != nil {
				return fmt.Errorf("dilladtest: channel %s member %s: %w", target, u, err)
			}
		}
		return nil
	})
}

// MarkRevoked sets the device row's revoked_at and nothing else. A real revocation
// (auth.Sessions.RevokeDevice) also deletes the device's sessions in the same transaction; this
// leaves them, which is the window a request authenticated just before the revocation reaches the
// delivery service in. Invariant 4's external-joiner clause refuses a revoked joiner there.
func (h *Host) MarkRevoked(ctx context.Context, device id.ID) error {
	s := h.Server()
	if _, err := s.Repo().GetDevice(ctx, device); err != nil {
		return fmt.Errorf("dilladtest: mark %s revoked: %w", device, err)
	}
	return s.Repo().RevokeDevice(ctx, device, h.clk.Now().Unix())
}

// WebhookURL is where the in-process LiveKit posts its webhooks: the host's own loopback receiver,
// which hands each verified event to the current instance's call events. "" without an SFU.
func (h *Host) WebhookURL() string { return h.webhookURL }

// Server is the instance as it stands: a Restore replaces it.
func (h *Host) Server() *dillad.Server {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.server
}

// Handler is the public /v1 and /gateway surface. It is stable across Restore: each request goes
// to the server that is current when it arrives.
func (h *Host) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		track := r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/groups/") && strings.HasSuffix(r.URL.Path, "/bot/messages")
		trackHeal := (r.Method == http.MethodGet || r.Method == http.MethodPost) && strings.HasPrefix(r.URL.Path, "/v1/groups/") && strings.HasSuffix(r.URL.Path, "/heal")
		trackRuntime := strings.HasPrefix(r.URL.Path, "/v1/bot-runtime/")
		server := h.Server()
		if !track && !trackHeal && !trackRuntime {
			server.Handler().ServeHTTP(w, r)
			return
		}
		if trackHeal {
			wrapped := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			server.Handler().ServeHTTP(wrapped, r)
			parts := strings.Split(r.URL.Path, "/")
			attempt := GroupHealAttempt{Method: r.Method, Route: "/v1/groups/{group}/heal", Status: wrapped.status,
				ErrorCode: responseErrorCode(wrapped.body.Bytes())}
			if len(parts) == 5 {
				if group, err := id.Parse(parts[3]); err == nil {
					attempt.Group = group.String()
					if row, getErr := server.Repo().GetGroup(r.Context(), group); getErr == nil {
						attempt.Epoch = row.Epoch
						attempt.EpochUnknown = row.EpochUnknown
					}
				}
			}
			h.groupHealMu.Lock()
			h.groupHealAttempts = append(h.groupHealAttempts, attempt)
			if len(h.groupHealAttempts) > 64 {
				h.groupHealAttempts = append([]GroupHealAttempt(nil), h.groupHealAttempts[len(h.groupHealAttempts)-64:]...)
			}
			h.groupHealMu.Unlock()
			return
		}
		if trackRuntime {
			wrapped := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			server.Handler().ServeHTTP(wrapped, r)
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) == 8 && parts[1] == "v1" && parts[2] == "bot-runtime" && parts[5] == "interaction-groups" && parts[7] == "heal" {
				attempt := BotInteractionHealAttempt{Method: r.Method,
					Route:  "/v1/bot-runtime/installations/{installation}/interaction-groups/{group}/heal",
					Status: wrapped.status, ErrorCode: responseErrorCode(wrapped.body.Bytes())}
				if groupID, err := id.Parse(parts[6]); err == nil {
					attempt.Group = groupID.String()
					if group, err := server.Repo().GetGroup(r.Context(), groupID); err == nil {
						attempt.Epoch = group.Epoch
						attempt.EpochUnknown = group.EpochUnknown
					}
				}
				h.botRuntimeMu.Lock()
				h.botInteractionHealAttempts = append(h.botInteractionHealAttempts, attempt)
				if len(h.botInteractionHealAttempts) > 64 {
					h.botInteractionHealAttempts = append([]BotInteractionHealAttempt(nil), h.botInteractionHealAttempts[len(h.botInteractionHealAttempts)-64:]...)
				}
				h.botRuntimeMu.Unlock()
			}
			if wrapped.status >= 400 {
				h.botRuntimeMu.Lock()
				observation := BotRuntimeError{Method: r.Method, Path: redactedBotRuntimeRoute(r.URL.Path), Status: wrapped.status, ErrorCode: responseErrorCode(wrapped.body.Bytes())}
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ready") && wrapped.status == http.StatusConflict {
					observation.ErrorDetail = readinessConflictDetail(wrapped.body.Bytes())
				}
				if len(parts) == 8 && parts[1] == "v1" && parts[2] == "bot-runtime" && parts[5] == "interaction-groups" {
					if groupID, err := id.Parse(parts[6]); err == nil {
						if group, err := server.Repo().GetGroup(r.Context(), groupID); err == nil {
							observation.GroupEpoch = group.Epoch
							observation.GroupEpochUnknown = group.EpochUnknown
						}
					}
				}
				h.botRuntimeErrors = append(h.botRuntimeErrors, observation)
				if len(h.botRuntimeErrors) > 64 {
					h.botRuntimeErrors = append([]BotRuntimeError(nil), h.botRuntimeErrors[len(h.botRuntimeErrors)-64:]...)
				}
				h.botRuntimeMu.Unlock()
			}
			return
		}
		attempt := BotSendAttempt{Method: r.Method, Route: "/v1/groups/{group}/bot/messages"}
		h.botSendRequests.Add(1)
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) == 6 {
			if group, err := id.Parse(parts[3]); err == nil {
				attempt.Group = group.String()
			}
		}
		wrapped := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		server.Handler().ServeHTTP(wrapped, r)
		attempt.Status = wrapped.status
		attempt.ErrorCode = responseErrorCode(wrapped.body.Bytes())
		if group, err := id.Parse(attempt.Group); err == nil {
			if row, getErr := server.Repo().GetGroup(r.Context(), group); getErr == nil {
				attempt.Epoch = row.Epoch
				attempt.EpochUnknown = row.EpochUnknown
			}
		}
		h.botSendMu.Lock()
		h.botSendAttempts = append(h.botSendAttempts, attempt)
		if len(h.botSendAttempts) > 64 {
			h.botSendAttempts = append([]BotSendAttempt(nil), h.botSendAttempts[len(h.botSendAttempts)-64:]...)
		}
		h.botSendMu.Unlock()
		if wrapped.status >= 200 && wrapped.status < 300 {
			h.botSendAccepted.Add(1)
		} else {
			h.botSendRefused.Add(1)
		}
	})
}

func redactedBotRuntimeRoute(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) < 5 || parts[1] != "v1" || parts[2] != "bot-runtime" || parts[3] != "installations" {
		return path
	}
	parts[4] = "{installation}"
	if len(parts) == 7 && parts[5] == "groups" {
		parts[6] = "{operation}"
	}
	if len(parts) == 8 && parts[5] == "interaction-groups" {
		parts[6] = "{group}"
	}
	return strings.Join(parts, "/")
}

// BotRuntimeError is a testhost-only, body-free observation of HTTP failures
// on managed Bot runtime routes.
type BotRuntimeError struct {
	Method            string `json:"method"`
	Path              string `json:"path"`
	Status            int    `json:"status"`
	ErrorCode         string `json:"error_code"`
	ErrorDetail       string `json:"error_detail,omitempty"`
	GroupEpoch        uint64 `json:"group_epoch"`
	GroupEpochUnknown bool   `json:"group_epoch_unknown"`
}

// readinessConflictDetail exposes only fixed E_INVALID_REQUEST details from the
// Bot /ready transition; arbitrary protocol error text is discarded.
func readinessConflictDetail(body []byte) string {
	var fields []any
	if cborx.Unmarshal(body, &fields) != nil || len(fields) < 2 {
		return ""
	}
	code, codeOK := fields[0].(string)
	detail, detailOK := fields[1].(string)
	if !codeOK || code != "E_INVALID_REQUEST" || !detailOK {
		return ""
	}
	switch detail {
	case "installation revision or state changed":
		return "installation revision or state changed"
	case "installation prerequisites are still pending":
		return "installation prerequisites are still pending"
	case "managed role is missing":
		return "managed role is missing"
	case "request id was used for a different readiness operation":
		return "request id was used for a different readiness operation"
	default:
		return "unrecognized fixed readiness conflict"
	}
}

// BotInteractionHealAttempt is a body-free testhost observation of the restore-only scoped Bot heal path.
type BotInteractionHealAttempt struct {
	Method       string `json:"method"`
	Route        string `json:"route"`
	Group        string `json:"group"`
	Status       int    `json:"status"`
	ErrorCode    string `json:"error_code"`
	Epoch        uint64 `json:"epoch"`
	EpochUnknown bool   `json:"epoch_unknown"`
}

// BotSendAttempt is a redacted testhost observation of a managed Bot message route.
// It retains the route's group, status, and group state; request bytes are discarded.
type BotSendAttempt struct {
	Method       string `json:"method"`
	Route        string `json:"route"`
	Group        string `json:"group"`
	Status       int    `json:"status"`
	ErrorCode    string `json:"error_code"`
	Epoch        uint64 `json:"epoch"`
	EpochUnknown bool   `json:"epoch_unknown"`
}

// GroupHealAttempt is a testhost observation of human/native member healing.
type GroupHealAttempt struct {
	Method       string `json:"method"`
	Route        string `json:"route"`
	Group        string `json:"group"`
	Status       int    `json:"status"`
	ErrorCode    string `json:"error_code"`
	Epoch        uint64 `json:"epoch"`
	EpochUnknown bool   `json:"epoch_unknown"`
}

func responseErrorCode(body []byte) string {
	var fields []any
	if cborx.Unmarshal(body, &fields) != nil || len(fields) == 0 {
		return ""
	}
	code, _ := fields[0].(string)
	return code
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(body []byte) (int, error) {
	if w.body.Len() < 4096 {
		remaining := 4096 - w.body.Len()
		if len(body) > remaining {
			_, _ = w.body.Write(body[:remaining])
		} else {
			_, _ = w.body.Write(body)
		}
	}
	return w.ResponseWriter.Write(body)
}

// Invite is the plain codes of the harness's invites, comma-separated, as DILLA_TESTKIT_INVITE
// carries them: every scenario client redeems one, and the runner moves on when one is spent.
func (h *Host) Invite() string { return strings.Join(h.invites, ",") }

// Clock is the instance clock.
func (h *Host) Clock() *clock.Fake { return h.clk }

// Log is a logger for the host's own events, at the instance's level.
func (h *Host) Log() *slog.Logger {
	out := h.o.LogOutput
	if out == nil {
		out = os.Stderr
	}
	return obs.NewLogger(h.cfg.Log, out)
}

var snapshotName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (h *Host) snapshotPath(name string) (string, error) {
	if !snapshotName.MatchString(name) {
		return "", fmt.Errorf("dilladtest: snapshot name %q is not [A-Za-z0-9_-]{1,64}", name)
	}
	return filepath.Join(h.cfg.Instance.DataDir, "snapshots", name+".tar.gz"), nil
}

// Snapshot is `dillad backup`: ops.Backup writes the instance archive — database, blobs, keys —
// exactly as the CLI does, under the shared data-directory lock.
func (h *Host) Snapshot(ctx context.Context, name string) error {
	path, err := h.snapshotPath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: a path the harness itself chose
	if err != nil {
		return err
	}
	_, err = ops.Backup(ctx, h.cfg, h.server.Repo(), ops.BackupOptions{Out: f, IncludeBlobs: true, Clock: h.clk})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("dilladtest: snapshot %s: %w", name, err)
	}
	return nil
}

// Restore is `dillad restore` followed by `dillad serve`: the server stops, ops.Restore — the
// CLI's own code path — verifies the archive, swaps the data directory and runs invariant 11's
// restore SQL (generation, epoch-unknown groups, KeyPackage purge, live calls ended), and the new
// server finishes the pending restore at start through ds.FinishRestore, as serve does.
func (h *Host) Restore(ctx context.Context, name string) error {
	path, err := h.snapshotPath(name)
	if err != nil {
		return err
	}
	archive, err := os.Open(path) //nolint:gosec // G304: a path the harness itself chose
	if err != nil {
		return fmt.Errorf("dilladtest: no snapshot %s: %w", name, err)
	}
	defer func() { _ = archive.Close() }()
	h.mu.Lock()
	defer h.mu.Unlock()
	// No webhook reaches the instance being shut down: until the restored one is stored, events are dropped (task-10
	// review M9).
	h.current.Store(nil)
	if err := h.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("dilladtest: stop the instance for the restore: %w", err)
	}
	if _, err := ops.Restore(ctx, h.cfg, ops.RestoreOptions{From: archive, RemoveOld: true, Clock: h.clk}); err != nil {
		return fmt.Errorf("dilladtest: restore %s: %w", name, err)
	}
	server, err := h.newServer(ctx)
	if err != nil {
		return fmt.Errorf("dilladtest: start the restored instance: %w", err)
	}
	h.server = server
	h.current.Store(server)
	return nil
}

// Close stops the SFU and its webhook receiver, then the server and, when NewHost compiled it, the wasm runtime: no
// webhook reaches the instance as it shuts down (task-10 review M9).
func (h *Host) Close(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopSFU(ctx)
	h.current.Store(nil)
	err := h.server.Shutdown(ctx)
	h.closeWasm(ctx)
	return err
}

func (h *Host) closeWasm(ctx context.Context) {
	if h.owns {
		_ = h.wasm.Close(ctx)
	}
}
