package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bot v2 Task 9: the per-community command catalog the Bot publishes, its command-key publication,
// and the revision sync a client runs on connect. The catalog is not secret, but a member is only
// ever shown the commands they may use: the GET and the pushed op 61 are both filtered per member
// with the permission resolution every other route uses (catalogAudience).

const (
	// maxBotCatalogCommands bounds one published catalog; the built-in features contribute a few
	// dozen commands.
	maxBotCatalogCommands = 256
	maxBotCommandName     = 64
	maxBotCommandDesc     = 512
	// maxCommandKeyLifetime is dilla_core::bot::command::MAX_COMMAND_KEY_LIFETIME_SECS: a weekly
	// rotation plus the 24 h overlap. Clients refuse a longer-lived key, so dillad refuses it first.
	maxCommandKeyLifetime = 8 * 24 * 60 * 60
	// maxCommandKeyPublication is the core decoder's own bound.
	maxCommandKeyPublication = 256
)

// The catalog status states (GET …/bot/catalog element 3). State 1, degraded, is the Bot's own health report
// (bot_health.go, Bot v2 Task 17); a ready Bot that stops reporting is offline.
const (
	botStatusReady    uint64 = 0
	botStatusOffline  uint64 = 2
	botStatusDisabled uint64 = 3
	botStatusAbsent   uint64 = 4
)

// commandKeyDomain is dilla_core::bot::command::DOMAIN_COMMAND_KEY.
var commandKeyDomain = []byte("dilla bot command key v1")

// BotCatalogGateway is what the catalog publish needs of the gateway: whether a member holds a live
// connection, and delivery to that member's connections. *gateway.Gateway is one.
type BotCatalogGateway interface {
	DeliverUser(userID id.ID, f gateway.Frame)
	UserConnected(userID id.ID) bool
}

func registerBotCatalog(m *server.Mux, d Deps) {
	// Socket-only member routes, behind the chain the Bot admin routes use (the enrolled session, then the device
	// session's read bucket).
	m.SocketAuthed("GET /v1/communities/{community}/bot/catalog", d.socketChain(auth.ScopeEnrolled, dsClassRead), http.HandlerFunc(d.GetBotCatalog))
	m.SocketAuthed("GET /v1/bot/revisions", d.socketChain(auth.ScopeEnrolled, dsClassRead), http.HandlerFunc(d.GetBotRevisions))
	// Bot runtime routes, socket-only since Task 14, behind BotSessionRoute's chain (the enrolled session, whose Bot
	// allow-list admits only the ready Bot of the path's community, then the write bucket).
	m.SocketAuthed("PUT /v1/bot-runtime/communities/{community}/catalog", botSocketChain(d.Sessions, d.Limiter, "PUT /v1/bot-runtime/communities/{community}/catalog"), http.HandlerFunc(d.PutBotRuntimeCatalog))
	m.SocketAuthed("PUT /v1/bot-runtime/communities/{community}/command-key", botSocketChain(d.Sessions, d.Limiter, "PUT /v1/bot-runtime/communities/{community}/command-key"), http.HandlerFunc(d.PutBotRuntimeCommandKey))
}

// botCommand is one validated catalog entry: its 7-element array exactly as the Bot sent it, and
// the two fields dillad reads.
type botCommand struct {
	raw  cbor.RawMessage
	name string
	perm Bits
}

// decodeBotCatalog validates a commands list, [[name, description, operation, args_shape,
// permission_bit, needs_bot_channel, handler_kind]…]. args_shape follows validArgsShape's grammar,
// handler_kind is any uint (0 built-in; the rest reserved for outgoing integrations) and is kept as
// sent. needs_bot_channel is uint 0 or 1: the subset has no boolean.
func decodeBotCatalog(body []byte) ([]botCommand, error) {
	invalid := func(msg string) error { return server.Errorf(server.CodeInvalidRequest, "%s", msg) }
	var raw []cbor.RawMessage
	if err := cborx.ExpectMajor(body, cborx.MajorArray); err != nil {
		return nil, invalid("the catalog is an array of commands")
	}
	if err := cborx.Unmarshal(body, &raw); err != nil {
		return nil, invalid("the catalog is not deterministic CBOR")
	}
	if len(raw) > maxBotCatalogCommands {
		return nil, invalid("the catalog holds too many commands")
	}
	out := make([]botCommand, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, c := range raw {
		var f []cbor.RawMessage
		if cborx.ExpectMajor(c, cborx.MajorArray) != nil || cborx.Unmarshal(c, &f) != nil || len(f) != 7 {
			return nil, invalid("a command is [name, description, operation, args_shape, permission_bit, needs_bot_channel, handler_kind]")
		}
		name, ok := rawText(f[0])
		if !ok || name == "" || len(name) > maxBotCommandName {
			return nil, invalid("a command name is 1 to 64 bytes of text")
		}
		if seen[name] {
			return nil, invalid("a command name appears twice")
		}
		seen[name] = true
		if desc, ok := rawText(f[1]); !ok || len(desc) > maxBotCommandDesc {
			return nil, invalid("a command description is at most 512 bytes of text")
		}
		if _, ok := rawUintOf(f[2]); !ok {
			return nil, invalid("a command operation is a uint")
		}
		if err := validArgsShape(f[3]); err != nil {
			return nil, err
		}
		perm, ok := rawUintOf(f[4])
		if !ok || Bits(perm)&^PermAll != 0 {
			return nil, invalid("a command permission_bit names only defined permissions")
		}
		if needs, ok := rawUintOf(f[5]); !ok || needs > 1 {
			return nil, invalid("needs_bot_channel is uint 0 or 1")
		}
		if _, ok := rawUintOf(f[6]); !ok {
			return nil, invalid("handler_kind is a uint")
		}
		out = append(out, botCommand{raw: c, name: name, perm: Bits(perm)})
	}
	return out, nil
}

// The argument type codes of args_shape. They name the kinds of value a member supplies, which the
// client encodes as dilla_core::bot::command::CommandArg (text, uint, bytes, null): text is a
// string, uint a number, user and channel a 16-byte id the member picks (the client resolves it;
// the catalog itself never carries one), duration a uint of seconds, bool a uint 0 or 1.
const (
	argText uint64 = iota
	argUint
	argUser
	argChannel
	argDuration
	argBool
	argTypeCount
)

const (
	maxCommandArgs   = 16
	maxCommandArgLen = 32
)

// validArgsShape checks args_shape against its one grammar: an array of at most 16 arguments, each
// [name(text, 1-32 bytes, unique), type_code(uint, one of the codes above), required(uint 0 or 1)].
// Nothing else is admitted, so a Bot cannot carry an id or free bytes to members in a catalog.
func validArgsShape(b cbor.RawMessage) error {
	bad := server.Errorf(server.CodeInvalidRequest, "args_shape is at most 16 [name(text 1-32), type_code(0 text, 1 uint, 2 user, 3 channel, 4 duration, 5 bool), required(uint 0|1)]")
	var args []cbor.RawMessage
	if cborx.ExpectMajor(b, cborx.MajorArray) != nil || cborx.Unmarshal(b, &args) != nil || len(args) > maxCommandArgs {
		return bad
	}
	seen := make(map[string]bool, len(args))
	for _, a := range args {
		var f []cbor.RawMessage
		if cborx.ExpectMajor(a, cborx.MajorArray) != nil || cborx.Unmarshal(a, &f) != nil || len(f) != 3 {
			return bad
		}
		name, ok := rawText(f[0])
		if !ok || name == "" || len(name) > maxCommandArgLen || seen[name] {
			return bad
		}
		seen[name] = true
		if code, ok := rawUintOf(f[1]); !ok || code >= argTypeCount {
			return bad
		}
		if req, ok := rawUintOf(f[2]); !ok || req > 1 {
			return bad
		}
	}
	return nil
}

func rawText(b cbor.RawMessage) (string, bool) {
	var s string
	if cborx.ExpectMajor(b, cborx.MajorText) != nil || cborx.Unmarshal(b, &s) != nil || !utf8.ValidString(s) {
		return "", false
	}
	return s, true
}

func rawUintOf(b cbor.RawMessage) (uint64, bool) {
	var n uint64
	if cborx.ExpectMajor(b, cborx.MajorUint) != nil || cborx.Unmarshal(b, &n) != nil {
		return 0, false
	}
	return n, true
}

// catalogChannel is one channel's overwrites, read once per catalogAudience.
type catalogChannel struct {
	overwrites []store.OverwriteRow
	afk        bool
}

// catalogAudience is what every member's filter reads, loaded once per community: the roles and
// owner (a Snapshot without the member's grants) and, when a command needs a channel-scoped
// permission, the overwrites of every text and voice channel.
type catalogAudience struct {
	base     Snapshot
	channels []catalogChannel
}

func loadCatalogAudience(ctx context.Context, repo store.Repository, cid id.ID, cmds []botCommand) (*catalogAudience, error) {
	base, err := loadCommunitySnapshotBase(ctx, repo, cid)
	if err != nil {
		return nil, err
	}
	a := &catalogAudience{base: base}
	channelScoped := false
	for _, c := range cmds {
		if c.perm&^communityBits != 0 {
			channelScoped = true
			break
		}
	}
	if !channelScoped {
		return a, nil
	}
	channels, err := repo.ListChannels(ctx, cid)
	if err != nil {
		return nil, err
	}
	afk, err := afkPointerOf(ctx, repo, cid)
	if err != nil {
		return nil, err
	}
	for _, ch := range channels {
		if ch.DeletedAt != nil || (ch.Kind != ChannelText && ch.Kind != ChannelVoice) {
			continue
		}
		ow, err := repo.ListOverwrites(ctx, ch.ID)
		if err != nil {
			return nil, err
		}
		a.channels = append(a.channels, catalogChannel{overwrites: ow, afk: afk.ChannelID != nil && *afk.ChannelID == ch.ID})
	}
	return a, nil
}

// filter is the member's view of cmds, encoded as a CBOR array of the commands' own bytes. A command
// is shown only when both readings of its permission_bit let the member use it: the community-wide
// resolution (owner, administrator, @everyone and held roles, deny before allow), which is what the
// Bot authorises against, and, for a permission a channel overwrite can change, at least one text or
// voice channel where the member holds it after that channel's overwrites. permission_bit 0 is
// every member's. A Bot account is shown nothing.
func (a *catalogAudience) filter(ctx context.Context, repo store.Repository, cid, user id.ID, cmds []botCommand) (cbor.RawMessage, error) {
	shown := make([]cbor.RawMessage, 0, len(cmds))
	u, err := repo.GetUser(ctx, user)
	if err != nil {
		return nil, err
	}
	if u.Kind == kindHuman {
		held, err := repo.ListMemberRoles(ctx, cid, user)
		if err != nil {
			return nil, err
		}
		snap := a.base
		snap.Bot, snap.BotModerator = false, 0
		snap.Held = make(map[id.ID]bool, len(held))
		for _, r := range held {
			snap.Held[r] = true
		}
		community := snap.Resolve(user)
		var perChannel []Bits
		for _, ch := range a.channels {
			s := snap
			s.Overwrite, s.AFK = ch.overwrites, ch.afk
			perChannel = append(perChannel, s.ResolveChannel(user))
		}
		for _, c := range cmds {
			if commandVisible(c.perm, community, perChannel) {
				shown = append(shown, c.raw)
			}
		}
	}
	b, err := cborx.Marshal(shown)
	if err != nil {
		return nil, err
	}
	return cbor.RawMessage(b), nil
}

func commandVisible(perm, community Bits, perChannel []Bits) bool {
	if perm == 0 {
		return true
	}
	if !community.Has(perm) {
		return false
	}
	if perm&^communityBits == 0 {
		return true
	}
	for _, bits := range perChannel {
		if bits.Has(perm) {
			return true
		}
	}
	return false
}

// botCatalogStatus is GET …/bot/catalog's [state, since]: absent with no installation, ready,
// disabled, or offline for an installation still enrolling or joining. since is the installation's
// last change. A ready installation reads the Bot's health reports: degraded or offline, with since when.
func botCatalogStatus(ctx context.Context, repo store.Repository, health *BotHealth, cid id.ID) ([]any, error) {
	row, err := repo.GetActiveBotInstallationByCommunity(ctx, cid)
	if errors.Is(err, store.ErrNotFound) {
		return []any{botStatusAbsent, nil}, nil
	}
	if err != nil {
		return nil, err
	}
	since, ok := checkedUint64(row.Updated)
	if !ok {
		return nil, store.ErrConflict
	}
	state := botStatusOffline
	switch row.State {
	case store.BotInstallationReady:
		state = botStatusReady
		if health != nil {
			if judged, judgedSince := health.Status(cid); judged != botStatusReady || judgedSince != 0 {
				state, since = judged, judgedSince
			}
		}
	case store.BotInstallationDisabled:
		state = botStatusDisabled
	}
	return []any{state, since}, nil
}

// GetBotCatalog answers [revision, commands, publication|null, status] to a member of the
// community: the commands this member may use, the Bot's command-key publication as the Bot sent
// it, and the Bot's state.
func (d Deps) GetBotCatalog(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	cid, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	ctx := r.Context()
	if _, err := d.Repo.GetMember(ctx, cid, s.UserID); err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if _, err := d.Repo.GetCommunity(ctx, cid); err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	var revision uint64
	var cmds []botCommand
	catalog, err := d.Repo.GetBotCatalog(ctx, cid)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		server.WriteError(w, d.storeError(r, err))
		return
	default:
		if cmds, err = decodeBotCatalog(catalog.Body); err != nil {
			d.logf(r, "a stored Bot catalog does not decode", "community", cid, "err", err)
			server.WriteError(w, server.Errorf(server.CodeInternal, "stored catalog"))
			return
		}
		revision = catalog.Revision
	}
	audience, err := loadCatalogAudience(ctx, d.Repo, cid, cmds)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	shown, err := audience.filter(ctx, d.Repo, cid, s.UserID, cmds)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	var publication any
	key, err := d.Repo.GetBotCommandKey(ctx, cid)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		server.WriteError(w, d.storeError(r, err))
		return
	default:
		publication = key.Publication
	}
	status, err := botCatalogStatus(ctx, d.Repo, d.BotHealth, cid)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, []any{revision, shown, publication, status})
}

// readyBotFor checks that the session is the ready Bot of cid, against the installation as stored
// now: its user and its managed device.
func readyBotFor(ctx context.Context, repo store.Repository, s auth.Session, cid id.ID) (store.BotInstallationRow, error) {
	forbidden := server.Errorf(server.CodeForbidden, "the community's ready Bot session is required")
	if s.Scope != auth.ScopeEnrolled || s.Bot == nil || s.Bot.State != store.BotInstallationReady || s.Bot.CommunityID != cid {
		return store.BotInstallationRow{}, forbidden
	}
	row, err := repo.GetActiveBotInstallationByCommunity(ctx, cid)
	if errors.Is(err, store.ErrNotFound) {
		return store.BotInstallationRow{}, forbidden
	}
	if err != nil {
		return store.BotInstallationRow{}, err
	}
	if row.ID != s.Bot.InstallationID || row.State != store.BotInstallationReady || row.RemovedAt != nil ||
		row.UserID == nil || *row.UserID != s.UserID || row.DeviceID == nil || *row.DeviceID != s.DeviceID {
		return store.BotInstallationRow{}, forbidden
	}
	return row, nil
}

// PutBotRuntimeCatalog stores the commands list the community's ready Bot publishes. A changed list
// takes the next revision and is pushed (op 61) to every connected member, each filtered for that
// member; an unchanged one keeps its revision and pushes nothing. The answer is [revision].
func (d Deps) PutBotRuntimeCatalog(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	cid, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	ctx := r.Context()
	if _, err := readyBotFor(ctx, d.Repo, s, cid); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	body, err := server.ReadBody(w, r, maxCBORBody)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if _, err := decodeBotCatalog(body); err != nil {
		server.WriteError(w, err)
		return
	}
	var revision uint64
	changed := false
	err = d.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockBotCommunity(ctx, cid); err != nil {
			return notFound(err)
		}
		if _, err := readyBotFor(ctx, tx, s, cid); err != nil {
			return err
		}
		prev, err := tx.GetBotCatalog(ctx, cid)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		case bytes.Equal(prev.Body, body):
			revision = prev.Revision
			return nil
		}
		revision, changed = prev.Revision+1, true
		return tx.PutBotCatalog(ctx, store.BotCatalogRow{CommunityID: cid, Revision: revision, Body: body})
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if changed {
		d.scheduleBotCatalogPush(cid)
	}
	d.write(w, r, http.StatusOK, []uint64{revision})
}

// scheduleBotCatalogPush hands the community's push to the background pusher: the Bot's request
// does not wait for the fan-out.
func (d Deps) scheduleBotCatalogPush(cid id.ID) {
	if d.BotCatalogGateway == nil || d.BotCatalogPushes == nil {
		return
	}
	d.BotCatalogPushes.schedule(cid, func(ctx context.Context) { d.pushBotCatalog(ctx, cid) })
}

// pushBotCatalog sends op 61 once to each connected human member, filtered for that member, with
// the catalog and command-key publication stored now (a push that ran late carries the newest
// both). A member with no live connection is skipped: it compares revisions at connect sync. A
// failure is logged; the catalog is committed, and the revision sync recovers a missed frame.
func (d Deps) pushBotCatalog(ctx context.Context, cid id.ID) {
	gw := d.BotCatalogGateway
	if gw == nil {
		return
	}
	catalog, err := d.Repo.GetBotCatalog(ctx, cid)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			d.logCtx(ctx, "reading the catalog for a Bot catalog push failed", "community", cid, "err", err)
		}
		return
	}
	cmds, err := decodeBotCatalog(catalog.Body)
	if err != nil {
		d.logCtx(ctx, "a stored Bot catalog does not decode", "community", cid, "err", err)
		return
	}
	var publication []byte
	if key, err := d.Repo.GetBotCommandKey(ctx, cid); err == nil {
		publication = key.Publication
	} else if !errors.Is(err, store.ErrNotFound) {
		d.logCtx(ctx, "reading the command key for a Bot catalog push failed", "community", cid, "err", err)
		return
	}
	members, err := communityMemberIDs(ctx, d.Repo, cid)
	if err != nil {
		d.logCtx(ctx, "listing members for a Bot catalog push failed", "community", cid, "err", err)
		return
	}
	var audience *catalogAudience
	for _, m := range members {
		if !gw.UserConnected(m) {
			continue
		}
		if audience == nil {
			if audience, err = loadCatalogAudience(ctx, d.Repo, cid, cmds); err != nil {
				d.logCtx(ctx, "loading permissions for a Bot catalog push failed", "community", cid, "err", err)
				return
			}
		}
		u, err := d.Repo.GetUser(ctx, m)
		if err != nil {
			d.logCtx(ctx, "reading a member for a Bot catalog push failed", "community", cid, "user", m, "err", err)
			continue
		}
		if u.Kind != kindHuman {
			continue
		}
		shown, err := audience.filter(ctx, d.Repo, cid, m, cmds)
		if err != nil {
			d.logCtx(ctx, "filtering a Bot catalog push failed", "community", cid, "user", m, "err", err)
			continue
		}
		payload, err := gateway.BotCatalogPayload(cid, catalog.Revision, shown, publication)
		if err != nil {
			d.logCtx(ctx, "encoding a Bot catalog push failed", "community", cid, "err", err)
			return
		}
		gw.DeliverUser(m, gateway.Frame{Op: gateway.OpBotCatalog, Payload: payload})
	}
}

func (d Deps) logCtx(ctx context.Context, msg string, args ...any) {
	if d.Log != nil {
		d.Log.ErrorContext(ctx, msg, args...)
	}
}

// commandKeyPublication is dilla_core::bot::command::CommandKeyPublication: [1, community_id,
// key_id, public_key, not_after, bot_device_id, sig].
type commandKeyPublication struct {
	CommunityID id.ID
	KeyID       id.ID
	PublicKey   []byte
	NotAfter    uint64
	BotDeviceID id.ID
	Sig         []byte
}

// decodeCommandKeyPublication is the core's strict decoder: version 1, exactly seven elements, exact
// lengths, non-zero ids and key, deterministic CBOR with no trailing bytes.
func decodeCommandKeyPublication(b []byte) (commandKeyPublication, error) {
	bad := server.Errorf(server.CodeInvalidRequest, "the body is a command-key publication [1, community_id, key_id, public_key, not_after, bot_device_id, sig]")
	var p commandKeyPublication
	if len(b) > maxCommandKeyPublication || cborx.ExpectMajor(b, cborx.MajorArray) != nil {
		return p, bad
	}
	var f []cbor.RawMessage
	if err := cborx.Unmarshal(b, &f); err != nil || len(f) != 7 {
		return p, bad
	}
	if v, ok := rawUintOf(f[0]); !ok || v != 1 {
		return p, bad
	}
	fixed := func(raw cbor.RawMessage, n int) ([]byte, bool) {
		var out []byte
		if cborx.ExpectMajor(raw, cborx.MajorBytes) != nil || cborx.Unmarshal(raw, &out) != nil || len(out) != n || allZero(out) {
			return nil, false
		}
		return out, true
	}
	community, ok1 := fixed(f[1], 16)
	key, ok2 := fixed(f[2], 16)
	pk, ok3 := fixed(f[3], 32)
	notAfter, ok4 := rawUintOf(f[4])
	device, ok5 := fixed(f[5], 16)
	sig, ok6 := fixed(f[6], 64)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
		return p, bad
	}
	copy(p.CommunityID[:], community)
	copy(p.KeyID[:], key)
	copy(p.BotDeviceID[:], device)
	p.PublicKey, p.NotAfter, p.Sig = pk, notAfter, sig
	return p, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// signingMessage is "dilla bot command key v1" || CBOR([1, community_id, key_id, public_key,
// not_after, bot_device_id]).
func (p commandKeyPublication) signingMessage() ([]byte, error) {
	unsigned, err := cborx.Marshal([]any{uint64(1), p.CommunityID, p.KeyID, p.PublicKey, p.NotAfter, p.BotDeviceID})
	if err != nil {
		return nil, err
	}
	return append(append([]byte{}, commandKeyDomain...), unsigned...), nil
}

func (p commandKeyPublication) verify(pub ed25519.PublicKey) bool {
	if len(pub) != ed25519.PublicKeySize || len(p.Sig) != ed25519.SignatureSize {
		return false
	}
	msg, err := p.signingMessage()
	return err == nil && ed25519.Verify(pub, msg, p.Sig)
}

// botListedDeviceKey is the signing key of device in the Bot user's newest verified device list,
// the list dillad serves (GET /v1/users/{user_id}/device-list), verified with the same verifier the
// readiness check uses. A device that is not listed, or whose entry is revoked, has none.
func (d Deps) botListedDeviceKey(ctx context.Context, botUser, device id.ID) (ed25519.PublicKey, error) {
	forbidden := server.Errorf(server.CodeForbidden, "the publication is not signed by a listed device of the community's Bot")
	if d.DeviceLists == nil {
		return nil, server.Errorf(server.CodeInternal, "device-list verifier not wired")
	}
	user, err := d.Repo.GetUser(ctx, botUser)
	if err != nil {
		return nil, err
	}
	list, err := d.Repo.GetDeviceList(ctx, botUser)
	if errors.Is(err, store.ErrNotFound) {
		return nil, forbidden
	}
	if err != nil {
		return nil, err
	}
	var key ed25519.PublicKey
	verr := d.DeviceLists.WithVerification(ctx, func(ctx context.Context) error {
		entries, err := d.DeviceLists.Verify(ctx, list.Blob, user.SSKPub, botUser)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if bytes.Equal(e.DeviceID, device[:]) && !e.Revoked && len(e.DSKPub) == ed25519.PublicKeySize {
				key = ed25519.PublicKey(bytes.Clone(e.DSKPub))
			}
		}
		return nil
	})
	if verr != nil || key == nil {
		// An invalid stored list fails closed, as the readiness check does.
		return nil, forbidden
	}
	return key, nil
}

// commandKeyPublicationSlack is the Bot helper's PUBLICATION_SLACK_SECS: the helper mints a key at its rotation with
// not_after = minted + maxCommandKeyLifetime - commandKeyPublicationSlack. Both sides are checked against
// protocol/vectors/bot.json command_key_timing (TestCommandKeyTimingMatchesTheVector here, and
// command_key_timing_matches_the_vector in dilla-bot-core), so dillad and the helper cannot drift.
const commandKeyPublicationSlack = 600

// commandKeyRotatedAt is when the helper rotated to the key whose publication says notAfter: the previous key's 24 h
// overlap runs from then, not from when dillad heard of it (Task 21b fix round 1, review M2). A rotation time after now
// (a clock ahead) counts as now.
func commandKeyRotatedAt(notAfter uint64, now int64) int64 {
	minted := int64(notAfter) - (maxCommandKeyLifetime - commandKeyPublicationSlack) //nolint:gosec // G115: not_after was checked to be at most now + 8 days
	return min(minted, now)
}

// PutBotRuntimeCommandKey stores the community's command-key publication from its ready Bot after
// checking it the way a client will: the installation's device signed it and is a listed, unrevoked
// device of the Bot user, it names this community, and its not_after is neither past nor more than
// 8 days ahead. A new key_id bumps the catalog revision and pushes op 61 with the publication. 204.
func (d Deps) PutBotRuntimeCommandKey(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	cid, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	ctx := r.Context()
	installation, err := readyBotFor(ctx, d.Repo, s, cid)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	body, err := server.ReadBody(w, r, maxCommandKeyPublication)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	p, err := decodeCommandKeyPublication(body)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if p.CommunityID != cid {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "the publication names another community"))
		return
	}
	now := d.Clock.Now().Unix()
	if now < 0 || p.NotAfter < uint64(now) {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "the publication's not_after has passed"))
		return
	}
	if p.NotAfter > uint64(now)+maxCommandKeyLifetime {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "the publication's not_after is more than 8 days ahead; clients refuse it"))
		return
	}
	// The signer is the installation's own managed device, never another device the Bot user lists.
	if p.BotDeviceID != *installation.DeviceID {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "the publication is not signed by the Bot installation's device"))
		return
	}
	pub, err := d.botListedDeviceKey(ctx, *installation.UserID, p.BotDeviceID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if !p.verify(pub) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "the publication's signature does not verify under the Bot device's key"))
		return
	}
	rotated := false
	err = d.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockBotCommunity(ctx, cid); err != nil {
			return notFound(err)
		}
		current, err := readyBotFor(ctx, tx, s, cid)
		if err != nil {
			return err
		}
		if current.ID != installation.ID || *current.UserID != *installation.UserID || *current.DeviceID != p.BotDeviceID {
			return server.Errorf(server.CodeForbidden, "the Bot installation changed")
		}
		row := store.BotCommandKeyRow{CommunityID: cid, KeyID: p.KeyID, Publication: body, PublishedAt: now}
		prevKey, err := tx.GetBotCommandKey(ctx, cid)
		switch {
		case errors.Is(err, store.ErrNotFound):
			rotated = true
		case err != nil:
			return err
		default:
			rotated = prevKey.KeyID != p.KeyID
		}
		// The relay keeps accepting the replaced key for the helper's 24 h overlap (Task 21b); a re-publication of
		// the same key keeps the overlap it had.
		switch {
		case rotated && !prevKey.KeyID.IsZero():
			row.PrevKeyID, row.PrevUntil = prevKey.KeyID, commandKeyRotatedAt(p.NotAfter, now)+int64(commandKeyOverlap.Seconds())
		case !rotated:
			row.PrevKeyID, row.PrevUntil = prevKey.PrevKeyID, prevKey.PrevUntil
		}
		if err := tx.PutBotCommandKey(ctx, row); err != nil {
			return err
		}
		// A new key reaches clients the way a changed catalog does: the catalog takes the next
		// revision (so the connect sync sees it) and the push carries the publication.
		if !rotated {
			return nil
		}
		catalog, err := tx.GetBotCatalog(ctx, cid)
		if errors.Is(err, store.ErrNotFound) {
			rotated = false
			return nil
		}
		if err != nil {
			return err
		}
		catalog.Revision++
		return tx.PutBotCatalog(ctx, catalog)
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if rotated {
		d.scheduleBotCatalogPush(cid)
	}
	w.WriteHeader(http.StatusNoContent)
}

// botScopeCatalog is the scope kind GET /v1/bot/revisions reports a community's catalog revision
// under (scope_id the community). Kinds 0-2 are the stored Bot state scopes (store.BotScope*).
const botScopeCatalog uint64 = 3

// GetBotRevisions is the connect sync's one read: [[scope_kind, scope_id, revision]…], the same
// three elements op 63 carries. It holds the caller's own user scope in each community it is in
// (kind 0; scope_id is the community, since the user is the caller), every channel or call scope
// with a stored revision in a channel the caller can view now, and the catalog of every community
// the caller is in (kind 3). A client fetches a scope's Bot state, or a catalog, only when it is
// behind.
func (d Deps) GetBotRevisions(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	ctx := r.Context()
	communities, err := d.Repo.ListCommunitiesForUser(ctx, s.UserID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	type scoped struct {
		community id.ID
		channel   store.ChannelRow
	}
	channels := map[id.ID]scoped{}
	scopes := []id.ID{}
	for _, c := range communities {
		rows, err := d.Repo.ListChannels(ctx, c.ID)
		if err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
		for _, ch := range rows {
			if ch.DeletedAt != nil || (ch.Kind != ChannelText && ch.Kind != ChannelVoice) {
				continue
			}
			channels[ch.ID] = scoped{community: c.ID, channel: ch}
			scopes = append(scopes, ch.ID)
		}
	}
	rows, err := d.Repo.BotStateRevisions(ctx, s.UserID, scopes)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	// Visibility is resolved once per community with a stored scope: one snapshot of the caller,
	// then each scope channel's overwrites (viewableChannels).
	byCommunity := map[id.ID][]store.ChannelRow{}
	for _, row := range rows {
		if sc, ok := channels[row.ScopeID]; ok && row.ScopeKind != store.BotScopeUser && row.CommunityID == sc.community {
			byCommunity[sc.community] = append(byCommunity[sc.community], sc.channel)
		}
	}
	member := make(map[id.ID]bool, len(communities))
	for _, c := range communities {
		member[c.ID] = true
	}
	visible := map[id.ID]bool{}
	for cid, chs := range byCommunity {
		v, err := viewableChannels(ctx, d.Repo, cid, s.UserID, chs)
		if err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
		for ch := range v {
			visible[ch] = true
		}
	}
	out := make([][]any, 0, len(rows))
	for _, row := range rows {
		switch {
		case row.ScopeKind == store.BotScopeUser:
			// The caller's own revision in one community: scope_id names the community.
			if member[row.CommunityID] {
				out = append(out, []any{uint64(row.ScopeKind), row.CommunityID, row.Revision})
			}
		case visible[row.ScopeID] && channels[row.ScopeID].community == row.CommunityID:
			out = append(out, []any{uint64(row.ScopeKind), row.ScopeID, row.Revision})
		}
	}
	// Each community's catalog revision rides along as scope kind 3, so the one read tells a client
	// which catalogs to fetch again.
	for _, c := range communities {
		catalog, err := d.Repo.GetBotCatalog(ctx, c.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
		out = append(out, []any{botScopeCatalog, c.ID, catalog.Revision})
	}
	d.write(w, r, http.StatusOK, out)
}

// viewableChannels is the subset of channels (live channels of community cid) that member user can
// view now: one snapshot of the member (LoadSnapshot), then each channel's overwrites and the AFK
// grant, through Snapshot.ResolveChannel as Resolver.Resolve does per channel. A user who is not a
// member, or a deleted community, sees none.
func viewableChannels(ctx context.Context, repo store.Repository, cid, user id.ID, channels []store.ChannelRow) (map[id.ID]bool, error) {
	out := map[id.ID]bool{}
	if len(channels) == 0 {
		return out, nil
	}
	if _, err := repo.GetMember(ctx, cid, user); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return out, nil
		}
		return nil, err
	}
	snap, err := LoadSnapshot(ctx, repo, cid, user, nil)
	if errors.Is(err, store.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	afk, err := afkPointerOf(ctx, repo, cid)
	if err != nil {
		return nil, err
	}
	for _, ch := range channels {
		if ch.DeletedAt != nil || ch.CommunityID == nil || *ch.CommunityID != cid {
			continue
		}
		s := snap
		if s.Overwrite, err = repo.ListOverwrites(ctx, ch.ID); err != nil {
			return nil, err
		}
		s.AFK = afk.ChannelID != nil && *afk.ChannelID == ch.ID
		if s.ResolveChannel(user).Has(PermViewChannel) {
			out[ch.ID] = true
		}
	}
	return out, nil
}
