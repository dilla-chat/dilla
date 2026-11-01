package api

import (
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// RegisterPublicInvites mounts the two unauthenticated invite routes, each on
// the ("invite", client address) bucket. api.Register calls it; it is exported
// so a handler test can mount them without the session-scoped routes around
// them, which need an auth.Sessions.
func (d Deps) RegisterPublicInvites(m *server.Mux) {
	m.Handle("GET /i/{code}", d.metered(classInvite, d.InviteLanding, d.landingRefusal))
	m.Handle("POST /v1/invites/redeem", d.metered(classInvite, d.RedeemInvite, refuseCBOR))
}

// RedeemInvite is POST /v1/invites/redeem: [code] →
// [invite_id, community_id|null, grants_admin]. It SPENDS a use, which is why
// it is a POST and why the landing page below is not.
func (d Deps) RedeemInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		_    struct{} `cbor:",toarray"`
		Code string
	}
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	candidates, err := auth.CanonicalizeInviteCode(req.Code)
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeInviteInvalid, "invite code: %v", err))
		return
	}
	var invite store.InviteRow
	now := d.Clock.Now().Unix()
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		var rerr error
		invite, rerr = redeem(r.Context(), tx, candidates, now)
		return rerr
	})
	if err != nil {
		if errors.Is(err, store.ErrExhausted) || errors.Is(err, store.ErrNotFound) {
			server.WriteError(w, server.Errorf(server.CodeInviteInvalid,
				"the invite is spent, expired, revoked or unknown"))
			return
		}
		server.WriteError(w, d.storeError(r, err))
		return
	}
	var community any
	if invite.CommunityID != nil {
		community = *invite.CommunityID
	}
	d.write(w, r, http.StatusOK, []any{invite.ID, community, uint64(invite.GrantsAdmin)})
}

// landingPage is the invite landing page. It carries no script, no external
// reference and no form, and one same-origin link into the web client: it is one
// card a human reads before following that link or pasting the code into a
// client, and anything more is a phishing surface an instance hands out to
// strangers by construction. The colours are the Signal tokens as literals, dark
// and light (packages/design-tokens/dist/tokens.css, pinned by TestLandingTokensMatchSignal).
// The font is the web client's JetBrains Mono when dillad serves one (Deps.LandingFont,
// R43; font-src 'self' is the only thing the CSP widens), else the system monospace.
var landingPage = template.Must(template.New("invite").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<meta name="description" content="{{.Description}}">
<meta property="og:title" content="{{.Title}}">
<meta property="og:description" content="{{.Description}}">
<meta property="og:type" content="website">
<style>
{{if .Font}}@font-face{font-family:"JetBrains Mono";font-style:normal;font-weight:100 800;font-display:swap;src:url("{{.Font}}") format("woff2")}
{{end}}:root{color-scheme:dark;--bg:#08090B;--bg-2:#0B0D10;--surface:#0D1014;--fg:#E4E8EE;--fg-2:#8C939E;--accent:#5AA8E6;--accent-ink:#03121E;--hairline:#1A1D23}
@media (prefers-color-scheme: light){:root{color-scheme:light;--bg:#F5F6F5;--bg-2:#ECEEEC;--surface:#FFFFFF;--fg:#14171A;--fg-2:#3F453F;--accent:#005799;--accent-ink:#FFFFFF;--hairline:#CBD0CB}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;background:var(--bg);color:var(--fg);font-family:{{if .Font}}"JetBrains Mono",{{end}}ui-monospace,"SFMono-Regular",Menlo,Consolas,"Liberation Mono",monospace;font-size:13.5px;line-height:1.5}
.bar{display:flex;align-items:center;gap:7px;height:2rem;padding:0 12px;background:var(--bg-2);border-bottom:1px solid var(--hairline);font-weight:700;letter-spacing:.14em}
.tile{display:inline-flex;align-items:center;justify-content:center;width:1.125rem;height:1.125rem;background:var(--accent);color:var(--accent-ink);border-radius:2px;font-size:.6875rem;letter-spacing:0}
.caret{width:6px;height:12px;background:var(--accent)}
main{width:100%;max-width:37rem;margin:0 auto;padding:3rem 1.5rem}
h1{margin:0;font-size:1.375rem;line-height:1.3;font-weight:700;letter-spacing:.02em;overflow-wrap:anywhere}
.on{margin:.25rem 0 0;color:var(--fg-2);overflow-wrap:anywhere}
.label{margin:1.5rem 0 .25rem;color:var(--fg-2);font-size:.875rem}
.code{display:block;padding:.75rem;background:var(--surface);border:1px solid var(--hairline);font-size:1.125rem;overflow-wrap:anywhere}
.meta{margin:1rem 0 0;color:var(--fg-2);font-size:.875rem}
.open{display:inline-flex;align-items:center;min-height:2.25rem;margin-top:1.5rem;padding:0 1rem;background:var(--accent);color:var(--accent-ink);font-weight:700;text-decoration:none}
@media (pointer: coarse), (max-width: 40rem){.open{min-height:2.75rem}}
.open:focus-visible{outline:2px solid var(--fg);outline-offset:3px}
.note{margin:1rem 0 0;color:var(--fg-2);font-size:.875rem;overflow-wrap:anywhere}
</style>
</head><body>
<header class="bar"><span class="tile" aria-hidden="true">D</span>DILLA<span class="caret" aria-hidden="true"></span></header>
<main>
{{if .Community}}<h1>You have been invited to {{.Community}}</h1>
<p class="on">on {{.Instance}}</p>{{else}}<h1>You have been invited to {{.Instance}}</h1>{{end}}
<p class="label">Invite code</p>
<code class="code">{{.Code}}</code>
<p class="meta">{{if .ExpiresAt}}<time datetime="{{.ExpiresAt}}">{{.Expires}}</time>{{else}}{{.Expires}}{{end}} · {{.Uses}}</p>
<a class="open" href="/welcome?invite={{.Code}}">Open dilla in this browser</a>
<p class="note">Or paste the code into a dilla client for {{.Instance}}. This page never asks for a password.</p>
</main>
</body></html>
`))

type landingData struct {
	Instance    string
	Community   string
	Code        string
	Title       string
	Description string
	ExpiresAt   string // the ISO instant, for <time datetime>; empty for an invite that never expires
	Expires     string // "Expires in 23 hours" or "Expires on 2 January 2006 (UTC)"
	Uses        string // "1 use left" or "2 uses left"
	Font        string // the same-origin JetBrains Mono woff2, or "" for the system stack (R43)
}

// landingExpiry words an expiry for a reader: under 48 hours the rounded-down
// hours or minutes, otherwise the UTC date.
func landingExpiry(expiresAt, now int64) string {
	left := time.Duration(expiresAt-now) * time.Second
	switch {
	case left >= 48*time.Hour:
		return "Expires on " + time.Unix(expiresAt, 0).UTC().Format("2 January 2006") + " (UTC)"
	case left >= time.Hour:
		return "Expires in " + landingCount(int(left/time.Hour), "hour", "hours")
	case left >= time.Minute:
		return "Expires in " + landingCount(int(left/time.Minute), "minute", "minutes")
	default:
		return "Expires in less than a minute"
	}
}

func landingCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// landingFontGlob finds the JetBrains Mono latin variable woff2 in the served web client's tree.
const landingFontGlob = "assets/jetbrains-mono-latin-wght-normal-*.woff2"

// LandingFont is the same-origin path of the web client's JetBrains Mono latin woff2 (the first
// match of landingFontGlob), or "" when the tree has none (the embedded placeholder) or is nil.
// The composition root calls it once at start and hands the answer to Deps.LandingFont.
func LandingFont(webFS fs.FS) string {
	if webFS == nil {
		return ""
	}
	matches, err := fs.Glob(webFS, landingFontGlob)
	if err != nil || len(matches) == 0 {
		return ""
	}
	return "/" + matches[0]
}

// InviteLanding is GET /i/{code}. It NEVER mutates: a link pasted into a chat
// is fetched by every preview bot that sees it, and a landing page that spent a
// use would burn every invite the moment it was shared. The use is spent by
// POST /v1/invites/redeem and by POST /v1/accounts, both of which a bot does
// not issue.
func (d Deps) InviteLanding(w http.ResponseWriter, r *http.Request) {
	// no-store keeps the code out of shared caches; no-referrer keeps it out of
	// the Referer header of anything this page could ever link to.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; font-src 'self'")

	candidates, err := auth.CanonicalizeInviteCode(r.PathValue("code"))
	if err != nil {
		d.landingRefusal(w, r, server.Errorf(server.CodeInviteInvalid, "invite code: %v", err))
		return
	}
	var invite store.InviteRow
	found := false
	for _, c := range candidates {
		row, gerr := d.Repo.GetInviteByHash(r.Context(), auth.HashInviteCode(c))
		if gerr == nil {
			invite, found = row, true
			break
		}
		if !errors.Is(gerr, store.ErrNotFound) {
			d.landingRefusal(w, r, d.storeError(r, gerr))
			return
		}
	}
	now := d.Clock.Now().Unix()
	if !found || invite.RevokedAt != nil || invite.ExpiresAt <= now || invite.Exhausted() {
		d.landingRefusal(w, r, server.Errorf(server.CodeInviteInvalid,
			"the invite is spent, expired, revoked or unknown"))
		return
	}

	// A community invite names its community. A community that has been deleted
	// takes its invites with it: the refusal is the one an unknown code gets, so
	// the page says nothing about a community that no longer exists.
	community := ""
	var communityID, communityName any
	if invite.CommunityID != nil {
		row, cerr := d.Repo.GetCommunity(r.Context(), *invite.CommunityID)
		if errors.Is(cerr, store.ErrNotFound) {
			d.landingRefusal(w, r, server.Errorf(server.CodeInviteInvalid,
				"the invite is spent, expired, revoked or unknown"))
			return
		}
		if cerr != nil {
			d.landingRefusal(w, r, d.storeError(r, cerr))
			return
		}
		community = row.Name
		communityID, communityName = row.ID, row.Name
	}
	if wantsCBOR(r) {
		// [instance, community_id|null, expires, community_name|null]. The name
		// is element 3, appended, so a client that reads the first three keeps
		// reading them.
		// expires is 0 for an invite that never expires (Task 10c).
		d.write(w, r, http.StatusOK, []any{d.Domain, communityID, inviteWireExpiry(invite), communityName})
		return
	}
	d.generation(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// The preview (title and description) names the community and the instance
	// and nothing else: no code, expiry, uses or inviter (R15).
	title, description := "Invitation to "+d.Domain, "Join "+d.Domain+", a dilla server. Open the link to accept."
	if community != "" {
		title = "Invitation to " + community + " on " + d.Domain
		description = "Join " + community + " on " + d.Domain + ", a dilla server. Open the link to accept."
	}
	uses := "No use limit"
	if invite.UnlimitedUses == 0 {
		uses = landingCount(int(invite.MaxUses-invite.UsedCount), "use", "uses") + " left" //nolint:gosec // G115: a use count this server wrote, small and non-negative
	}
	expiresAt, expires := "", "Never expires"
	if !invite.NeverExpires() {
		expiresAt, expires = time.Unix(invite.ExpiresAt, 0).UTC().Format(time.RFC3339), landingExpiry(invite.ExpiresAt, now)
	}
	if err := landingPage.Execute(w, landingData{
		Instance:    d.Domain,
		Community:   community,
		Code:        r.PathValue("code"),
		Title:       title,
		Description: description,
		ExpiresAt:   expiresAt,
		Expires:     expires,
		Uses:        uses,
		Font:        d.LandingFont,
	}); err != nil {
		d.logf(r, "api: render invite landing", "err", err)
	}
}

// landingRefusal answers in the format the caller asked for: CBOR for a client,
// a bare status for a browser. The refusal is identical for an unknown, spent,
// expired and revoked code, so the page is not an invite oracle.
func (d Deps) landingRefusal(w http.ResponseWriter, r *http.Request, err error) {
	if wantsCBOR(r) {
		server.WriteError(w, err)
		return
	}
	var e *server.Error
	status := http.StatusGone
	if errors.As(err, &e) {
		status = e.Status()
		// A browser or a preview bot that is rate limited is told when to come
		// back the way a CBOR client is (server.WriteError).
		if e.RetryAfterMS != nil && e.Code == server.CodeRateLimited {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(float64(*e.RetryAfterMS)/1000)), 10))
		}
	}
	http.Error(w, "This invitation is no longer valid.", status)
}

// wantsCBOR is the content negotiation of GET /i/{code}: a client asks for CBOR
// explicitly, and everything else — a browser, a preview bot, curl — gets HTML.
func wantsCBOR(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mt, _, _ := strings.Cut(part, ";")
		if strings.TrimSpace(mt) == "application/cbor" {
			return true
		}
	}
	return false
}

// Invites serves the three community-scoped invite routes of interfaces.md §5.2
// (protocol/09 § Invites): mint a community invite, list a community's invites
// and revoke one. The two unauthenticated routes and the redemption itself are
// Deps' (RedeemInvite, InviteLanding) and Communities.join's.
//
// Register mounts the handlers bare, as Communities does; each handler requires
// an enrolled session itself, so a mis-mounted route fails closed.
type Invites struct {
	repo    store.Repository
	clk     clock.Clock
	baseURL string
	log     *slog.Logger
}

// NewInvites takes the instance's public base URL, the origin GET /i/{code} is
// served from: it is the prefix of the url a new invite answers with.
func NewInvites(repo store.Repository, clk clock.Clock, baseURL string, log *slog.Logger) *Invites {
	return &Invites{repo: repo, clk: clk, baseURL: strings.TrimRight(baseURL, "/"), log: log}
}

func (h *Invites) Register(mux *server.Mux) {
	mux.HandleFunc("POST /v1/communities/{id}/invites", h.create)
	mux.HandleFunc("GET /v1/communities/{id}/invites", h.list)
	mux.HandleFunc("DELETE /v1/invites/{id}", h.revoke)
}

const (
	maxInviteUses = 1000           // interfaces.md §4.3: max_uses BETWEEN 1 AND 1000
	maxInviteTTL  = 30 * 24 * 3600 // seconds: an invite lives at most thirty days
)

type createInviteReq struct {
	_           struct{} `cbor:",toarray"`
	MaxUses     uint64
	TTLSeconds  uint64
	GrantsAdmin uint64
}

type createInviteResp struct {
	_        struct{} `cbor:",toarray"`
	InviteID id.ID
	Code     string
	URL      string
	Expires  uint64
}

type inviteResp struct {
	_           struct{} `cbor:",toarray"`
	InviteID    id.ID
	CommunityID *id.ID
	CreatedBy   *id.ID
	GrantsAdmin uint64
	MaxUses     uint64
	UsedCount   uint64
	Created     uint64
	ExpiresAt   uint64
	RevokedAt   *uint64
}

func (h *Invites) create(w http.ResponseWriter, r *http.Request) {
	m, err := moderate(r, h.repo, PermCreateInvite)
	if err != nil {
		h.fail(w, r, "create invite", err)
		return
	}
	var req createInviteReq
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	// max_uses 0 is "any number of uses" and ttl_seconds 0 is "never expires" (Task 10c); every
	// other value keeps the finite bounds.
	unlimited, never := req.MaxUses == 0, req.TTLSeconds == 0
	if !unlimited && req.MaxUses > maxInviteUses {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
			"max_uses must be 0 (unlimited) or 1..%d", maxInviteUses))
		return
	}
	if !never && req.TTLSeconds > maxInviteTTL {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
			"ttl_seconds must be 0 (never) or 1..%d", maxInviteTTL))
		return
	}
	grants, err := flagByte("grants_admin", req.GrantsAdmin)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if unlimited || never {
		// An open-ended link is a standing way in that only a manager can take back, so making
		// one needs manage_community on top of create_invite (owners and administrators hold
		// it). An instance-admin grant is never open-ended: it stays finite and short.
		if grants == 1 {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
				"a grants_admin invite needs a finite expiry and use count"))
			return
		}
		if !m.snap.Resolve(m.session.UserID).Has(PermManageCommunity) {
			server.WriteError(w, server.Errorf(server.CodeForbidden,
				"never-expiring or unlimited invites need manage_community"))
			return
		}
	}
	// grants_admin is an instance-level power: whoever redeems the invite at
	// registration becomes an instance admin. Only an instance admin may hand it
	// out, and a community owner is not one by being an owner.
	if grants == 1 {
		u, err := h.repo.GetUser(r.Context(), m.session.UserID)
		if err != nil {
			h.fail(w, r, "create invite", err)
			return
		}
		if u.Flags&store.UserFlagInstanceAdmin == 0 {
			server.WriteError(w, server.Errorf(server.CodeForbidden,
				"grants_admin is an instance-admin power"))
			return
		}
	}
	if err := requireCommunityWire(r.Context(), h.repo, m.session.WireVersion, &m.community); err != nil {
		server.WriteError(w, err)
		return
	}
	plain, hash := auth.NewInviteCode()
	now := h.clk.Now().Unix()
	row := store.InviteRow{
		ID: id.New(), CodeHash: hash, CommunityID: &m.community, CreatedBy: &m.session.UserID,
		GrantsAdmin: grants, MaxUses: req.MaxUses, UsedCount: 0,
		Created: now, ExpiresAt: now + int64(req.TTLSeconds), //nolint:gosec // G115: bounded by maxInviteTTL above
	}
	if unlimited {
		row.MaxUses, row.UnlimitedUses = maxInviteUses, 1
	}
	if never {
		row.ExpiresAt = store.InviteNeverExpires
	}
	if err := h.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.CreateInvite(r.Context(), row); err != nil {
			return err
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &m.session.UserID, Action: "invite.create", Target: row.ID.String(),
			// The code never reaches a log or an audit row; its 8-hex reference does.
			Detail: m.community.String() + " " + hex.EncodeToString(hash[:4]), At: now,
		})
	}); err != nil {
		h.fail(w, r, "create invite", err)
		return
	}
	if err := server.EncodeBody(w, http.StatusCreated, createInviteResp{
		InviteID: row.ID, Code: plain, URL: h.baseURL + "/i/" + plain,
		Expires: inviteWireExpiry(row),
	}); err != nil {
		h.log.Error("encode invite", "err", err)
	}
}

func (h *Invites) list(w http.ResponseWriter, r *http.Request) {
	m, err := moderate(r, h.repo, PermCreateInvite)
	if err != nil {
		h.fail(w, r, "list invites", err)
		return
	}
	rows, err := h.repo.ListInvites(r.Context(), &m.community)
	if err != nil {
		h.fail(w, r, "list invites", err)
		return
	}
	out := make([]inviteResp, 0, len(rows))
	for _, i := range rows {
		var revoked *uint64
		if i.RevokedAt != nil {
			v := uint64(*i.RevokedAt) //nolint:gosec // G115: a unix second this server wrote
			revoked = &v
		}
		out = append(out, inviteResp{
			InviteID: i.ID, CommunityID: i.CommunityID, CreatedBy: i.CreatedBy,
			GrantsAdmin: uint64(i.GrantsAdmin), MaxUses: inviteWireUses(i), UsedCount: i.UsedCount,
			Created:   uint64(i.Created), //nolint:gosec // G115: a unix second this server wrote
			ExpiresAt: inviteWireExpiry(i),
			RevokedAt: revoked,
		})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		h.log.Error("encode invites", "err", err)
	}
}

// inviteWireExpiry is an invite's expiry on the wire: 0 for one that never expires.
func inviteWireExpiry(i store.InviteRow) uint64 {
	if i.NeverExpires() {
		return 0
	}
	return uint64(i.ExpiresAt) //nolint:gosec // G115: a unix second this server wrote, positive
}

// inviteWireUses is an invite's max_uses on the wire: 0 for any number of uses.
func inviteWireUses(i store.InviteRow) uint64 {
	if i.UnlimitedUses == 1 {
		return 0
	}
	return i.MaxUses
}

// revoke kills an invite: the caller must be a member of the invite's community
// and either have created the invite or hold manage_community. An invite with no
// community is the instance admin's, revoked from the operator CLI, and reads
// here as an unknown one. Revoking a revoked invite is a no-op, so the first
// revocation time stands.
func (h *Invites) revoke(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	inviteID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	// store.Invites has no read by id (interfaces.md §4.1 fixes the method set),
	// so the invite is found in the list.
	all, err := h.repo.ListInvites(r.Context(), nil)
	if err != nil {
		h.fail(w, r, "revoke invite", err)
		return
	}
	var inv *store.InviteRow
	for i := range all {
		if all[i].ID == inviteID {
			inv = &all[i]
			break
		}
	}
	if inv == nil || inv.CommunityID == nil {
		h.fail(w, r, "revoke invite", server.Errorf(server.CodeNotFound, "no such object"))
		return
	}
	cid := *inv.CommunityID
	if _, err := h.repo.GetCommunity(r.Context(), cid); err != nil {
		h.fail(w, r, "revoke invite", notFound(err))
		return
	}
	if _, err := h.repo.GetMember(r.Context(), cid, s.UserID); err != nil {
		h.fail(w, r, "revoke invite", notFound(err))
		return
	}
	creator := inv.CreatedBy != nil && *inv.CreatedBy == s.UserID
	if !creator {
		snap, err := LoadSnapshot(r.Context(), h.repo, cid, s.UserID, nil)
		if err != nil {
			h.fail(w, r, "revoke invite", notFound(err))
			return
		}
		if !snap.Resolve(s.UserID).Has(PermManageCommunity) {
			h.fail(w, r, "revoke invite", server.Errorf(server.CodeForbidden, "missing permission"))
			return
		}
	}
	if inv.RevokedAt == nil {
		if err := requireCommunityWire(r.Context(), h.repo, s.WireVersion, &cid); err != nil {
			server.WriteError(w, err)
			return
		}
	}
	if inv.RevokedAt == nil {
		now := h.clk.Now().Unix()
		if err := h.repo.Tx(r.Context(), func(tx store.Repository) error {
			if err := tx.RevokeInvite(r.Context(), inviteID, now); err != nil {
				return err
			}
			return tx.Audit(r.Context(), store.AuditRow{
				Actor: &s.UserID, Action: "invite.revoke", Target: inviteID.String(),
				Detail: cid.String() + " " + hex.EncodeToString(inv.CodeHash[:4]), At: now,
			})
		}); err != nil {
			h.fail(w, r, "revoke invite", err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// fail writes err as the client's refusal; anything that is not a
// *server.Error is logged and reaches the client as an empty E_INTERNAL.
func (h *Invites) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	var se *server.Error
	if !errors.As(err, &se) {
		h.log.ErrorContext(r.Context(), what, "err", err)
	}
	server.WriteError(w, err)
}
