package dilladtest

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/dillad"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// ControlHandler is the /debug listener. It is a SEPARATE http.Handler, mounted on a second
// listener by the harness; it is never a route on the public mux.
//
// **The encoding is JSON, on both sides.** `/debug` is not a `/v1` route, so nothing requires it
// to be deterministic CBOR, and JSON is what a person debugging a scenario can read off the wire.
// The Rust side agrees: `control_post` in testkit/src/ds/remote.rs posts `{"seconds": <n>}` with
// `Content-Type: application/json`. TestTheControlListenerAdvancesTheClockAndReportsItBack is the
// round trip that pins it.
//
// It takes the Host rather than one *dillad.Server and its clock (deviation from the plan's
// `ControlHandler(s, clk)`): restore_snapshot replaces the server, and a handler bound to the first
// one would drive a stopped instance for the rest of the scenario.
func ControlHandler(h *Host) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /debug/clock", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Seconds int64 `json:"seconds"`
			Sweep   *bool `json:"sweep"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Seconds < 0 {
			http.Error(w, "the clock only moves forward", http.StatusBadRequest)
			return
		}
		if err := h.advance(r.Context(), time.Duration(body.Seconds)*time.Second, body.Sweep == nil || *body.Sweep); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// The host's fake clock, so a spec reads how far POST /debug/clock has moved it (a TOTP code is
	// computed for the instance's time, not the browser's).
	mux.HandleFunc("GET /debug/clock", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]int64{"now": h.Clock().Now().Unix()})
	})
	// The operator's reset (`dillad admin user reset-factors`) with this host's live seams: the
	// person's factors go, factor_owed is set, their sockets close and their calls are cut, and the
	// sessions stay (web3.6 D5). It stands in for the operator and never for the client.
	mux.HandleFunc("POST /debug/reset-factors", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Username string `json:"username"`
		}
		if !decode(w, r, &body) {
			return
		}
		u, err := h.Server().Repo().GetUserByUsername(r.Context(), body.Username)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such user", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := h.Server().ResetFactors(r.Context(), u.ID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /debug/kick", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Actor     string `json:"actor"`
			Target    string `json:"target"`
			Community string `json:"community"`
		}
		if !decode(w, r, &body) {
			return
		}
		target, err := id.Parse(body.Target)
		if err != nil {
			http.Error(w, "target: "+err.Error(), http.StatusBadRequest)
			return
		}
		if body.Community != "" {
			// The production kick: the /v1 route on the actor's authority (fix wave I6).
			community, err := id.Parse(body.Community)
			if err != nil {
				http.Error(w, "community: "+err.Error(), http.StatusBadRequest)
				return
			}
			actor, err := id.Parse(body.Actor)
			if err != nil {
				http.Error(w, "actor: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := h.KickFromCommunity(r.Context(), community, actor, target); err != nil {
				writeError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		n, err := h.Kick(r.Context(), target)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]int{"proposals": n})
	})
	mux.HandleFunc("POST /debug/admit", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Group  string `json:"group"`
			Device string `json:"device"`
		}
		if !decode(w, r, &body) {
			return
		}
		groupID, err := id.Parse(body.Group)
		if err != nil {
			http.Error(w, "group: "+err.Error(), http.StatusBadRequest)
			return
		}
		device, err := id.Parse(body.Device)
		if err != nil {
			http.Error(w, "device: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Every registration and KeyPackage hook already started runs to its end first: the
		// eager Adds a group's registration issues (Plan 2 task 7) are then in the delivery
		// service before this one is asked for, never racing it or the commit that follows.
		if err := h.Server().DrainHooks(r.Context()); err != nil {
			http.Error(w, "drain hooks: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// "Admit" means "make sure the device is admitted": an outstanding Add for it is already a
		// no-op in the delivery service, and a device an earlier commit already added (an eager Add
		// committed with another joiner's) is done, not refused.
		err = h.Server().DS().ProposeAdd(r.Context(), groupID, device, id.New())
		if err != nil && !errors.Is(err, ds.ErrAlreadyMember) {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /debug/admit-batch", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Group   string   `json:"group"`
			Devices []string `json:"devices"`
		}
		if !decode(w, r, &body) {
			return
		}
		groupID, err := id.Parse(body.Group)
		if err != nil {
			http.Error(w, "group: "+err.Error(), http.StatusBadRequest)
			return
		}
		devices := make([]id.ID, 0, len(body.Devices))
		for _, d := range body.Devices {
			device, err := id.Parse(d)
			if err != nil {
				http.Error(w, "device: "+err.Error(), http.StatusBadRequest)
				return
			}
			devices = append(devices, device)
		}
		// The join storm: at most MaxAddsPerCommit outstanding, the tail issued slice by slice as
		// the commits land (protocol/01 § Joining).
		if err := h.Server().DS().ProposeAddBatch(r.Context(), groupID, devices); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// A web3.8 administrator's AFK pick (E-AFK's upgrade case): the pointer moves to the channel
	// named, keeping the stored timeout, exactly as web3.8's PUT …/afk/settings wrote it. The next
	// start's backfill gives the community a new built-in AFK and leaves this channel ordinary.
	mux.HandleFunc("POST /debug/afk/legacy", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Community string `json:"community"`
			Channel   string `json:"channel"`
		}
		if !decode(w, r, &body) {
			return
		}
		community, err := id.Parse(body.Community)
		if err != nil {
			http.Error(w, "community: "+err.Error(), http.StatusBadRequest)
			return
		}
		channel, err := id.Parse(body.Channel)
		if err != nil {
			http.Error(w, "channel: "+err.Error(), http.StatusBadRequest)
			return
		}
		repo := h.Server().Repo()
		cfg, err := repo.GetCommunityAFK(r.Context(), community)
		if err != nil {
			writeError(w, err)
			return
		}
		if err := repo.SetCommunityAFK(r.Context(), community, store.CommunityAFK{ChannelID: &channel, TimeoutSeconds: cfg.TimeoutSeconds}); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /debug/mark-revoked", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Device string `json:"device"`
		}
		if !decode(w, r, &body) {
			return
		}
		device, err := id.Parse(body.Device)
		if err != nil {
			http.Error(w, "device: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := h.MarkRevoked(r.Context(), device); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /debug/channel", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Target     string   `json:"target"`
			Visibility string   `json:"visibility"`
			Mode       string   `json:"mode"`
			Members    []string `json:"members"`
			Community  string   `json:"community"`
		}
		if !decode(w, r, &body) {
			return
		}
		// A membership change orders after every hook already started: a registration's eager
		// Adds read the membership as the statements BEFORE this one left it, on every machine.
		if err := h.Server().DrainHooks(r.Context()); err != nil {
			http.Error(w, "drain hooks: "+err.Error(), http.StatusInternalServerError)
			return
		}
		target, err := id.Parse(body.Target)
		if err != nil {
			http.Error(w, "target: "+err.Error(), http.StatusBadRequest)
			return
		}
		visibility, ok := map[string]uint8{
			"private": VisibilityPrivate, "invite": VisibilityInvite,
			"discoverable": VisibilityDiscoverable,
		}[body.Visibility]
		if !ok {
			http.Error(w, "visibility is private, invite or discoverable", http.StatusBadRequest)
			return
		}
		mode, ok := map[string]uint8{"e2ee": ModeE2EE, "readable": ModeReadable}[body.Mode]
		if !ok {
			http.Error(w, "mode is e2ee or readable", http.StatusBadRequest)
			return
		}
		members := make([]id.ID, 0, len(body.Members))
		for _, m := range body.Members {
			u, err := id.Parse(m)
			if err != nil {
				http.Error(w, "members: "+err.Error(), http.StatusBadRequest)
				return
			}
			members = append(members, u)
		}
		if body.Community != "" {
			community, err := id.Parse(body.Community)
			if err != nil {
				http.Error(w, "community: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := h.PutCommunityChannel(r.Context(), community, target, visibility, mode, members); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.Channels().Set(target, visibility, mode)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if len(members) > 0 {
			if err := h.PutChannel(r.Context(), target, visibility, mode, members); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		h.Channels().Set(target, visibility, mode)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /debug/deny-view", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Actor   string `json:"actor"`
			Target  string `json:"target"`
			Channel string `json:"channel"`
		}
		if !decode(w, r, &body) {
			return
		}
		var ids [3]id.ID
		for i, v := range []string{body.Actor, body.Target, body.Channel} {
			parsed, err := id.Parse(v)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			ids[i] = parsed
		}
		if err := h.DenyView(r.Context(), ids[2], ids[0], ids[1]); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /debug/snapshot", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &body) {
			return
		}
		if err := h.Snapshot(r.Context(), body.Name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /debug/group-state", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Target string `json:"target"`
		}
		if !decode(w, r, &body) {
			return
		}
		target, err := id.Parse(body.Target)
		if err != nil {
			http.Error(w, "invalid target group id", http.StatusBadRequest)
			return
		}
		repo := h.Server().Repo()
		type groupState struct {
			ID           string `json:"id"`
			Epoch        uint64 `json:"epoch"`
			Seq          uint64 `json:"seq"`
			EpochUnknown bool   `json:"epoch_unknown"`
			Messages     int    `json:"messages"`
		}
		states := make([]groupState, 0, 1)
		var after id.ID
		for {
			groups, err := repo.ListGroupsForRetention(r.Context(), after, 256)
			if err != nil {
				writeError(w, err)
				return
			}
			for _, group := range groups {
				after = group.GroupID
				if group.TargetID != target || group.Kind != 0 || group.ClosedAt != nil {
					continue
				}
				messages, err := repo.ListAppMessages(r.Context(), group.GroupID, 0, 512)
				if err != nil {
					writeError(w, err)
					return
				}
				states = append(states, groupState{ID: group.GroupID.String(), Epoch: group.Epoch,
					Seq: group.Seq, EpochUnknown: group.EpochUnknown, Messages: len(messages)})
			}
			if len(groups) < 256 {
				break
			}
		}
		writeJSON(w, map[string]any{"groups": states})
	})
	mux.HandleFunc("POST /debug/restore", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &body) {
			return
		}
		if err := h.Restore(r.Context(), body.Name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /debug/seed", func(w http.ResponseWriter, r *http.Request) {
		var body []SeedRequest
		if !decode(w, r, &body) {
			return
		}
		out, err := SeedUsers(r.Context(), h.Server(), body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /debug/state", func(w http.ResponseWriter, r *http.Request) {
		state, err := State(r.Context(), h.Server())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, state)
	})
	mux.HandleFunc("GET /debug/bot-send-stats", func(w http.ResponseWriter, _ *http.Request) {
		h.botSendMu.Lock()
		attempts := append([]BotSendAttempt(nil), h.botSendAttempts...)
		h.botSendMu.Unlock()
		writeJSON(w, map[string]any{
			"requests": h.botSendRequests.Load(),
			"accepted": h.botSendAccepted.Load(),
			"refused":  h.botSendRefused.Load(),
			"attempts": attempts,
		})
	})
	mux.HandleFunc("GET /debug/group-heal-attempts", func(w http.ResponseWriter, _ *http.Request) {
		h.groupHealMu.Lock()
		defer h.groupHealMu.Unlock()
		writeJSON(w, map[string]any{"attempts": append([]GroupHealAttempt(nil), h.groupHealAttempts...)})
	})
	mux.HandleFunc("GET /debug/bot-interaction-heal-attempts", func(w http.ResponseWriter, _ *http.Request) {
		h.botRuntimeMu.Lock()
		defer h.botRuntimeMu.Unlock()
		writeJSON(w, map[string]any{"attempts": append([]BotInteractionHealAttempt(nil), h.botInteractionHealAttempts...)})
	})
	mux.HandleFunc("GET /debug/bot-runtime-errors", func(w http.ResponseWriter, _ *http.Request) {
		h.botRuntimeMu.Lock()
		defer h.botRuntimeMu.Unlock()
		writeJSON(w, map[string]any{"errors": append([]BotRuntimeError(nil), h.botRuntimeErrors...)})
	})
	mux.HandleFunc("GET /debug/conn", func(w http.ResponseWriter, r *http.Request) {
		var ids [2]id.ID
		for i, key := range []string{"device", "group"} {
			parsed, err := id.Parse(r.URL.Query().Get(key))
			if err != nil {
				http.Error(w, key+": "+err.Error(), http.StatusBadRequest)
				return
			}
			ids[i] = parsed
		}
		report, err := Conn(r.Context(), h.Server(), ids[0], ids[1])
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, report)
	})
	h.mountSFU(mux)
	return mux
}

// ConnReport is what GET /debug/conn answers: both sides of one device in one group, for a
// scenario whose client stopped receiving with its socket open. Gateway is the live connections'
// state and the fan-out list (gateway.Gateway.Debug); DS is what the delivery service's store says
// the device should have been sent.
type ConnReport struct {
	Gateway gateway.DebugReport `json:"gateway"`
	DS      ConnDSReport        `json:"ds"`
}

// ConnDSReport is the delivery service's side of ConnReport. Found is false when the group does
// not exist, and every other field is then zero.
type ConnDSReport struct {
	Found bool   `json:"found"`
	Epoch uint64 `json:"epoch"`
	Seq   uint64 `json:"seq"`
	// Leaves is every mls_members row of the device in the group, removed ones included.
	Leaves []ConnLeaf `json:"leaves"`
	// Outstanding counts the group's non-void proposals at its current epoch.
	Outstanding int `json:"outstanding"`
	// CursorSeq and CursorEpoch are the cursor the device last acknowledged (zero when it has
	// acknowledged nothing).
	CursorSeq   uint64 `json:"cursor_seq"`
	CursorEpoch uint64 `json:"cursor_epoch"`
}

// ConnLeaf is one mls_members row.
type ConnLeaf struct {
	Leaf         uint32  `json:"leaf"`
	AddedEpoch   uint64  `json:"added_epoch"`
	RemovedEpoch *uint64 `json:"removed_epoch"`
}

// Conn assembles a ConnReport. It only reads.
func Conn(ctx context.Context, s *dillad.Server, device, group id.ID) (ConnReport, error) {
	out := ConnReport{Gateway: s.Gateway().Debug(device, group), DS: ConnDSReport{Leaves: []ConnLeaf{}}}
	repo := s.Repo()
	row, err := repo.GetGroup(ctx, group)
	if errors.Is(err, store.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.DS.Found, out.DS.Epoch, out.DS.Seq = true, row.Epoch, row.Seq
	members, err := repo.ListMembers(ctx, group)
	if err != nil {
		return out, err
	}
	for _, m := range members {
		if m.DeviceID == device {
			out.DS.Leaves = append(out.DS.Leaves, ConnLeaf{
				Leaf: m.LeafIndex, AddedEpoch: m.AddedEpoch, RemovedEpoch: m.RemovedEpoch,
			})
		}
	}
	proposals, err := repo.ListProposals(ctx, group, row.Epoch, false)
	if err != nil {
		return out, err
	}
	for _, p := range proposals {
		if p.VoidAt == nil {
			out.DS.Outstanding++
		}
	}
	cursor, err := repo.GetCursor(ctx, device, group)
	if err != nil {
		return out, err
	}
	out.DS.CursorSeq, out.DS.CursorEpoch = cursor.LastSeq, cursor.LastEpoch
	return out, nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// writeError answers a delivery-service refusal with its own status and its E_* code at the head
// of the body, so a scenario's `expect_reject <code> kick …` matches it the way it matches a /v1
// refusal. Anything else is a 500.
func writeError(w http.ResponseWriter, err error) {
	var dsErr *ds.Error
	if errors.As(err, &dsErr) {
		http.Error(w, dsErr.Error(), dsErr.Status)
		return
	}
	var routeErr *RouteError
	if errors.As(err, &routeErr) {
		http.Error(w, routeErr.Error(), routeErr.Status)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Advance moves the instance clock and then runs, once and synchronously, the two pieces of
// background work the clock gates: the election watchdog (a round is overdue 2 s after it was sent)
// and the sweeper (proposal TTLs, both halves of retention, the heal window). Their own tickers are
// wall-clock and keep running; this makes a scenario's `advance_clock` observable by the very next
// statement instead of up to a minute later.
func (h *Host) Advance(ctx context.Context, d time.Duration) error {
	return h.advance(ctx, d, true)
}

// advance optionally defers the forced sweep for wall-clock-driven visual fixtures.
// Hooks and the watchdog still run; the delivery service's normal tickers are unchanged.
func (h *Host) advance(ctx context.Context, d time.Duration, sweep bool) error {
	s := h.Server()
	// The proposals a registration or a KeyPackage publish issues run after the answer, on hooks of
	// their own; they finish first, so what the scenario did before `advance_clock` has landed
	// before the clock moves (and `advance_clock 0s` is a barrier for them).
	if err := s.DrainHooks(ctx); err != nil {
		return fmt.Errorf("dilladtest: wait for the post-answer hooks: %w", err)
	}
	h.clk.Advance(d)
	s.DS().RunWatchdogOnce(ctx)
	if sweep {
		if _, err := s.DS().Sweep(ctx); err != nil {
			return fmt.Errorf("dilladtest: sweep after advancing the clock: %w", err)
		}
	}
	return nil
}

// Kick is the instance proposing the removal of `target` from every group it is a member of: an
// instance Remove proposal per group, which freezes the group (invariant 5) until a member commits
// it. It answers how many proposals it issued. In a call group the Remove goes by device
// (ProposeRemoveDevice), so a kick that finds an instance Remove of that leaf already standing is
// dropped rather than stacked (DEV-45); a member's own Remove of the leaf does not stop it.
//
// A target that is in no group any more is kicked again at the leaves an earlier kick named, so
// the delivery service's own "a Remove whose target leaf is already gone is dropped, not proposed"
// (invariant 6) answers — as the E_* refusal writeError relays — rather than this helper deciding
// it by finding nothing to do. The text-group Remove names the target's device
// (ProposeRemoveOf), so a leaf another device holds by now is refused the same way.
func (h *Host) Kick(ctx context.Context, target id.ID) (int, error) {
	s := h.Server()
	groups, err := s.Repo().GroupsForDevice(ctx, target)
	if err != nil {
		return 0, err
	}
	var leaves []kicked
	for _, groupID := range groups {
		members, err := s.Repo().ListMembers(ctx, groupID)
		if err != nil {
			return 0, err
		}
		for _, m := range members {
			if m.DeviceID == target && m.RemovedEpoch == nil {
				leaves = append(leaves, kicked{group: groupID, leaf: m.LeafIndex})
			}
		}
	}
	h.kickMu.Lock()
	if len(leaves) == 0 {
		leaves = h.kicked[target]
	} else {
		h.kicked[target] = leaves
	}
	h.kickMu.Unlock()
	n := 0
	for _, k := range leaves {
		row, err := s.Repo().GetGroup(ctx, k.group)
		if err != nil {
			return n, err
		}
		if row.Kind == 1 { // call: by device, as every production call path proposes (DEV-45)
			err = s.DS().ProposeRemoveDevice(ctx, k.group, target, id.New())
		} else {
			err = s.DS().ProposeRemoveOf(ctx, k.group, k.leaf, target, id.New())
		}
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// kicked is one (group, leaf) a kick proposed removing.
type kicked struct {
	group id.ID
	leaf  uint32
}

// SeedRequest is one account the harness wants created, with its devices' key material already
// generated by the caller: the control listener never mints a secret the test cannot see.
type SeedRequest struct {
	Username  string   `json:"username"`
	Display   string   `json:"display"`
	Bot       bool     `json:"bot"`
	UMKPub    string   `json:"umk_pub"`     // 64 hex
	SSKPub    string   `json:"ssk_pub"`     // 64 hex
	SigUMKSSK string   `json:"sig_umk_ssk"` // 128 hex
	Devices   []Device `json:"devices"`
}

// Device is one seeded device.
type Device struct {
	DeviceID   string `json:"device_id"` // 32 hex
	DSKPub     string `json:"dsk_pub"`   // 64 hex
	Tier       uint8  `json:"tier"`
	SignerTier uint8  `json:"signer_tier"`
	Credential string `json:"credential"` // hex of the CredentialIdentity CBOR
}

// SeedResponse carries the session tokens the scenario then uses.
type SeedResponse struct {
	UserID string            `json:"user_id"`
	Tokens map[string]string `json:"tokens"` // device_id hex -> session token
}

// SeedUsers creates the accounts, devices and sessions, each account in one transaction, through
// the same store and the same session minting the HTTP handlers use. It is the seeding convenience
// R14 keeps out of `dillad serve`: no `--insecure-test-bootstrap` flag exists.
func SeedUsers(ctx context.Context, s *dillad.Server, reqs []SeedRequest) ([]SeedResponse, error) {
	out := make([]SeedResponse, 0, len(reqs))
	for _, req := range reqs {
		res, err := seedOne(ctx, s, req)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

// user kinds of `users.kind` (internal/api's kindHuman and kindBot).
const (
	kindHuman = 0
	kindBot   = 1
)

// seedOne creates one account, its devices and one session per device.
//
// The caller supplies every public half and the credential blob; dilladtest never generates key
// material, because a harness that cannot see the private key it seeded cannot sign anything with
// it afterwards. The session is minted by auth.Sessions.NewDeviceSession, the call POST
// /v1/accounts makes for a freshly registered device, so no session exists here that the server
// would not itself have issued — which is why no `IssueForTest` was needed (the plan's B18).
func seedOne(ctx context.Context, s *dillad.Server, req SeedRequest) (SeedResponse, error) {
	now := s.Now().Unix()
	user := store.UserRow{
		ID: id.New(), Username: req.Username, Display: req.Display, Kind: kindHuman, Created: now,
	}
	if req.Bot {
		user.Kind = kindBot
	}
	// A seeded human owes a second factor exactly as one POST /v1/accounts registers does (web3.6
	// S1, fixture reachability): the control route has no option that marks an account satisfied.
	user.FactorOwed = user.Kind == kindHuman
	var err error
	if user.UMKPub, err = hexField("umk_pub", req.UMKPub, 32); err != nil {
		return SeedResponse{}, err
	}
	if user.SSKPub, err = hexField("ssk_pub", req.SSKPub, 32); err != nil {
		return SeedResponse{}, err
	}
	if user.SigUMKSSK, err = hexField("sig_umk_ssk", req.SigUMKSSK, 64); err != nil {
		return SeedResponse{}, err
	}
	devices := make([]store.DeviceRow, 0, len(req.Devices))
	for _, d := range req.Devices {
		deviceID, err := id.Parse(d.DeviceID)
		if err != nil {
			return SeedResponse{}, fmt.Errorf("dilladtest: device_id: %w", err)
		}
		dsk, err := hexField("dsk_pub", d.DSKPub, 32)
		if err != nil {
			return SeedResponse{}, err
		}
		credential, err := hex.DecodeString(d.Credential)
		if err != nil {
			return SeedResponse{}, fmt.Errorf("dilladtest: credential: %w", err)
		}
		devices = append(devices, store.DeviceRow{
			ID: deviceID, UserID: user.ID, DSKPub: dsk, Tier: d.Tier, SignerTier: d.SignerTier,
			CredentialBlob: credential, LastSeen: now, Created: now,
		})
	}

	res := SeedResponse{UserID: user.ID.String(), Tokens: map[string]string{}}
	err = s.Repo().Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateUser(ctx, user); err != nil {
			return err
		}
		for _, d := range devices {
			if err := tx.CreateDevice(ctx, d); err != nil {
				return err
			}
			token, err := s.Sessions().NewDeviceSession(ctx, tx, user.ID, d.ID, d.Tier)
			if err != nil {
				return err
			}
			res.Tokens[d.ID.String()] = token.Token
		}
		return nil
	})
	if err != nil {
		return SeedResponse{}, fmt.Errorf("dilladtest: seed %s: %w", req.Username, err)
	}
	return res, nil
}

func hexField(name, v string, n int) ([]byte, error) {
	b, err := hex.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("dilladtest: %s: %w", name, err)
	}
	if len(b) != n {
		return nil, fmt.Errorf("dilladtest: %s is %d bytes, want %d", name, len(b), n)
	}
	return b, nil
}

// DebugState is what GET /debug/state reports: the delivery service's counters (ds.DebugState,
// which dillad re-exports so the release binary never links this package) plus the two lists the
// scenario verbs expect_quarantined and expect_closed look themselves up in.
type DebugState struct {
	ds.DebugState
	// QuarantinedDevices is every device invariant 9 has quarantined, as 32 hex digits.
	QuarantinedDevices []string `json:"quarantined_devices"`
	// ClosedGroups is every group invariant 11 (or anything else) has closed, as 32 hex digits.
	ClosedGroups []string `json:"closed_groups"`
	// ExternalSenderPub is the instance's current external-sender Ed25519 public key, as 64 hex
	// digits: the key every `text` and `call` group carries in its `external_senders` extension
	// (protocol/01 § External senders). protocol/09's discovery document does not carry it, so a
	// scenario's clients read it here.
	ExternalSenderPub string `json:"external_sender_pub"`
}

// keyHistory is protocol/03 § Instance keys' `key_history`: [v, [[kind, key_id, public, secret,
// created, retired|null]]]. Only what ExternalSenderPub needs is decoded.
type keyHistory struct {
	_       struct{} `cbor:",toarray"`
	V       uint64
	Entries []keyHistoryEntry
}

type keyHistoryEntry struct {
	_       struct{} `cbor:",toarray"`
	Kind    uint64
	KeyID   []byte
	Public  []byte
	Secret  []byte
	Created uint64
	Retired *uint64
}

// externalSenderPub reads the current external-sender public key out of the instance row.
func externalSenderPub(row store.InstanceRow) (string, error) {
	var h keyHistory
	if err := cborx.Unmarshal(row.KeyHistory, &h); err != nil {
		return "", fmt.Errorf("dilladtest: instances.key_history: %w", err)
	}
	for _, e := range h.Entries {
		if e.Kind == 0 && e.Retired == nil && bytes.Equal(e.KeyID, row.ExternalSenderKeyID[:]) {
			return hex.EncodeToString(e.Public), nil
		}
	}
	return "", errors.New("dilladtest: the instance holds no current external-sender key")
}

// State reads the counters and walks the users and the groups for the two lists. A walk is fine
// for a test instance and is why this is a debug read and not a metric.
func State(ctx context.Context, s *dillad.Server) (DebugState, error) {
	counters, err := s.DebugState(ctx)
	if err != nil {
		return DebugState{}, err
	}
	out := DebugState{DebugState: counters, QuarantinedDevices: []string{}, ClosedGroups: []string{}}
	instance, err := s.Repo().GetInstance(ctx)
	if err != nil {
		return DebugState{}, err
	}
	if out.ExternalSenderPub, err = externalSenderPub(instance); err != nil {
		return DebugState{}, err
	}
	const page = 256
	var after id.ID
	for {
		users, err := s.Repo().ListUsers(ctx, after, page)
		if err != nil {
			return DebugState{}, err
		}
		for _, u := range users {
			devices, err := s.Repo().ListDevicesByUser(ctx, u.ID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return DebugState{}, err
			}
			for _, d := range devices {
				if d.QuarantinedAt != nil {
					out.QuarantinedDevices = append(out.QuarantinedDevices, d.ID.String())
				}
			}
			after = u.ID
		}
		if len(users) < page {
			break
		}
	}
	after = id.ID{}
	for {
		groups, err := s.Repo().ListGroupsForRetention(ctx, after, page)
		if err != nil {
			return DebugState{}, err
		}
		for _, g := range groups {
			if g.ClosedAt != nil {
				out.ClosedGroups = append(out.ClosedGroups, g.GroupID.String())
			}
			after = g.GroupID
		}
		if len(groups) < page {
			break
		}
	}
	return out, nil
}
