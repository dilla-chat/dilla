package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"sync"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// Groups holds the delivery-service HTTP surface. Bodies are deterministic CBOR fixed-position
// arrays; MLS objects are byte strings (interfaces.md §5.0).
//
// EVERY handler here is written against the surface plan-1a task 6 actually produces —
// `*server.Mux`, `server.DecodeBody(w, r, max, v)` (four arguments), `server.EncodeBody(w, status,
// v)`, `server.WriteError`, `server.PathID`, `server.Errorf` and the four extended constructors
// `server.CommitConflict`, `server.CommitRequired`, `server.CommitInvalid`, `server.Version` — and
// reads the session with `auth.FromContext(r.Context())`, which `(*auth.Sessions).Middleware` put
// there. There is no `server.Handler`, no `server.HandlerWithLimit`, no `server.QueryUint`, no
// `server.SessionOf` and no `server.APIError`: those names appear in no plan and in no contract.
// Deviation B12 records that part 1b invents none of them.
//
// A handler is therefore an ordinary http.HandlerFunc that writes its own refusal, because
// plan-1a's Mux takes an http.Handler and has no error-returning form.
//
// Groups mounts itself rather than hanging off `api.Deps` and `api.Register`, because a nil
// `*ds.DS` must not silently turn three documented routes into 501s: an instance either has a
// delivery service or is not a dilla instance. The composition root (task 27a) builds the DS and
// calls Register beside `api.Register`.
type Groups struct {
	DS      *ds.DS
	Deps    Deps  // trusted read dependencies for human current interaction-context proofs
	MaxBody int64 // §5.3's cap for this group of routes; zero means maxDSBody
	// Limiter meters every route per device session from `[limits.rate]` (dsmeter.go); nil
	// meters nothing.
	Limiter *server.RateLimiter
	// AfterRegister, when set, runs after the delivery service accepted a registration, once the
	// 201 has been written and flushed, on a goroutine of its own under a context the request's
	// cancellation does not reach; Drain waits for it. The composition root sets it to
	// SyncRegisteredGroup (Plan 2 task 7): a channel's new text group is populated by batched
	// delivery-service Adds, which is protocol/01 § Joining's "creating a private channel". It runs
	// outside any transaction; a failure is the hook's to log, because the registration itself has
	// succeeded.
	AfterRegister func(ctx context.Context, groupID id.ID)
	// LoadBotInteractionPreflight supplies fresh trusted context for a kind-3 registration. The
	// handler obtains its identifiers from an authenticated MLS GroupContext before invoking it.
	LoadBotInteractionPreflight func(context.Context, BotInteractionPreflightRequest) (ds.InteractionRegistrationPreflight, error)
	// AfterKeyPackages, when set, runs after the delivery service stored a device's KeyPackages,
	// the same way AfterRegister does: after the 201, on its own goroutine, under a context the
	// request's cancellation does not reach, drained by Drain. SyncGroupMembers proposes only a
	// device that holds an available KeyPackage, so the composition root uses it to bring the
	// device's DMs in line once it has one (Plan 2 task 6).
	AfterKeyPackages func(ctx context.Context, userID, deviceID id.ID)

	// afterRegister counts the AfterRegister and AfterKeyPackages runs still in flight, for Drain.
	afterRegister inFlight

	// commitsInFlight holds the devices with a POST /commit upload in progress (commitguard.go).
	commitsInFlight sync.Map
}

// maxDSBody is the delivery service's own body cap. It is far above §5.3's 64 KiB general limit
// because a registration carries the whole ratchet tree: the committed 1,500-leaf fixture's is
// 620 KiB, and the MLS ciphertext cap alone is 128 KiB.
const maxDSBody = 2 << 20

// maxCommitBody is POST /commit's own cap, above maxDSBody because of the one batch the protocol
// mandates: protocol/01 § Joining puts up to 256 Adds in one commit, and row 5 addresses the
// commit's Welcome to each added device separately (`welcomes([[device_id, blob]])`), so a full
// batch carries 256 copies of a Welcome that is itself about 30 KiB at 256 joiners — some 8 MiB,
// measured by join_storm_256_batched. 16 MiB leaves room for the commit and the GroupInfo beside
// them and is still a cap (deviation B37; a wire form that sends one shared Welcome once is the
// protocol follow-up that would bring this back down). The headroom is the Welcomes' alone: the
// commit handler holds the commit, the GroupInfo and the tree together to maxDSBody.
const maxCommitBody = 16 << 20

func (h *Groups) max() int64 {
	if h.MaxBody == 0 {
		return maxDSBody
	}
	return h.MaxBody
}

// maxCommit is the commit route's cap: maxCommitBody, or MaxBody when a caller configured a
// larger one.
func (h *Groups) maxCommit() int64 {
	return max(h.max(), maxCommitBody)
}

func (h *Groups) Register(mux *server.Mux, sessions *auth.Sessions) {
	// Every delivery-service route needs an enrolled device session. The KeyPackage and Welcome
	// routes of task 24 are the only ones that also accept a provisional session, and they say so
	// at their own Register.
	mux.SocketAuthed("POST /v1/groups", socketMeter(sessions, h.Limiter, dsClassWrite), http.HandlerFunc(h.create))
	h.registerBotInteractionGroupReads(mux, sessions)
	h.RegisterBotRegistration(mux, sessions)
	h.RegisterBotMessages(mux, sessions)
	mux.SocketAuthed("GET /v1/groups/{id}/info", socketMeter(sessions, h.Limiter, dsClassRead), http.HandlerFunc(h.info))
	mux.SocketAuthed("GET /v1/groups/{id}/interaction-admission-context", socketMeter(sessions, h.Limiter, dsClassRead), http.HandlerFunc(h.currentInteractionAdmissionContext))
	mux.SocketAuthed("GET /v1/groups/{id}/tree", socketMeter(sessions, h.Limiter, dsClassRead), http.HandlerFunc(h.tree))
}

// RegisterSequencer mounts the four routes of the sequencer and the commit path: endpoints 4
// (handshake catch-up), 5 (commit), 6 (member proposal) and 19 (outstanding proposals). It is a
// second method rather than four more lines in Register so the delivery service's surface grows
// one named group per task, and so a composition root that wants the registry without the
// sequencer — there is none yet, but task 27a decides that, not this file — has the choice.
func (h *Groups) RegisterSequencer(mux *server.Mux, sessions *auth.Sessions) {
	mux.SocketAuthed("GET /v1/groups/{id}/handshakes", socketMeter(sessions, h.Limiter, dsClassRead), http.HandlerFunc(h.handshakes))
	mux.SocketAuthed("POST /v1/groups/{id}/commit", socketMeter(sessions, h.Limiter, dsClassCommit), http.HandlerFunc(h.commit))
	mux.SocketAuthed("POST /v1/groups/{id}/proposal", socketMeter(sessions, h.Limiter, dsClassProposal), http.HandlerFunc(h.proposal))
	mux.SocketAuthed("GET /v1/groups/{id}/proposals", socketMeter(sessions, h.Limiter, dsClassRead), http.HandlerFunc(h.proposals))
}

// RegisterRecovery mounts the two routes a device out of step with the epoch uses: endpoint 8
// (`POST /v1/groups/{id}/resync`, the own-leaf external commit R25 exempts from invariant 5's
// freeze) and endpoint 9 (`POST /v1/groups/{id}/fork-report`, invariant 9's report). It is a
// third method for the reason RegisterSequencer is a second one: the delivery service's surface
// grows one named group per task.
func (h *Groups) RegisterRecovery(mux *server.Mux, sessions *auth.Sessions) {
	mux.SocketAuthed("POST /v1/groups/{id}/resync", socketMeter(sessions, h.Limiter, dsClassCommit), http.HandlerFunc(h.resync))
	mux.SocketAuthed("POST /v1/groups/{id}/fork-report", socketMeter(sessions, h.Limiter, dsClassWrite), http.HandlerFunc(h.forkReport))
}

// sessionOf is the one place this file reads the request's session. It is a helper in package api,
// not in package server: the context key belongs to internal/auth.
func sessionOf(r *http.Request) (ds.Session, error) {
	s, ok := auth.FromContext(r.Context())
	if !ok {
		return ds.Session{}, server.Errorf(server.CodeUnauthenticated, "no device session")
	}
	return s, nil
}

type createGroupRequest struct {
	_           struct{} `cbor:",toarray"`
	GroupID     id.ID
	Binding     []byte
	GroupInfo   []byte
	RatchetTree []byte
}

type createGroupResponse struct {
	_       struct{} `cbor:",toarray"`
	GroupID id.ID
	NextSeq uint64
}

func (h *Groups) create(w http.ResponseWriter, r *http.Request) {
	var body createGroupRequest
	if err := server.DecodeBody(w, r, h.max(), &body); err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if session.Bot != nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "managed bots must use the installation-scoped group registration route"))
		return
	}
	out, err := h.DS.Register(r.Context(), ds.RegisterRequest{
		Session:     session,
		GroupID:     body.GroupID,
		Binding:     body.Binding,
		GroupInfo:   body.GroupInfo,
		RatchetTree: body.RatchetTree,
	})
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	if err := server.EncodeBody(w, http.StatusCreated,
		createGroupResponse{GroupID: out.GroupID, NextSeq: out.NextSeq}); err != nil {
		server.WriteError(w, err)
		return
	}
	if h.AfterRegister == nil {
		return
	}
	// The hook fans instance proposals and a commit_needed election out to the creator, and
	// populating a large channel is work the registrant does not wait for. It runs on its own
	// goroutine, so the handler returns at once. Over HTTP the 201 was flushed first; on the
	// gateway socket the dispatcher's recorder cannot flush, so the hook's frames may precede the
	// op 60 answer. The client holds or drops group frames until its group is registered
	// (groupRegistered), and the election, re-election and ready-volunteer paths recover what was
	// dropped. The request's cancellation must not reach the hook: the group exists whether or
	// not the client is still connected.
	groupID := out.GroupID
	h.runAfter(w, r, func(ctx context.Context) { h.AfterRegister(ctx, groupID) })
}

// runAfter runs hook on a goroutine counted for Drain, once the answer is flushed, under a context
// the request's cancellation does not reach.
func (h *Groups) runAfter(w http.ResponseWriter, r *http.Request, hook func(ctx context.Context)) {
	_ = http.NewResponseController(w).Flush()
	ctx := context.WithoutCancel(r.Context())
	h.afterRegister.start()
	go func() {
		defer h.afterRegister.done()
		hook(ctx)
	}()
}

// Drain waits until every AfterRegister and AfterKeyPackages run the handler started has returned, or until ctx ends,
// whichever is first; it answers ctx's error in the second case. Shutdown calls it after the HTTP
// server has stopped taking requests (so no new run can start) and before the delivery service
// stops, because a run is still issuing that service's proposals.
func (h *Groups) Drain(ctx context.Context) error {
	return h.afterRegister.wait(ctx)
}

// inFlight counts running goroutines and lets a caller wait for none. It is not a sync.WaitGroup
// because a handler that outlives the HTTP server's graceful stop (the Close fallback) may start
// one while Drain is waiting, which a WaitGroup forbids.
type inFlight struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed when n returns to zero; nil while n is zero
}

func (f *inFlight) start() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == 0 {
		f.idle = make(chan struct{})
	}
	f.n++
}

func (f *inFlight) done() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n--
	if f.n == 0 {
		close(f.idle)
		f.idle = nil
	}
}

func (f *inFlight) wait(ctx context.Context) error {
	f.mu.Lock()
	idle := f.idle
	f.mu.Unlock()
	if idle == nil {
		return nil
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type groupInfoResponse struct {
	_         struct{} `cbor:",toarray"`
	Epoch     uint64
	GroupInfo []byte
	TreeHash  []byte
	NextSeq   uint64
}

func (h *Groups) info(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	out, err := h.DS.Info(r.Context(), groupID, session)
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	if err := server.EncodeBody(w, http.StatusOK, groupInfoResponse{
		Epoch: out.Epoch, GroupInfo: out.GroupInfo, TreeHash: out.TreeHash, NextSeq: out.NextSeq,
	}); err != nil {
		server.WriteError(w, err)
	}
}

type treeResponse struct {
	_           struct{} `cbor:",toarray"`
	Epoch       uint64
	RatchetTree []byte
	TreeHash    []byte
}

func (h *Groups) tree(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	out, err := h.DS.Tree(r.Context(), groupID, session)
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	if err := server.EncodeBody(w, http.StatusOK, treeResponse{
		Epoch: out.Epoch, RatchetTree: out.RatchetTree, TreeHash: out.TreeHash,
	}); err != nil {
		server.WriteError(w, err)
	}
}

// dsError maps a *ds.Error onto plan-1a's *server.Error, which writes protocol/02's CBOR body.
// The four codes that carry extra elements go through 1a's own constructors rather than through a
// generic "extra fields" struct: the element order of each extended shape is fixed in one place,
// and `scripts/check-protocol-docs.mjs` diffs 1a's code vocabulary against protocol/02 in both
// directions. A non-ds error is returned unchanged, and WriteError turns it into a 500 with an
// empty detail.
func dsError(err error) error {
	var upgrade *capability.UpgradeRequired
	if errors.As(err, &upgrade) {
		return server.Errorf(server.CodeUpgradeRequired, "wire version %d is required", auth.WireVersion)
	}
	var e *ds.Error
	if !errors.As(err, &e) {
		return err
	}
	switch server.Code(e.Code) {
	case server.CodeCommitConflict:
		return server.CommitConflict(e.WinningCommit, e.Proposals)
	case server.CodeCommitRequired:
		var ms uint64
		if e.RetryAfterMS != nil {
			ms = *e.RetryAfterMS
		}
		return server.CommitRequired(e.Proposals, ms)
	case server.CodeCommitInvalid:
		return server.CommitInvalid(e.Rule)
	case server.CodeRateLimited:
		var ms uint64
		if e.RetryAfterMS != nil {
			ms = *e.RetryAfterMS
		}
		return server.RateLimited(ms)
	default:
		converted := server.Errorf(server.Code(e.Code), "%s", e.Detail)
		if e.Status != 0 && e.Status != converted.Status() {
			return server.WithStatus(e.Status, converted)
		}
		return converted
	}
}

// queryUint reads a decimal query parameter, falling back to def when it is absent or malformed.
// It is a helper in package api: plan-1a's internal/server declares no such function, and part 1b
// does not invent names in a package it does not own (deviation B12).
func queryUint(r *http.Request, name string, def uint64) uint64 {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}

// queryLimit is queryUint for a page-size parameter the delivery service takes as an int32. A value
// past MaxInt32 saturates rather than wrapping, so it reaches the DS's own clamp as "a lot" and not
// as a negative number.
func queryLimit(r *http.Request, name string, def uint64) int32 {
	v := queryUint(r, name, def)
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v)
}

// queryCursor is queryUint for a "rows after this id" parameter the store takes as an int64. A
// value past MaxInt64 saturates: it names no row, so the page is empty, as it should be.
func queryCursor(r *http.Request, name string) int64 {
	v := queryUint(r, name, 0)
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// queryFrom is queryUint for the "first seq I still want" cursor of the catch-up reads, which the
// delivery service passes on as a uint64. It saturates at MaxInt64, the largest seq a store can
// hold: a wider cursor names no row, and left unbounded it wraps negative in the adapters.
func queryFrom(r *http.Request, name string) uint64 {
	return min(queryUint(r, name, 0), math.MaxInt64)
}

type handshakeItem struct {
	_      struct{} `cbor:",toarray"`
	Seq    uint64
	Epoch  uint64
	Kind   uint8
	Sender *uint32
	Blob   []byte
}

// handshakes is endpoint 4: the catch-up stream over the group's one seq space.
func (h *Groups) handshakes(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	from := queryFrom(r, "from")
	limit := queryLimit(r, "limit", 256)
	// The whole handshake log of a group is member-only: it names every leaf that ever committed
	// and every epoch transition. Handshakes takes the session and answers E_NOT_FOUND to a
	// non-member, exactly as Info and Tree do.
	rows, err := h.DS.Handshakes(r.Context(), groupID, session, from, limit)
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	out := make([]handshakeItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, handshakeItem{
			Seq: row.Seq, Epoch: row.Epoch, Kind: row.Kind, Sender: row.SenderLeaf, Blob: row.Blob,
		})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		server.WriteError(w, err)
	}
}

type commitRequestBody struct {
	_           struct{} `cbor:",toarray"`
	Epoch       uint64
	Commit      []byte
	GroupInfo   []byte
	Welcomes    []welcomeForBody
	RatchetTree []byte
}

type welcomeForBody struct {
	_        struct{} `cbor:",toarray"`
	DeviceID id.ID
	Blob     []byte
}

type seqEpochResponse struct {
	_     struct{} `cbor:",toarray"`
	Seq   uint64
	Epoch uint64
}

// commit is endpoint 5: body [epoch, commit, group_info, welcomes, ratchet_tree] -> [seq, epoch].
func (h *Groups) commit(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	// Everything that can be answered without the body is answered before it is read: the body
	// may be 16 MiB (deviation B37). One upload in flight per device, then the membership and
	// the epoch, from the path, the session and the first bytes of the stream.
	done, err := h.beginCommit(session.DeviceID)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	defer done()
	// Every valid body starts with 0x85 and a uint epoch, so a head that is not that is refused
	// here rather than handed to a decoder that would read up to 16 MiB to say the same.
	epoch, ok := peekCommitEpoch(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
			"the body does not start with a commit array [epoch, commit, group_info, welcomes, ratchet_tree]"))
		return
	}
	if err := h.DS.CheckCommit(r.Context(), session, groupID, epoch); err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	var body commitRequestBody
	if err := server.DecodeBody(w, r, h.maxCommit(), &body); err != nil {
		server.WriteError(w, err)
		return
	}
	// protocol/01 § Joining batches at most 256 Adds, and each Welcome is addressed to one of them.
	if len(body.Welcomes) > maxWelcomesPerCommit {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
			"%d Welcomes in one commit; at most %d (MAX_ADDS)", len(body.Welcomes), maxWelcomesPerCommit))
		return
	}
	// The headroom above the delivery service's own cap exists for a full batch of Welcomes
	// (deviation B37). Everything else the commit carries is held to that cap, as it is on every
	// other delivery-service route.
	if rest := int64(len(body.Commit) + len(body.GroupInfo) + len(body.RatchetTree)); rest > h.max() {
		server.WriteError(w, server.Errorf(server.CodeTooLarge,
			"the commit, GroupInfo and tree are %d bytes, over %d; only Welcomes may exceed it", rest, h.max()))
		return
	}
	welcomes := make([]ds.WelcomeFor, 0, len(body.Welcomes))
	for _, wf := range body.Welcomes {
		welcomes = append(welcomes, ds.WelcomeFor{DeviceID: wf.DeviceID, Blob: wf.Blob})
	}
	out, err := h.DS.Commit(r.Context(), session, groupID, ds.CommitRequest{
		Epoch: body.Epoch, Commit: body.Commit, GroupInfo: body.GroupInfo,
		Welcomes: welcomes, RatchetTree: body.RatchetTree,
	})
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	if err := server.EncodeBody(w, http.StatusOK,
		seqEpochResponse{Seq: out.Seq, Epoch: out.Epoch}); err != nil {
		server.WriteError(w, err)
	}
}

// proposal is endpoint 6: body [epoch, proposal] -> [seq].
func (h *Groups) proposal(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var body struct {
		_        struct{} `cbor:",toarray"`
		Epoch    uint64
		Proposal []byte
	}
	if err := server.DecodeBody(w, r, h.max(), &body); err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	seq, err := h.DS.Proposal(r.Context(), session, groupID, body.Epoch, body.Proposal)
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	if err := server.EncodeBody(w, http.StatusOK, struct {
		_   struct{} `cbor:",toarray"`
		Seq uint64
	}{Seq: seq}); err != nil {
		server.WriteError(w, err)
	}
}

type proposalItem struct {
	_          struct{} `cbor:",toarray"`
	Ref        []byte
	Kind       uint8
	TargetLeaf *uint32
	Blob       []byte
	Void       uint8
}

// proposals is endpoint 19: the outstanding proposals of the group's current epoch. protocol/02
// fixes the row as [ref, kind, target_leaf|null, blob, void], and the blob is the guest's own
// queued proposal — `mls_pending_proposals` has no blob column, because the bytes a committer
// includes by reference are the ones the PublicGroup stored.
func (h *Groups) proposals(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	rows, err := h.DS.Proposals(r.Context(), groupID, session)
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	out := make([]proposalItem, 0, len(rows))
	for _, p := range rows {
		var void uint8
		if p.Row.VoidAt != nil {
			void = 1
		}
		out = append(out, proposalItem{
			Ref: p.Row.Ref, Kind: p.Row.Kind, TargetLeaf: p.Row.TargetLeaf, Blob: p.Blob, Void: void,
		})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		server.WriteError(w, err)
	}
}

type resyncRequestBody struct {
	_              struct{} `cbor:",toarray"`
	ExternalCommit []byte
	GroupInfo      []byte
}

// resync is endpoint 8: body [external_commit, group_info] -> [seq, epoch], the same pair endpoint
// 5 answers. It carries no epoch of its own: a device that has fallen out of the epoch does not
// know it, which is the whole reason it is resyncing, so the instance supplies its own under the
// group lock (`ds.Resync`).
func (h *Groups) resync(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var body resyncRequestBody
	if err := server.DecodeBody(w, r, h.max(), &body); err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	out, err := h.DS.Resync(r.Context(), session, groupID, ds.ResyncRequest{
		ExternalCommit: body.ExternalCommit, GroupInfo: body.GroupInfo,
	})
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	if err := server.EncodeBody(w, http.StatusOK,
		seqEpochResponse{Seq: out.Seq, Epoch: out.Epoch}); err != nil {
		server.WriteError(w, err)
	}
}

type forkReportBody struct {
	_      struct{} `cbor:",toarray"`
	Epoch  uint64
	Seq    uint64
	Reason string
}

// forkReport is endpoint 9: body [epoch, seq, reason] -> 202 with an empty array body.
//
// The body is `[]any{}` rather than nothing at all: protocol/02's row says `202 []`, every /v1
// response is deterministic CBOR, and a client that decodes each answer as an array would have to
// special-case a zero-length one.
func (h *Groups) forkReport(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var body forkReportBody
	if err := server.DecodeBody(w, r, h.max(), &body); err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := h.DS.ForkReport(r.Context(), session, groupID, body.Epoch, body.Seq, body.Reason); err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	if err := server.EncodeBody(w, http.StatusAccepted, []any{}); err != nil {
		server.WriteError(w, err)
	}
}
