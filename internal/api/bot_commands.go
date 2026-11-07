package api

import (
	"container/list"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bot v2 Task 10: the sealed command relay. A member's device sends a sealed command over its socket;
// dillad attaches the identity it authenticated (never anything from the body) and pushes it to the
// Bot runtime's connection (op 64). The Bot answers each command, sealed to the requesting device's
// one-time reply key, through POST /v1/bot-runtime/replies, and dillad delivers the answer (op 62) to
// exactly that device. dillad never opens a seal, and the only thing it keeps is the in-memory
// request_id -> device_id ledger below, for five minutes.

const (
	// maxSealedCommand is dilla_core::bot::command::MAX_SEALED_COMMAND_BYTES.
	maxSealedCommand = 16384
	// maxSealedReply is dilla_core::bot::command::MAX_SEALED_REPLY_BYTES.
	maxSealedReply = 65536
	// botEnvelopeSlack is room for the CBOR framing around a sealed body: the arrays, the ids, seq.
	botEnvelopeSlack = 128
	// BotRequestTTL is how long a relayed request_id is remembered: a retry from the same device
	// within it is accepted again, one from another device is 409, and a reply to it is delivered.
	BotRequestTTL = 5 * time.Minute
	// BotRequestCap bounds the ledger. At the default per-device command rate (2/s) it is the five
	// minutes of about 110 devices sending flat out; past it the oldest entry is evicted, so a late
	// reply to it is 404 and the Bot's own replay table still de-duplicates.
	BotRequestCap = 65536
	// sealedCommandVersion is the version element of the sealed form.
	sealedCommandVersion = 1
)

// BotRelayGateway is what the relay needs of the gateway. *gateway.Gateway is one.
type BotRelayGateway interface {
	// DeliverBot hands op 64 to the Bot runtime's live connections of the device, and reports
	// whether any received it.
	DeliverBot(deviceID id.ID, f gateway.Frame) bool
	DeliverDevice(deviceID id.ID, f gateway.Frame)
	DeliverUser(userID id.ID, f gateway.Frame)
	// DeliverSubscribed reaches the live connections subscribed to the group.
	DeliverSubscribed(groupID id.ID, f gateway.Frame)
	OnlineIn(groupID id.ID) []gateway.OnlineDevice
}

// BotRequestLedger is the relay's only state: which device sent each request_id, in which
// community, for BotRequestTTL. It is in memory and bounded by BotRequestCap; a restart forgets it
// (an answer to a command relayed before the restart is then 404, and the client retries).
type BotRequestLedger struct {
	mu      sync.Mutex
	clk     clock.Clock
	cap     int
	entries map[id.ID]*list.Element
	order   *list.List // of *botRequest, least recently relayed first
}

type botRequest struct {
	requestID, device, community id.ID
	at                           time.Time
}

// NewBotRequestLedger is an empty ledger on clk.
func NewBotRequestLedger(clk clock.Clock) *BotRequestLedger {
	return newBotRequestLedger(clk, BotRequestCap)
}

func newBotRequestLedger(clk clock.Clock, capacity int) *BotRequestLedger {
	return &BotRequestLedger{clk: clk, cap: capacity, entries: map[id.ID]*list.Element{}, order: list.New()}
}

var errBotDuplicate = server.Errorf(server.CodeBotDuplicate, "another device sent this request_id")

// admit records that device sent requestID to community. A request_id another device (or another
// community) sent within BotRequestTTL is errBotDuplicate. The same device sending it again is
// admitted and its entry refreshed; inserted reports whether the entry is new, so a relay that
// fails can take back exactly what it added.
func (l *BotRequestLedger) admit(requestID, device, community id.ID) (inserted bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	l.expireLocked(now)
	if e, ok := l.entries[requestID]; ok {
		r, _ := e.Value.(*botRequest)
		if r.device != device || r.community != community {
			return false, errBotDuplicate
		}
		r.at = now
		l.order.MoveToBack(e)
		return false, nil
	}
	l.entries[requestID] = l.order.PushBack(&botRequest{requestID: requestID, device: device, community: community, at: now})
	for l.order.Len() > l.cap {
		l.removeLocked(l.order.Front())
	}
	return true, nil
}

// forget takes back an entry admit inserted, when the command was not relayed after all.
func (l *BotRequestLedger) forget(requestID, device id.ID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[requestID]; ok {
		if r, _ := e.Value.(*botRequest); r.device == device {
			l.removeLocked(e)
		}
	}
}

// relayed reports whether requestID was relayed from device in community within BotRequestTTL.
func (l *BotRequestLedger) relayed(requestID, device, community id.ID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.expireLocked(l.clk.Now())
	e, ok := l.entries[requestID]
	if !ok {
		return false
	}
	r, _ := e.Value.(*botRequest)
	return r.device == device && r.community == community
}

// expireLocked drops every entry older than BotRequestTTL; the list is in relay order, so they are
// at the front.
func (l *BotRequestLedger) expireLocked(now time.Time) {
	for e := l.order.Front(); e != nil; e = l.order.Front() {
		r, _ := e.Value.(*botRequest)
		if now.Sub(r.at) <= BotRequestTTL {
			return
		}
		l.removeLocked(e)
	}
}

func (l *BotRequestLedger) removeLocked(e *list.Element) {
	r, _ := l.order.Remove(e).(*botRequest)
	delete(l.entries, r.requestID)
}

// len is the number of live entries, for tests.
func (l *BotRequestLedger) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.order.Len()
}

func registerBotCommands(m *server.Mux, d Deps) {
	// Socket-only: the session middleware for an enrolled session (a Bot session's allow-list refuses
	// it), then one token of the device's own bot_command bucket.
	m.SocketAuthed("POST /v1/communities/{community}/bot/commands", d.socketChain(auth.ScopeEnrolled, dsClassBotCommand), http.HandlerFunc(d.PostBotCommand))
	// Bot runtime routes stay on HTTP until Task 14. The session middleware runs the Bot allow-list
	// (ready Bot only), then one token of the Bot device's bot_runtime bucket; each handler then binds
	// the session to its community's ready installation (readyBotFor).
	botRuntime := func(h http.HandlerFunc) http.Handler {
		return d.Sessions.Middleware(dsMeter(d.Limiter, dsClassBotRuntime, h), auth.ScopeEnrolled)
	}
	m.Handle("POST /v1/bot-runtime/replies", botRuntime(d.PostBotReply))
	m.Handle("POST /v1/bot-runtime/state-hints", botRuntime(d.PostBotStateHint))
}

// rawBstrOf decodes one CBOR byte string, refusing any other major type.
func rawBstrOf(b cbor.RawMessage) ([]byte, bool) {
	var v []byte
	if cborx.ExpectMajor(b, cborx.MajorBytes) != nil || cborx.Unmarshal(b, &v) != nil {
		return nil, false
	}
	return v, true
}

// rawIDOf decodes a 16-byte, non-zero id carried as a byte string.
func rawIDOf(b cbor.RawMessage) (id.ID, bool) {
	v, ok := rawBstrOf(b)
	if !ok || len(v) != len(id.ID{}) {
		return id.ID{}, false
	}
	var out id.ID
	copy(out[:], v)
	return out, !out.IsZero()
}

// rawArrayOf decodes a CBOR array of exactly n elements.
func rawArrayOf(b []byte, n int) ([]cbor.RawMessage, bool) {
	var elems []cbor.RawMessage
	if cborx.ExpectMajor(b, cborx.MajorArray) != nil || cborx.Unmarshal(b, &elems) != nil || len(elems) != n {
		return nil, false
	}
	return elems, true
}

// decodeBotCommand reads the body [request_id(16), sealed] and the sealed form's outer array,
// [1, key_id(16), request_id(16), enc(32), ciphertext], strictly: deterministic CBOR, version 1,
// exact lengths, at most maxSealedCommand bytes, the two request_ids equal. It never looks inside
// the ciphertext.
func decodeBotCommand(body []byte) (id.ID, []byte, error) {
	invalid := func(msg string) (id.ID, []byte, error) {
		return id.ID{}, nil, server.Errorf(server.CodeInvalidRequest, "%s", msg)
	}
	elems, ok := rawArrayOf(body, 2)
	if !ok {
		return invalid("the body is [request_id(bstr 16), sealed(bstr)]")
	}
	requestID, ok := rawIDOf(elems[0])
	if !ok {
		return invalid("request_id is 16 bytes, not all zero")
	}
	sealed, ok := rawBstrOf(elems[1])
	if !ok || len(sealed) == 0 || len(sealed) > maxSealedCommand {
		return invalid("sealed is 1 to 16384 bytes")
	}
	outer, ok := rawArrayOf(sealed, 5)
	if !ok {
		return invalid("sealed is [1, key_id(16), request_id(16), enc(32), ciphertext]")
	}
	if v, ok := rawUintOf(outer[0]); !ok || v != sealedCommandVersion {
		return invalid("sealed version is 1")
	}
	if _, ok := rawIDOf(outer[1]); !ok {
		return invalid("key_id is 16 bytes, not all zero")
	}
	inner, ok := rawIDOf(outer[2])
	if !ok {
		return invalid("the sealed request_id is 16 bytes, not all zero")
	}
	if inner != requestID {
		return invalid("the sealed request_id differs from the body's")
	}
	if enc, ok := rawBstrOf(outer[3]); !ok || len(enc) != 32 {
		return invalid("enc is 32 bytes")
	}
	if _, ok := rawBstrOf(outer[4]); !ok {
		return invalid("ciphertext is a byte string")
	}
	return requestID, sealed, nil
}

// PostBotCommand relays one sealed command: 202 [0] once the Bot runtime's connection has it.
func (d Deps) PostBotCommand(w http.ResponseWriter, r *http.Request) {
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
		server.WriteError(w, d.storeError(r, notFound(err)))
		return
	}
	if _, err := d.Repo.GetCommunity(ctx, cid); err != nil {
		server.WriteError(w, d.storeError(r, notFound(err)))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	body, err := server.ReadBody(w, r, maxSealedCommand+botEnvelopeSlack)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	requestID, sealed, err := decodeBotCommand(body)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	offline := server.Errorf(server.CodeBotOffline, "the community's Bot is not connected")
	installation, err := d.Repo.GetActiveBotInstallationByCommunity(ctx, cid)
	switch {
	case errors.Is(err, store.ErrNotFound):
		server.WriteError(w, offline)
		return
	case err != nil:
		server.WriteError(w, d.storeError(r, err))
		return
	case installation.State == store.BotInstallationDisabled:
		server.WriteError(w, server.Errorf(server.CodeForbidden, "the community's Bot is disabled"))
		return
	case installation.State != store.BotInstallationReady || installation.DeviceID == nil || d.BotRelay == nil:
		server.WriteError(w, offline)
		return
	}
	inserted, err := d.BotRequests.admit(requestID, s.DeviceID, cid)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	payload, err := gateway.BotCommandPayload(requestID, cid, s.UserID, s.DeviceID, sealed)
	if err != nil {
		if inserted {
			d.BotRequests.forget(requestID, s.DeviceID)
		}
		server.WriteError(w, server.Errorf(server.CodeInternal, "encode"))
		return
	}
	if !d.BotRelay.DeliverBot(*installation.DeviceID, gateway.Frame{Op: gateway.OpBotCommand, Payload: payload}) {
		if inserted {
			d.BotRequests.forget(requestID, s.DeviceID)
		}
		server.WriteError(w, offline)
		return
	}
	d.write(w, r, http.StatusAccepted, []uint64{0})
}

// readyBotSession is the session's ready installation: the caller must be the ready Bot of its own
// community, as stored now.
func readyBotSession(r *http.Request, d Deps) (auth.Session, store.BotInstallationRow, error) {
	s, ok := session(r)
	if !ok {
		return auth.Session{}, store.BotInstallationRow{}, server.Errorf(server.CodeUnauthenticated, "")
	}
	if s.Bot == nil {
		return auth.Session{}, store.BotInstallationRow{}, server.Errorf(server.CodeForbidden, "the community's ready Bot session is required")
	}
	row, err := readyBotFor(r.Context(), d.Repo, s, s.Bot.CommunityID)
	if err != nil {
		return auth.Session{}, store.BotInstallationRow{}, d.storeError(r, err)
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		return auth.Session{}, store.BotInstallationRow{}, err
	}
	return s, row, nil
}

// PostBotReply delivers one sealed answer of the Bot, [device_id, request_id, seq, sealed, final],
// to the device that sent the command: 204. A request_id not relayed from that device into the
// Bot's own community within BotRequestTTL is 404.
func (d Deps) PostBotReply(w http.ResponseWriter, r *http.Request) {
	_, installation, err := readyBotSession(r, d)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	body, err := server.ReadBody(w, r, maxSealedReply+botEnvelopeSlack)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	invalid := server.Errorf(server.CodeInvalidRequest, "a reply is [device_id(bstr 16), request_id(bstr 16), seq(uint32), sealed(bstr 1..65536), final(uint 0|1)]")
	elems, ok := rawArrayOf(body, 5)
	if !ok {
		server.WriteError(w, invalid)
		return
	}
	device, okDevice := rawIDOf(elems[0])
	requestID, okRequest := rawIDOf(elems[1])
	seq, okSeq := rawUintOf(elems[2])
	sealed, okSealed := rawBstrOf(elems[3])
	final, okFinal := rawUintOf(elems[4])
	if !okDevice || !okRequest || !okSeq || seq > 1<<32-1 || !okSealed || len(sealed) == 0 || len(sealed) > maxSealedReply || !okFinal || final > 1 {
		server.WriteError(w, invalid)
		return
	}
	if d.BotRelay == nil || !d.BotRequests.relayed(requestID, device, installation.CommunityID) {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such request"))
		return
	}
	payload, err := gateway.BotReplyPayload(requestID, seq, sealed, final == 1)
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeInternal, "encode"))
		return
	}
	d.BotRelay.DeliverDevice(device, gateway.Frame{Op: gateway.OpBotReply, Payload: payload, Replay: true})
	w.WriteHeader(http.StatusNoContent)
}
