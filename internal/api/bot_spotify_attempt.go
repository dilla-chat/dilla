package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

const spotifyAttemptTTL = 10 * time.Minute
const maxSpotifyAttempts = 1024

type botSpotifyBeginRequest struct {
	_        struct{} `cbor:",toarray"`
	StepUp   string
	ClientID string
}
type botSpotifyCallbackRequest struct {
	_     struct{} `cbor:",toarray"`
	State string
	Code  string
}
type botSpotifyAttempt struct {
	id                                     id.ID
	community, installation, runtime, user id.ID
	sessionHash                            []byte
	state, verifier                        string
	expires                                int64
	code                                   string
	codeHash                               [32]byte
	claimed                                bool
}
type botSpotifyBroker struct {
	mu       sync.Mutex
	clk      clock.Clock
	attempts map[id.ID]*botSpotifyAttempt
}

func newBotSpotifyBroker(clk clock.Clock) *botSpotifyBroker {
	return &botSpotifyBroker{clk: clk, attempts: make(map[id.ID]*botSpotifyAttempt)}
}
func randomOAuthValue(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func (b *botSpotifyBroker) prune(now int64) {
	for k, a := range b.attempts {
		if a.expires <= now {
			delete(b.attempts, k)
		}
	}
}

func registerBotSpotifyAttempt(m *server.Mux, d Deps) {
	guard := BotSessionRoute(d.Sessions, d.Limiter)
	m.Handle("GET /v1/communities/{community}/bot/spotify/config", guard("GET /v1/communities/{community}/bot/spotify/config", http.HandlerFunc(d.GetBotSpotifyConfig)))
	base := "/v1/communities/{community}/bot/spotify/attempts"
	m.Handle("POST "+base, guard("POST "+base, http.HandlerFunc(d.BotSpotifyAttemptBegin)))
	m.Handle("POST "+base+"/{attempt}/callback", guard("POST "+base+"/{attempt}/callback", http.HandlerFunc(d.BotSpotifyAttemptCallback)))
	m.Handle("DELETE "+base+"/{attempt}", guard("DELETE "+base+"/{attempt}", http.HandlerFunc(d.BotSpotifyAttemptCancel)))
	m.HandleFunc("POST /v1/bot-runtime/installations/{installation}/spotify/claim", d.ClaimBotSpotifyCode)
	m.HandleFunc("GET /v1/bot-runtime/installations/{installation}/spotify/config", d.GetRuntimeBotSpotifyConfig)
}

func spotifyClientIDValid(v string) bool {
	if len(v) < 1 || len(v) > 256 {
		return false
	}
	for _, c := range v {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func spotifySettingKey(community id.ID) string { return "community_bot_spotify:" + community.String() }

func spotifyRedirectValid(redirect string) bool {
	u, err := url.Parse(redirect)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == ""
}

func (d Deps) spotifyConfiguration(ctx context.Context, community id.ID) (string, string, error) {
	redirect := "https://" + d.Domain + "/settings/bot/music/spotify/callback"
	raw, err := d.Repo.GetSetting(ctx, spotifySettingKey(community))
	clientID := string(raw)
	if errors.Is(err, store.ErrNotFound) {
		clientID = d.BotSpotifyClientID
		if d.BotSpotifyRedirectURL != "" { // Existing static OAuth configuration keeps its registered callback.
			redirect = d.BotSpotifyRedirectURL
		}
	} else if err != nil || !spotifyClientIDValid(clientID) {
		return "", "", errors.New("spotify application setting unavailable")
	}
	if !spotifyRedirectValid(redirect) {
		return "", "", errors.New("invalid Spotify callback URL")
	}
	return clientID, redirect, nil
}

func (d Deps) GetBotSpotifyConfig(w http.ResponseWriter, r *http.Request) {
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
	if _, _, err = humanForBot(r, d.Repo, cid, s); err != nil {
		server.WriteError(w, err)
		return
	}
	clientID, _, err := d.spotifyConfiguration(r.Context(), cid)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	redirect := "https://" + d.Domain + "/settings/bot/music/spotify/callback"
	if !spotifyRedirectValid(redirect) {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "Spotify callback URL is unavailable"))
		return
	}
	d.write(w, r, http.StatusOK, []any{clientID, redirect})
}

func (d Deps) GetRuntimeBotSpotifyConfig(w http.ResponseWriter, r *http.Request) {
	runtimeID, ok := d.authenticateBotRuntime(w, r, false)
	if !ok {
		return
	}
	iid, err := server.PathID(r, "installation")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	row, err := d.Repo.GetBotInstallation(r.Context(), iid)
	if err != nil || row.RuntimeID == nil || *row.RuntimeID != runtimeID || row.RemovedAt != nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "runtime does not own installation"))
		return
	}
	clientID, redirect, err := d.spotifyConfiguration(r.Context(), row.CommunityID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, []any{clientID, redirect})
}
func (d Deps) BotSpotifyAttemptBegin(w http.ResponseWriter, r *http.Request) {
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
	var req botSpotifyBeginRequest
	if err = server.DecodeBody(w, r, 4096, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	_, _, err = humanForBot(r, d.Repo, cid, s)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err = requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	if err = d.spendBotStepUp(req.StepUp, s); err != nil {
		server.WriteError(w, err)
		return
	}
	inst, err := d.Repo.GetActiveBotInstallationByCommunity(r.Context(), cid)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if inst.RuntimeID == nil {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "Bot runtime is not assigned"))
		return
	}
	if d.botSpotify == nil {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "Spotify OAuth is unavailable"))
		return
	}
	if !spotifyClientIDValid(req.ClientID) {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "enter a valid Spotify application client ID"))
		return
	}
	redirect := "https://" + d.Domain + "/settings/bot/music/spotify/callback"
	if !spotifyRedirectValid(redirect) {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "Spotify callback URL is unavailable"))
		return
	}
	if err := d.Repo.PutSetting(r.Context(), spotifySettingKey(cid), []byte(req.ClientID), d.Clock.Now().Unix()); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	state, e1 := randomOAuthValue(32)
	verifier, e2 := randomOAuthValue(32)
	if e1 != nil || e2 != nil {
		server.WriteError(w, server.Errorf(server.CodeInternal, "OAuth initialization failed"))
		return
	}
	a := &botSpotifyAttempt{id: id.New(), community: cid, installation: inst.ID, runtime: *inst.RuntimeID, user: s.UserID, sessionHash: append([]byte(nil), s.TokenHash...), state: state, verifier: verifier, expires: d.Clock.Now().Add(spotifyAttemptTTL).Unix()}
	expires, ok := checkedUint64(a.expires)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "server clock is unavailable"))
		return
	}
	challenge := sha256.Sum256([]byte(verifier))
	u, _ := url.Parse("https://accounts.spotify.com/authorize")
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", req.ClientID)
	q.Set("redirect_uri", redirect)
	q.Set("scope", "streaming user-read-email user-read-private")
	q.Set("state", state)
	q.Set("code_challenge_method", "S256")
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	u.RawQuery = q.Encode()
	d.botSpotify.mu.Lock()
	d.botSpotify.prune(d.Clock.Now().Unix())
	for key, pending := range d.botSpotify.attempts {
		if pending.installation == inst.ID {
			delete(d.botSpotify.attempts, key)
		}
	}
	if len(d.botSpotify.attempts) >= maxSpotifyAttempts {
		d.botSpotify.mu.Unlock()
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "too many pending Spotify authorizations"))
		return
	}
	d.botSpotify.attempts[a.id] = a
	d.botSpotify.mu.Unlock()
	d.write(w, r, http.StatusOK, []any{a.id, u.String(), expires})
}
func (d Deps) BotSpotifyAttemptCallback(w http.ResponseWriter, r *http.Request) {
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
	aid, err := server.PathID(r, "attempt")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req botSpotifyCallbackRequest
	if err = server.DecodeBody(w, r, 4096, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	_, _, err = humanForBot(r, d.Repo, cid, s)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err = requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.State == "" || req.Code == "" || len(req.Code) > 2048 || strings.ContainsAny(req.Code, "\r\n\x00") {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "invalid OAuth callback"))
		return
	}
	if d.botSpotify == nil {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "Spotify OAuth is unavailable"))
		return
	}
	d.botSpotify.mu.Lock()
	defer d.botSpotify.mu.Unlock()
	a := d.botSpotify.attempts[aid]
	if a == nil {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "OAuth attempt expired or invalid"))
		return
	}
	if a.expires <= d.Clock.Now().Unix() {
		delete(d.botSpotify.attempts, aid)
		server.WriteError(w, server.Errorf(server.CodeNotFound, "OAuth attempt expired or invalid"))
		return
	}
	if a.community != cid || a.user != s.UserID || !bytes.Equal(a.sessionHash, s.TokenHash) {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "OAuth attempt expired or invalid"))
		return
	}
	if a.code != "" || a.claimed {
		if a.state == req.State && a.codeHash == sha256.Sum256([]byte(req.Code)) {
			d.write(w, r, http.StatusAccepted, []any{uint64(1)})
			return
		}
		delete(d.botSpotify.attempts, aid)
		server.WriteError(w, server.Errorf(server.CodeNotFound, "OAuth attempt expired or invalid"))
		return
	}
	if a.state != req.State {
		delete(d.botSpotify.attempts, aid)
		server.WriteError(w, server.Errorf(server.CodeNotFound, "OAuth attempt expired or invalid"))
		return
	}
	inst, e := d.Repo.GetActiveBotInstallationByCommunity(r.Context(), cid)
	if e != nil || inst.ID != a.installation || inst.RuntimeID == nil || *inst.RuntimeID != a.runtime {
		delete(d.botSpotify.attempts, aid)
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "Bot installation changed"))
		return
	}
	a.code = req.Code
	a.codeHash = sha256.Sum256([]byte(req.Code))
	d.write(w, r, http.StatusAccepted, []any{uint64(1)})
}
func (d Deps) BotSpotifyAttemptCancel(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	cid, e := server.PathID(r, "community")
	if e != nil {
		server.WriteError(w, e)
		return
	}
	aid, e := server.PathID(r, "attempt")
	if e != nil {
		server.WriteError(w, e)
		return
	}
	_, _, e = humanForBot(r, d.Repo, cid, s)
	if e != nil {
		server.WriteError(w, e)
		return
	}
	if d.botSpotify != nil {
		d.botSpotify.mu.Lock()
		if a := d.botSpotify.attempts[aid]; a != nil && a.community == cid && a.user == s.UserID && bytes.Equal(a.sessionHash, s.TokenHash) {
			delete(d.botSpotify.attempts, aid)
		}
		d.botSpotify.mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}

type spotifyCodeClaim struct {
	_              struct{} `cbor:",toarray"`
	Attempt        id.ID
	Code, Verifier string
}

func (d Deps) ClaimBotSpotifyCode(w http.ResponseWriter, r *http.Request) {
	runtimeID, ok := d.authenticateBotRuntime(w, r, true)
	if !ok {
		return
	}
	iid, e := server.PathID(r, "installation")
	if e != nil {
		server.WriteError(w, e)
		return
	}
	row, e := d.Repo.GetBotInstallation(r.Context(), iid)
	if e != nil || row.RuntimeID == nil || *row.RuntimeID != runtimeID || row.RemovedAt != nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "runtime does not own installation"))
		return
	}
	bearer := runtimeBearer(r)
	if e = d.Repo.Tx(r.Context(), func(tx store.Repository) error { return verifyLiveRuntime(r.Context(), tx, runtimeID, bearer) }); e != nil {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	if d.botSpotify == nil {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "Spotify OAuth is unavailable"))
		return
	}
	d.botSpotify.mu.Lock()
	defer d.botSpotify.mu.Unlock()
	d.botSpotify.prune(d.Clock.Now().Unix())
	for k, a := range d.botSpotify.attempts {
		if a.installation == iid && a.code != "" {
			a.claimed = true
			out := spotifyCodeClaim{Attempt: k, Code: a.code, Verifier: a.verifier}
			d.write(w, r, http.StatusOK, []any{out.Attempt, out.Code, out.Verifier})
			return
		}
	}
	server.WriteError(w, server.Errorf(server.CodeNotFound, "no pending Spotify authorization"))
}
