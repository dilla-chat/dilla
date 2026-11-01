package dillad

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"

	"github.com/livekit/protocol/livekit"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/blob"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/ops"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/sfu"
	"github.com/jonasthim/dilla/internal/store"
)

// SFU is the LiveKit control surface used when livekit.enabled: the call routes' SFU
// (token mint, room and permission control), its HTTP origin /rtc is proxied to, and the verifier of
// the room tokens it minted, which the /rtc join gate reads. Both the local and sidecar
// implementations satisfy it.
type SFU interface {
	api.CallTokens
	// HTTPURL is the SFU's signalling origin over HTTP, e.g. http://127.0.0.1:7880.
	HTTPURL() string
	// VerifyToken answers the identity, room and claims of a room-join token the SFU's key signed.
	VerifyToken(token string) (sfu.RoomToken, error)
}

// rtcRateClass is the bucket /rtc is metered on: the [limits.rate] unauth class's rate and burst,
// keyed per client address.
const rtcRateClass = "unauth"

// RTCGate admits device to the live call whose room is room and answers what it may hold there
// now — its current base permission — or a *server.Error refusal; *api.Calls is one. Admitted is
// told, once every check of the proxy passed, that the join goes through to the SFU: the call
// routes bind the session's leave to it and open its join window (M-2 of the integration
// re-review: a join the proxy refuses after AdmitRoom records nothing). resume says the join is a
// LiveKit signal resume of the session already in the room (sfu.JoinIsResume), not a new session.
type RTCGate interface {
	AdmitRoom(ctx context.Context, room string, device id.ID) (*livekit.ParticipantPermission, error)
	Admitted(ctx context.Context, room string, device id.ID, resume bool)
}

// planTwo is what Plan 2's handler groups are built over: the composition root's own collaborators,
// shared with Plan 1's routes rather than built a second time.
type planTwo struct {
	ctx      context.Context
	o        Options
	instance store.InstanceRow
	sessions *auth.Sessions
	limiter  *server.RateLimiter
	delivery *ds.DS
	gw       *gateway.Gateway
	blobs    *blob.Store
	uploads  *api.UploadMeter
	keys     api.FrankingKeys
	calls    api.CallsConfig
	diagnose func(context.Context) ops.Report
}

// mountPlanTwo mounts every handler group Plan 2 built in internal/api (protocol/09 § Communities
// through § Admin). The groups register their handlers bare; each is registered here through a view
// of the mux that puts the enrolled-session middleware and the [limits.rate] meter in front of every
// route (api.SessionRoute), the same two layers Plan 1's delivery-service routes sit behind. Every
// group that takes the delivery service gets the one *ds.DS, every group that takes a resolver the
// one api.Resolver, and the readable fan-out is the gateway's.
func mountPlanTwo(mux *server.Mux, p planTwo) (*api.Calls, *api.Roles) {
	cfg := p.o.Config
	repo, clk, log := p.o.Repo, p.o.Clock, p.o.Log
	res := api.NewResolver(repo)
	// P2-6: a group DM holds at most as many participants as its call can, so every participant
	// fits in the DM's call.
	maxGroupDM := cfg.LiveKit.MaxVoiceParticipants
	m := mux.Wrapped(api.SessionRoute(p.sessions, p.limiter))
	// An interface holding a nil *sfu.Server is not a nil interface, so the SFU is handed over only
	// when there is one; without it the call routes answer 501 for a call that would open.
	var tokens api.CallTokens
	if p.o.SFU != nil {
		tokens = p.o.SFU
	}
	// The DM call ring (web-3, L-GW-33) delivers op 53 through the gateway; a start that would ring
	// more users than a call holds rings nobody.
	ringer := api.NewRinger(p.gw, clk, log).WithMaxUsers(cfg.LiveKit.MaxVoiceParticipants)
	// The in-process LiveKit stamps a participant's join with the process's wall clock, never with
	// Options.Clock: the test host freezes and moves that one (advance_clock, its TOTP picks), and a
	// leave bound by mixing the two clocks proposes no Remove, so the leaf stays in the call group.
	calls := api.NewCalls(repo, res, tokens, p.calls, clk, log).WithCounters(p.o.Metrics).WithRinger(ringer).
		WithSFUClock(clock.System())
	// A cut device loses the relay with its call session; a nil *RelayRevocations must not become
	// a non-nil api.RelayRevoker.
	if p.o.Relay != nil {
		calls.WithRelay(p.o.Relay)
	}
	stats := api.NewCallStats(repo, calls, p.o.Metrics, clk).WithSessionResolver(p.sessions.ResolveHash)
	p.gw.WithCallStats(stats.AcceptCallStats)

	// The join is metered on the ("invite", client address) bucket GET /i/{code} is on, so a join
	// is not a way round the limit on guessing codes (task 5).
	// A kick, a leave, a ban, a group-DM removal and a user disable cut the user's connected call
	// sessions after their commit (dilla-media task 10).
	// A channel's create, update and delete, a community PATCH, a membership change and a role,
	// grant or overwrite change tell every member's live connections (op 54 community_changed), so
	// an open tab re-reads its lists; a delete tells the members it had, and a leave, kick or ban
	// tells the user it removed once.
	notify := api.NewStructureNotifier(p.gw, repo, log)
	calls.WithAFKNotify(notify)
	// AW2: the away roster's changes go out as op 55 to the AFK channel's audience, and a mark needs
	// the user to hold a gateway connection (Q2). The calls left live in AFK channels end in
	// New, after the delivery service starts (EndAFKCalls).
	calls.WithAFKAway(p.gw, p.gw.UserConnected).WithAFKDraining(p.gw.Draining)
	// R1: every community that predates web3.9, or lost its pointer, gets its built-in AFK channel
	// once, before any listener exists (dillad serve and dilla-testhost alike).
	// It logs its own summary and each community it could not finish; only a failure to list the
	// communities at all reaches here, and the start goes on.
	if _, err := api.EnsureAFKChannels(p.ctx, repo, p.delivery, clk, log); err != nil {
		log.ErrorContext(p.ctx, "afk backfill", "err", err)
	}
	api.NewCommunities(repo, p.delivery, clk, log).
		WithInviteMeter(p.limiter, cfg.Server.TrustedProxyCIDRs).WithCalls(calls).WithNotifier(notify).Register(m)
	roles := api.NewRoles(repo, clk, cfg.Auth.WebAuthn.RPID, log).WithDS(p.delivery).WithCalls(tokens, calls).WithNotifier(notify)
	channels := api.NewChannels(repo, p.delivery, clk, maxGroupDM, log).WithCalls(calls).WithNotifier(notify).WithRoles(roles)
	channels.Register(m)
	channels.RegisterMembers(m)
	roles.Register(m)
	api.NewBans(repo, p.delivery, clk, log).WithCalls(calls).WithNotifier(notify).Register(m)
	api.NewMemberTimeouts(repo, clk).WithNotifier(notify).Register(m)
	api.NewMemberWarnings(repo, clk).Register(m)
	api.NewInvites(repo, clk, "https://"+urlHost(cfg.Instance.Domain), log).Register(m)
	api.NewDMs(repo, p.delivery, clk, p.instance.InstanceID, maxGroupDM, log).Register(m)
	// A readable post is activity: it takes the sender out of the community's AFK list (AW2, Q1).
	api.NewReadable(repo, res, p.gw, p.keys, clk, log).WithRoles(roles).WithAFK(calls).Register(m)
	api.NewBlobs(repo, p.blobs, res, cfg.Blobs, clk, log).WithUploadMeter(p.uploads).Register(m)
	api.NewReports(repo, p.keys, clk, log).Register(m)
	// E8: the GIF picker. The Giphy key is a per-server setting; a server without one answers 404.
	api.NewGifs(repo, p.limiter, clk, log).Register(m)
	calls.Register(m)
	stats.Register(m)
	api.NewAdmin(repo, p.blobs, clk, log).WithMetrics(p.o.Metrics).
		WithDiagnostics(withCallLegs(p.diagnose, stats, cfg.TURN, p.o.Metrics)).WithCalls(calls).Register(m)
	return calls, roles
}

// callsConfig is what the call routes hand a client besides the token (task 16): the signalling URL,
// which is this instance's own origin because /rtc is proxied to the SFU (mountRTC), and, when
// turn.enabled, the relay URL and the shared secret its REST credentials are minted under. The relay
// is TURN over TLS on server.listen's port in the direct-TLS modes (the 443 demux), and plain TURN
// over TCP on turn.listen's port behind a proxy, which never carries TURN on 443.
func callsConfig(cfg *config.Config) (api.CallsConfig, error) {
	host := cfg.Instance.Domain
	if cfg.TLS.Mode == config.TLSModeACMEIP {
		host = cfg.Instance.PublicIP.String()
	}
	c := api.CallsConfig{
		LiveKitURL: "wss://" + urlHost(host), CredentialTTL: cfg.TURN.CredentialTTL.Value(),
		MaxVoiceParticipants: cfg.LiveKit.MaxVoiceParticipants, MaxPublishers: cfg.LiveKit.MaxPublishers,
		MaxAudioBitrateKbps: cfg.LiveKit.MaxAudioBitrateKbps, MaxShareBitrateKbps: cfg.LiveKit.MaxShareBitrateKbps,
		VP9: cfg.LiveKit.VP9,
	}
	if !cfg.TURN.Enabled {
		return c, nil
	}
	secret, err := ops.ReadSecret(cfg.TURN.SharedSecretFile)
	if err != nil {
		return api.CallsConfig{}, fmt.Errorf("dillad: turn.shared_secret_file: %w", err)
	}
	scheme, listen := "turns", cfg.Server.Listen
	if cfg.BehindProxy() {
		scheme, listen = "turn", cfg.TURN.Listen
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		// config.Validate refuses turn.enabled in behind_proxy with no turn.listen; a config that
		// never went through it offers no relay rather than one on a port nothing listens on.
		return c, nil //nolint:nilerr // no relay is the answer, not a failure
	}
	c.TURNSecret = secret
	c.TURNURLs = []string{fmt.Sprintf("%s:%s:%s?transport=tcp", scheme, urlHost(host), port)}
	if cfg.TURN.PublicURL != "" {
		// The operator knows what the world reaches: a proxy's port that differs from the listen
		// port, or a TLS-terminating proxy in front of the plaintext relay (turns:).
		c.TURNURLs = []string{cfg.TURN.PublicURL}
	}
	return c, nil
}

// urlHost is host as it is written inside a URL: an IPv6 literal in brackets, anything else as is.
func urlHost(host string) string {
	if a, err := netip.ParseAddr(host); err == nil && a.Is6() && !a.Is4In6() {
		return "[" + a.String() + "]"
	}
	return host
}

// mountRTC proxies LiveKit's signalling paths (/rtc, /rtc/validate, /rtc/v1, /rtc/v1/validate) to
// the in-process SFU, which binds livekit.bind_address (loopback by default) and so is reachable by
// a client only through this origin. The routes carry no dilla session; instead (DEV-25, DEV-44):
//
//   - only GET reaches LiveKit (405 otherwise): LiveKit joins a participant before its WebSocket
//     upgrade refuses a non-GET, and reads `publish` from a POST body too;
//   - the access token (the access_token query parameter, or a Bearer header) must be one the SFU's
//     key signed (403 E_FORBIDDEN), its identity a device id (403 E_FORBIDDEN), and that device a
//     current leaf of the live call whose room the token names (403 E_LEAF_NOT_CURRENT) — so a
//     device the call group removed cannot rejoin with a token LiveKit refreshed for it — whose user
//     still holds view_channel and connect, and the token may confer nothing beyond the device's
//     current base grant (403 E_FORBIDDEN; admitRTC);
//   - the client's signalling protocol must be the one dilla's clients speak, and a v1 SDK one
//     LiveKit's enum names (400 E_INVALID_REQUEST; admitRTC): LiveKit labels metrics with both;
//   - the `publish` query parameter is deleted (on the parsed values, so a percent-encoded spelling
//     goes too): it would open a second "<identity>#<x>" participant no gate admitted;
//   - CF-Connecting-IP and X-Real-IP are deleted and X-Forwarded-For replaced by the one client
//     address server.RealIP resolved behind the trusted proxies: LiveKit prefers the first two over
//     X-Forwarded-For, and nothing a client wrote reaches it.
//
// Every request is metered first, on the [limits.rate] unauth class (unauth_per_second,
// unauth_burst) keyed by the client address server.RealIP resolves (IPv6 by /64): the routes carry
// no session, and each request costs store reads and, with a repair pending, an SFU call. Past the
// burst it is 429 E_RATE_LIMITED with retry_after_ms.
//
// The upgrade itself is httputil.ReverseProxy's own.
func mountRTC(mux *server.Mux, s SFU, gate RTCGate, trusted []netip.Prefix, limiter *server.RateLimiter) error {
	if limiter == nil {
		return errors.New("dillad: the /rtc meter has no rate limiter")
	}
	upstream, err := url.Parse(s.HTTPURL())
	if err != nil {
		return fmt.Errorf("dillad: the SFU's URL %q: %w", s.HTTPURL(), err)
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.Host = r.In.Host
			if q := r.Out.URL.Query(); q.Has("publish") {
				q.Del("publish")
				r.Out.URL.RawQuery = q.Encode()
			}
			r.Out.Header.Del("CF-Connecting-IP")
			r.Out.Header.Del("X-Real-IP")
			r.Out.Header.Del("X-Forwarded-For")
			if a := server.RealIP(r.In, trusted); a.IsValid() {
				r.Out.Header.Set("X-Forwarded-For", a.String())
			}
		},
		// Stream at once: /rtc is a WebSocket.
		FlushInterval: -1,
	}
	rate := limiter.Config()
	class := server.Class{Name: rtcRateClass, PerSecond: rate.UnauthPerSecond, Burst: rate.UnauthBurst}
	gated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := limiter.Allow(class, server.RateKey(server.RealIP(r, trusted))); !ok {
			server.WriteError(w, server.RateLimitedAfter(wait))
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := admitRTC(r, s, gate); err != nil {
			server.WriteError(w, err)
			return
		}
		proxy.ServeHTTP(w, r)
	})
	mux.Handle("/rtc", gated)
	mux.Handle("/rtc/", gated)
	return nil
}

// admitRTC is the join gate in front of LiveKit. It reads the token exactly where LiveKit will
// (livekit-server pkg/service/auth.go: a non-empty Authorization header wins and must be a Bearer,
// else the access_token parameter), verifies it, admits the device through the gate, and then
// refuses a token that confers more than the device's current base permission: a token minted while
// it held speak or a sharing slot cannot be replayed after a demotion or an unshare.
//
// A resume is the one exception to the per-source comparison. LiveKit resumes a participant it still
// holds without re-reading the token's grants and refuses a resume of one it does not hold
// (roommanager.go:326-420), and its own refreshed token carries the participant's current promoted
// grant, so a sharer's signalling reconnect must still pass. The gate recognises a resume exactly as
// LiveKit does (sfu.JoinIsResume): on the v0 form, no join_request and reconnect "1" or "true"; on
// the v1 form livekit-client 2.22.3 uses, the Reconnect flag of the wrapped join request, parsed
// with LiveKit's own compression and size bounds. A join request LiveKit would refuse is refused
// here, 400 E_INVALID_REQUEST, before it reaches the SFU. Every other check — the token's signature,
// its device, the gate, every non-source right — applies to a resume as to a fresh join.
//
// Last, an admitted join whose signalling protocol is not sfu.ClientProtocol (the v0 `protocol`
// parameter, or client_info.protocol in a v1 join_request), or whose v1 client_info.sdk is not a
// value of LiveKit's enum, is 400 E_INVALID_REQUEST: LiveKit labels metric series with both as the
// client wrote them.
func admitRTC(r *http.Request, s SFU, gate RTCGate) error {
	q := r.URL.Query()
	token := q.Get("access_token")
	if h := r.Header.Get("Authorization"); h != "" {
		if !strings.HasPrefix(h, "Bearer ") {
			return server.Errorf(server.CodeForbidden, "the Authorization header is not a Bearer token")
		}
		token = strings.TrimPrefix(h, "Bearer ")
	}
	rt, err := s.VerifyToken(token)
	if err != nil {
		return server.Errorf(server.CodeForbidden, "the access token is not one this instance minted")
	}
	dev, err := id.Parse(rt.Identity)
	if err != nil {
		return server.Errorf(server.CodeForbidden, "the token's identity is not a device")
	}
	join, err := sfu.ReadJoin(q)
	if err != nil {
		return server.Errorf(server.CodeInvalidRequest, "%v", err)
	}
	allowed, err := gate.AdmitRoom(r.Context(), rt.Room, dev)
	if err != nil {
		return err
	}
	if err := sfu.TokenWithin(rt.Claims, allowed, !join.Resume); err != nil {
		return server.Errorf(server.CodeForbidden, "the access token grants more than your device holds now; start the call again")
	}
	// Last, once the device is admitted: LiveKit labels its session metrics with the client's
	// protocol number and its v1 SDK enum, so only what dilla's clients send passes (SFU-1).
	if err := join.CheckClient(); err != nil {
		return server.Errorf(server.CodeInvalidRequest, "%v", err)
	}
	gate.Admitted(r.Context(), rt.Room, dev, join.Resume)
	return nil
}
