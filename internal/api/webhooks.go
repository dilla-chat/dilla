package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/webhookavatar"
)

// Bot v2 Task 13: incoming webhooks (spec G5). An admin creates a webhook for one Bot channel on the socket and is
// shown https://<domain>/hooks/<webhook_id>/<secret> once. Anything that can send an HTTPS POST then posts JSON there;
// dillad checks the secret, meters the POST, hands the text to the Bot's live connection as gateway op 66 and answers
// 204 once the Bot acks it (op 67). The Bot posts it into the channel through MLS.
//
// The public route is unauthenticated and treated as hostile:
//   - the client address is metered first (server.WebhookAddressClass), on every attempt;
//   - at most maxWebhookBody bytes are read (http.MaxBytesReader), before anything is parsed;
//   - an unknown id, a malformed id or secret and a wrong secret do the same work (one store lookup, one SHA-256,
//     one constant-time compare against the stored hash or a per-process dummy) and get the same 401 and body;
//   - only then is the webhook's own bucket spent (server.WebhookClass, 30/min burst 5);
//   - the JSON is decoded strictly by type: unknown fields are ignored (Discord-compatible), known ones must have
//     their type, and the body must be valid UTF-8;
//   - the text, username and avatar_url are never stored or logged, and the secret travels only in the path, which
//     server.RequestLog logs as the route pattern, never the URL.
// dillad never fetches avatar_url; it passes it to the Bot.

// BotWebhookGateway is what the public route needs of the gateway. *gateway.Gateway is one.
type BotWebhookGateway interface {
	// DeliverBotWebhook sends op 66 to the Bot's live connection and waits for its ack, at most until ctx ends.
	DeliverBotWebhook(ctx context.Context, deviceID, deliveryID id.ID, p cbor.RawMessage) (uint64, error)
}

const (
	// maxWebhookBody is the most of a webhook POST body dillad reads (8 KiB).
	maxWebhookBody = 8 << 10
	// maxWebhookText is the most text a webhook may post, in bytes of UTF-8 (spec G5).
	maxWebhookText = 4000
	// maxWebhookUsername bounds a POST's username in bytes, as received.
	maxWebhookUsername = 80
	// maxWebhookAvatarURL bounds a POST's avatar_url in bytes.
	maxWebhookAvatarURL = 2048
	// maxWebhookName bounds a webhook's name in characters, as an admin sets it.
	maxWebhookName = 80
	// maxWebhooksPerCommunity bounds the webhooks one community holds.
	maxWebhooksPerCommunity = 50
	// webhookSecretBytes is the secret's length before base64url.
	webhookSecretBytes = 32
	// webhookAvatarBlobBytes is an avatar blob id's length: a content digest, never dereferenced by dillad.
	webhookAvatarBlobBytes = 32
	// webhookAvatarWait is the most a POST waits for a new avatar fetch before it delivers without one; the
	// fetch goes on (at most webhookavatar.FetchTimeout) and its image serves the webhook's next post.
	webhookAvatarWait = 750 * time.Millisecond
	// DefaultWebhookAckTimeout is how long a POST waits for the Bot's ack before it answers 503.
	DefaultWebhookAckTimeout = 2 * time.Second
	// webhookRetryAfter is the Retry-After of every 503.
	webhookRetryAfter = 5
)

// webhookDummyHash is what a presented secret is compared against when its id is unknown or malformed, so that
// path does the same work as a wrong secret. It is random per process: nothing can be presented to match it, and
// a match is never accepted without a found row anyway.
var webhookDummyHash = func() []byte {
	b := make([]byte, sha256.Size)
	if _, err := rand.Read(b); err != nil {
		panic("api: no randomness for the webhook dummy hash: " + err.Error())
	}
	return b
}()

func registerBotWebhooks(m *server.Mux, d Deps) {
	m.SocketAuthed("POST /v1/communities/{community}/bot/webhooks", d.socketChain(auth.ScopeEnrolled, dsClassWrite), http.HandlerFunc(d.CreateBotWebhook))
	m.SocketAuthed("GET /v1/communities/{community}/bot/webhooks", d.socketChain(auth.ScopeEnrolled, dsClassRead), http.HandlerFunc(d.ListBotWebhooks))
	m.SocketAuthed("POST /v1/communities/{community}/bot/webhooks/{webhook}/rotate", d.socketChain(auth.ScopeEnrolled, dsClassWrite), http.HandlerFunc(d.RotateBotWebhook))
	m.SocketAuthed("PATCH /v1/communities/{community}/bot/webhooks/{webhook}", d.socketChain(auth.ScopeEnrolled, dsClassWrite), http.HandlerFunc(d.PatchBotWebhook))
	m.SocketAuthed("DELETE /v1/communities/{community}/bot/webhooks/{webhook}", d.socketChain(auth.ScopeEnrolled, dsClassWrite), http.HandlerFunc(d.DeleteBotWebhook))
	// Unauthenticated and public, so it stays on HTTP; it meters itself (PostWebhook).
	m.Handle("POST /hooks/{webhook_id}/{secret}", http.HandlerFunc(d.PostWebhook))
	// A webhook avatar dillad fetched and re-encoded: binary, so it stays on HTTP like GIF media; members only.
	m.Handle("GET /v1/communities/{community}/bot/webhook-avatars/{avatar}", d.enrolled(dsMeter(d.Limiter, dsClassRead, d.GetWebhookAvatar)))
}

// newWebhookSecret is 32 random bytes and their SHA-256, the only form stored.
func newWebhookSecret() (encoded string, hash []byte, err error) {
	b := make([]byte, webhookSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(b), sum[:], nil
}

// webhookURL is the address shown once: the instance's configured public domain, never anything from the request.
func (d Deps) webhookURL(webhook id.ID, secret string) string {
	return (&url.URL{Scheme: "https", Host: d.Domain, Path: "/hooks/" + webhook.String() + "/" + secret}).String()
}

// isBotChannelOf reports whether channel is a live Bot channel of community now.
func isBotChannelOf(ctx context.Context, repo store.Repository, community, channel id.ID) (bool, error) {
	rows, err := repo.ListBotChannels(ctx, community)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.ChannelID == channel {
			return true, nil
		}
	}
	return false, nil
}

func notBotChannel() error {
	return server.Errorf(server.CodeNotBotChannel, "the webhook's channel is not a Bot channel")
}

// webhookSettings is the name, avatar and allow_everyone an admin sets, validated.
type webhookSettings struct {
	name          string
	avatar        []byte
	allowEveryone bool
}

func validWebhookSettings(name string, avatar []byte, allowEveryone uint64) (webhookSettings, error) {
	n, err := auth.NormalizeUserText(name, maxWebhookName, false)
	if err != nil || n == "" {
		return webhookSettings{}, server.Errorf(server.CodeInvalidRequest, "invalid webhook name")
	}
	if avatar != nil && len(avatar) != webhookAvatarBlobBytes {
		return webhookSettings{}, server.Errorf(server.CodeInvalidRequest, "avatar_blob must be %d bytes or null", webhookAvatarBlobBytes)
	}
	if allowEveryone > 1 {
		return webhookSettings{}, server.Errorf(server.CodeInvalidRequest, "allow_everyone must be 0 or 1")
	}
	return webhookSettings{name: n, avatar: avatar, allowEveryone: allowEveryone == 1}, nil
}

// webhookAdmin is the session and path of an admin route, after the Bot-page authorisation (humanForBot:
// a human member holding manage_community) and the wire check.
func (d Deps) webhookAdmin(w http.ResponseWriter, r *http.Request, withWebhook bool) (auth.Session, id.ID, id.ID, bool) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return auth.Session{}, id.ID{}, id.ID{}, false
	}
	cid, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return auth.Session{}, id.ID{}, id.ID{}, false
	}
	var wid id.ID
	if withWebhook {
		if wid, err = server.PathID(r, "webhook"); err != nil {
			server.WriteError(w, err)
			return auth.Session{}, id.ID{}, id.ID{}, false
		}
	}
	if _, _, err := humanForBot(r, d.Repo, cid, s); err != nil {
		server.WriteError(w, err)
		return auth.Session{}, id.ID{}, id.ID{}, false
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return auth.Session{}, id.ID{}, id.ID{}, false
	}
	return s, cid, wid, true
}

// CreateBotWebhook creates a webhook for a Bot channel of the community: body [channel_id, name,
// avatar_blob|null, allow_everyone], answer 201 [webhook_id, url]. The url carries the secret and is never shown
// again. A channel that is not a Bot channel of the community is 409 E_NOT_BOT_CHANNEL.
func (d Deps) CreateBotWebhook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		_             struct{} `cbor:",toarray"`
		ChannelID     id.ID
		Name          string
		Avatar        []byte
		AllowEveryone uint64
	}
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	s, cid, _, ok := d.webhookAdmin(w, r, false)
	if !ok {
		return
	}
	settings, err := validWebhookSettings(req.Name, req.Avatar, req.AllowEveryone)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	secret, hash, err := newWebhookSecret()
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeInternal, "randomness"))
		return
	}
	wid := id.New()
	ctx := r.Context()
	err = d.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockBotCommunity(ctx, cid); err != nil {
			return notFound(err)
		}
		if _, _, err := humanForBot(r, tx, cid, s); err != nil {
			return err
		}
		marked, err := isBotChannelOf(ctx, tx, cid, req.ChannelID)
		if err != nil {
			return err
		}
		if !marked {
			return notBotChannel()
		}
		existing, err := tx.ListBotWebhooks(ctx, cid)
		if err != nil {
			return err
		}
		if len(existing) >= maxWebhooksPerCommunity {
			return server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest, "a community holds at most %d webhooks", maxWebhooksPerCommunity))
		}
		now := d.Clock.Now().Unix()
		if err := tx.CreateBotWebhook(ctx, store.BotWebhookRow{WebhookID: wid, CommunityID: cid, ChannelID: req.ChannelID,
			Name: settings.name, AvatarBlob: settings.avatar, SecretHash: hash, AllowEveryone: settings.allowEveryone, CreatedAt: now}); err != nil {
			return err
		}
		return tx.Audit(ctx, store.AuditRow{Actor: &s.UserID, Action: "bot.webhook_created", Target: wid.String(), Detail: req.ChannelID.String(), At: now})
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusCreated, []any{wid, d.webhookURL(wid, secret)})
}

// ListBotWebhooks answers [[webhook_id, channel_id, name, avatar_blob|null, allow_everyone, created_at,
// rotated_at|null]…], oldest first. Never the secret or its hash.
func (d Deps) ListBotWebhooks(w http.ResponseWriter, r *http.Request) {
	_, cid, _, ok := d.webhookAdmin(w, r, false)
	if !ok {
		return
	}
	rows, err := d.Repo.ListBotWebhooks(r.Context(), cid)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	out := make([][]any, 0, len(rows))
	for _, row := range rows {
		created, ok := checkedUint64(row.CreatedAt)
		if !ok {
			server.WriteError(w, d.storeError(r, store.ErrConflict))
			return
		}
		var avatar, rotated any
		if row.AvatarBlob != nil {
			avatar = row.AvatarBlob
		}
		if row.RotatedAt != nil {
			at, ok := checkedUint64(*row.RotatedAt)
			if !ok {
				server.WriteError(w, d.storeError(r, store.ErrConflict))
				return
			}
			rotated = at
		}
		allow := uint64(0)
		if row.AllowEveryone {
			allow = 1
		}
		out = append(out, []any{row.WebhookID, row.ChannelID, row.Name, avatar, allow, created, rotated})
	}
	d.write(w, r, http.StatusOK, out)
}

// RotateBotWebhook replaces the webhook's secret, in one statement, and answers [url] with the new secret: the old
// URL stops working at once.
func (d Deps) RotateBotWebhook(w http.ResponseWriter, r *http.Request) {
	s, cid, wid, ok := d.webhookAdmin(w, r, true)
	if !ok {
		return
	}
	secret, hash, err := newWebhookSecret()
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeInternal, "randomness"))
		return
	}
	ctx := r.Context()
	err = d.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockBotCommunity(ctx, cid); err != nil {
			return notFound(err)
		}
		if _, _, err := humanForBot(r, tx, cid, s); err != nil {
			return err
		}
		now := d.Clock.Now().Unix()
		found, err := tx.RotateBotWebhookSecret(ctx, cid, wid, hash, now)
		if err != nil {
			return err
		}
		if !found {
			return server.Errorf(server.CodeNotFound, "webhook")
		}
		return tx.Audit(ctx, store.AuditRow{Actor: &s.UserID, Action: "bot.webhook_rotated", Target: wid.String(), Detail: cid.String(), At: now})
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, []any{d.webhookURL(wid, secret)})
}

// PatchBotWebhook replaces the webhook's name, avatar and allow_everyone: body [name, avatar_blob|null,
// allow_everyone], answer 204. The channel and the secret are not changed here.
func (d Deps) PatchBotWebhook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		_             struct{} `cbor:",toarray"`
		Name          string
		Avatar        []byte
		AllowEveryone uint64
	}
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	s, cid, wid, ok := d.webhookAdmin(w, r, true)
	if !ok {
		return
	}
	settings, err := validWebhookSettings(req.Name, req.Avatar, req.AllowEveryone)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	ctx := r.Context()
	err = d.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockBotCommunity(ctx, cid); err != nil {
			return notFound(err)
		}
		if _, _, err := humanForBot(r, tx, cid, s); err != nil {
			return err
		}
		found, err := tx.UpdateBotWebhook(ctx, store.BotWebhookRow{WebhookID: wid, CommunityID: cid, Name: settings.name,
			AvatarBlob: settings.avatar, AllowEveryone: settings.allowEveryone})
		if err != nil {
			return err
		}
		if !found {
			return server.Errorf(server.CodeNotFound, "webhook")
		}
		return tx.Audit(ctx, store.AuditRow{Actor: &s.UserID, Action: "bot.webhook_updated", Target: wid.String(), Detail: cid.String(), At: d.Clock.Now().Unix()})
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteBotWebhook deletes the webhook: 204, or 404 when the community holds no such webhook.
func (d Deps) DeleteBotWebhook(w http.ResponseWriter, r *http.Request) {
	s, cid, wid, ok := d.webhookAdmin(w, r, true)
	if !ok {
		return
	}
	ctx := r.Context()
	err := d.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockBotCommunity(ctx, cid); err != nil {
			return notFound(err)
		}
		if _, _, err := humanForBot(r, tx, cid, s); err != nil {
			return err
		}
		found, err := tx.DeleteBotWebhook(ctx, cid, wid)
		if err != nil {
			return err
		}
		if !found {
			return server.Errorf(server.CodeNotFound, "webhook")
		}
		return tx.Audit(ctx, store.AuditRow{Actor: &s.UserID, Action: "bot.webhook_deleted", Target: wid.String(), Detail: cid.String(), At: d.Clock.Now().Unix()})
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// The public route's answers are JSON, for senders that speak only JSON: {"code": "E_…", "message": "…"}.
type hookError struct {
	status     int
	code       server.Code
	message    string
	retryAfter time.Duration
}

func writeHookError(w http.ResponseWriter, e hookError) {
	if e.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(e.retryAfter.Seconds())), 10))
	}
	body, err := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{string(e.code), e.message})
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(e.status)
	_, _ = w.Write(body)
}

var (
	hookUnauthenticated = hookError{status: http.StatusUnauthorized, code: server.CodeUnauthenticated, message: "unknown webhook or wrong secret"}
	hookTooLarge        = hookError{status: http.StatusRequestEntityTooLarge, code: server.CodeTooLarge, message: "the body is over 8 KiB or the text over 4000 bytes"}
	hookNotBotChannel   = hookError{status: http.StatusConflict, code: server.CodeNotBotChannel, message: "the webhook's channel is not a Bot channel"}
	hookOffline         = hookError{status: http.StatusServiceUnavailable, code: server.CodeBotOffline, message: "the Bot did not take the post; retry later", retryAfter: webhookRetryAfter * time.Second}
	hookInternal        = hookError{status: http.StatusInternalServerError, code: server.CodeInternal}
)

func hookInvalid(message string) hookError {
	return hookError{status: http.StatusBadRequest, code: server.CodeInvalidRequest, message: message}
}

func hookRateLimited(wait time.Duration) hookError {
	if wait < time.Second {
		wait = time.Second
	}
	return hookError{status: http.StatusTooManyRequests, code: server.CodeRateLimited, message: "rate limited", retryAfter: wait}
}

// webhookByCredential finds the webhook the path names and checks its secret. Every path through it, a malformed
// id, an unknown one, a malformed or wrong secret and a right one, does one store lookup, one SHA-256 and one
// constant-time compare, so none is told apart by timing more than by status.
func (d Deps) webhookByCredential(ctx context.Context, rawID, rawSecret string) (store.BotWebhookRow, bool, error) {
	wid, idErr := id.Parse(rawID)
	if idErr != nil {
		wid = id.ID{}
	}
	row, err := d.Repo.GetBotWebhook(ctx, wid)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.BotWebhookRow{}, false, err
	}
	presented := make([]byte, webhookSecretBytes)
	n, decErr := base64.RawURLEncoding.Strict().Decode(presented, []byte(rawSecret[:min(len(rawSecret), base64.RawURLEncoding.EncodedLen(webhookSecretBytes))]))
	wellFormed := decErr == nil && n == webhookSecretBytes && len(rawSecret) == base64.RawURLEncoding.EncodedLen(webhookSecretBytes)
	sum := sha256.Sum256(presented)
	want := webhookDummyHash
	if found && idErr == nil && len(row.SecretHash) == sha256.Size {
		want = row.SecretHash
	}
	match := subtle.ConstantTimeCompare(sum[:], want) == 1
	return row, match && found && idErr == nil && wellFormed, nil
}

// webhookPost is a POST body as decoded. Pointers tell a missing field from an empty one; any other field is
// ignored (Discord sends embeds, tts, allowed_mentions and more).
type webhookPost struct {
	Text      *string `json:"text"`
	Content   *string `json:"content"`
	Username  *string `json:"username"`
	AvatarURL *string `json:"avatar_url"`
}

// decodeWebhookPost parses the body strictly by type: one JSON object, valid UTF-8, nothing after it.
func decodeWebhookPost(body []byte) (text string, username string, avatar *string, e *hookError) {
	invalid := func(m string) (string, string, *string, *hookError) {
		h := hookInvalid(m)
		return "", "", nil, &h
	}
	if !utf8.Valid(body) {
		return invalid("the body is not UTF-8")
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return invalid("the body must be a JSON object")
	}
	var req webhookPost
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&req); err != nil {
		return invalid("malformed JSON or a field of the wrong type")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalid("data after the JSON object")
	}
	// text is the field; Discord's content is read when text is absent.
	t := req.Text
	if t == nil {
		t = req.Content
	}
	if t == nil || *t == "" {
		return invalid("text is required")
	}
	if len(*t) > maxWebhookText {
		h := hookTooLarge
		return "", "", nil, &h
	}
	for _, r := range *t {
		if unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r' {
			return invalid("text holds a control character")
		}
	}
	if req.Username != nil {
		if len(*req.Username) > maxWebhookUsername {
			return invalid("username is over 80 bytes")
		}
		username = cleanWebhookUsername(*req.Username)
	}
	if req.AvatarURL != nil && *req.AvatarURL != "" {
		a := *req.AvatarURL
		if len(a) > maxWebhookAvatarURL || !validAvatarURL(a) {
			return invalid("avatar_url must be an https:// URL on port 443 of at most 2048 bytes")
		}
		avatar = &a
	}
	return *t, username, avatar, nil
}

// cleanWebhookUsername strips what must not reach a display name: control characters (Cc), format characters (Cf:
// bidi overrides, zero-width joiners and marks) and the line and paragraph separators, then trims white space.
// "" means the webhook's own name is shown.
func cleanWebhookUsername(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// validAvatarURL is the avatar fetcher's URL rule: https, port 443 only, a host, no credentials, no control or
// space characters (webhookavatar.CheckURL).
func validAvatarURL(s string) bool {
	_, err := webhookavatar.CheckURL(s)
	return err == nil
}

func webhookContentTypeOK(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// PostWebhook is POST /hooks/{webhook_id}/{secret}. Answers: 204 once the Bot acked; 400 a malformed body; 401 an
// unknown webhook or a wrong secret (one answer for both); 409 E_NOT_BOT_CHANNEL; 413 a body over 8 KiB or text
// over 4000 bytes; 429 with Retry-After; 503 with Retry-After: 5 when the Bot is offline, did not ack within
// WebhookAckTimeout, or did not post.
func (d Deps) PostWebhook(w http.ResponseWriter, r *http.Request) {
	if d.Limiter == nil || d.Config == nil || d.Repo == nil {
		writeHookError(w, hookInternal)
		return
	}
	client := server.RealIP(r, d.Config.Server.TrustedProxyCIDRs)
	if ok, wait := d.Limiter.Allow(server.WebhookAddressClass, server.RateKey(client)); !ok {
		writeHookError(w, hookRateLimited(wait))
		return
	}
	if ok, wait := d.Limiter.Allow(server.WebhookPrefixClass, server.RatePrefixKey(client)); !ok {
		writeHookError(w, hookRateLimited(wait))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeHookError(w, hookTooLarge)
			return
		}
		writeHookError(w, hookInvalid("the body could not be read"))
		return
	}
	ctx := r.Context()
	hook, ok, err := d.webhookByCredential(ctx, r.PathValue("webhook_id"), r.PathValue("secret"))
	if err != nil {
		d.logf(r, "webhook lookup failed", "err", err)
		writeHookError(w, hookInternal)
		return
	}
	if !ok || r.URL.RawPath != "" {
		writeHookError(w, hookUnauthenticated)
		return
	}
	if ok, wait := d.Limiter.Allow(server.WebhookClass, hook.WebhookID.String()); !ok {
		writeHookError(w, hookRateLimited(wait))
		return
	}
	if !webhookContentTypeOK(r) {
		writeHookError(w, hookInvalid("Content-Type must be application/json"))
		return
	}
	text, username, avatar, perr := decodeWebhookPost(body)
	if perr != nil {
		writeHookError(w, *perr)
		return
	}
	marked, err := isBotChannelOf(ctx, d.Repo, hook.CommunityID, hook.ChannelID)
	if err != nil {
		d.logf(r, "webhook channel check failed", "err", err)
		writeHookError(w, hookInternal)
		return
	}
	if !marked {
		writeHookError(w, hookNotBotChannel)
		return
	}
	installation, err := d.Repo.GetActiveBotInstallationByCommunity(ctx, hook.CommunityID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		d.logf(r, "webhook installation read failed", "err", err)
		writeHookError(w, hookInternal)
		return
	}
	if err != nil || installation.State != store.BotInstallationReady || installation.DeviceID == nil || d.BotWebhooks == nil {
		writeHookError(w, hookOffline)
		return
	}
	display := username
	if display == "" {
		display = hook.Name
	}
	var avatarID []byte
	if avatar != nil && d.WebhookAvatars != nil {
		avatarID = d.WebhookAvatars.Resolve(ctx, hook.CommunityID, hook.WebhookID, *avatar, webhookAvatarWait)
	}
	delivery := id.New()
	p, err := gateway.BotWebhookPayload(delivery, hook.WebhookID, hook.CommunityID, hook.ChannelID, display, avatarID, text, hook.AllowEveryone)
	if err != nil {
		writeHookError(w, hookInternal)
		return
	}
	wait := d.WebhookAckTimeout
	if wait <= 0 {
		wait = DefaultWebhookAckTimeout
	}
	ackCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	status, err := d.BotWebhooks.DeliverBotWebhook(ackCtx, *installation.DeviceID, delivery, p)
	if err != nil || status != 0 {
		writeHookError(w, hookOffline)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// GetWebhookAvatar serves a webhook avatar the instance fetched and re-encoded (fix round 1), to a member of the
// community whose webhook named it: image/png, privately cacheable, nosniff. Anything else is 404, so a non-member
// learns nothing about which avatars exist.
func (d Deps) GetWebhookAvatar(w http.ResponseWriter, r *http.Request) {
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
	avatar, err := server.PathDigest(r, "avatar")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if _, err := d.Repo.GetMember(r.Context(), cid, s.UserID); err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if d.WebhookAvatars == nil {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such avatar"))
		return
	}
	img, ok := d.WebhookAvatars.Get(cid, avatar)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such avatar"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Content-Length", strconv.Itoa(len(img)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("Cache-Control", "private, max-age=86400, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(img) //nolint:gosec // G705: a PNG the instance re-encoded itself, served as image/png with nosniff and a none CSP
}
