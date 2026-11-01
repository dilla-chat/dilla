package api_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/livekit/protocol/livekit"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/blob"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/sfu"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/sqlite"
	"github.com/jonasthim/dilla/internal/store/sqlite/sqlitetest"
)

// env is the shared internal/api harness Plan 2's handler tests build on: a
// real migrated SQLite repository, a fake clock, a real server.Mux behind a
// real httptest server, and a stand-in for the session middleware. Each test
// registers only the handlers it drives.
type env struct {
	t    *testing.T
	Repo store.Repository
	Clk  *clock.Fake
	Mux  *server.Mux
	Srv  *httptest.Server
	// DS records the delivery-service calls the Communities and Channels
	// handlers communityEnv and channelEnv mount make (task 4). It never stands
	// in for the delivery service's own behaviour, which internal/ds tests.
	DS *recordingDS
	// Blobs is the blob store blobEnv mounts the blob and admin routes over
	// (tasks 10 and 11); nil in every other harness.
	Blobs *blob.Store
	sess  map[string]auth.Session // bearer token -> session
}

// newEnv builds a repository over a temp SQLite file, migrates it, and returns an
// httptest server whose mux is empty; each test registers the handlers it needs.
func newEnv(t *testing.T) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dilla.db")
	sqlitetest.MigratedFile(t, path)
	write, err := sqlite.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	read, err := sqlite.OpenRead(path)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	repo := sqlite.New(write, read)
	t.Cleanup(func() { _ = repo.Close() })

	clk := clock.NewFake(time.Unix(1_790_000_000, 0).UTC())
	e := &env{
		t:    t,
		Repo: repo,
		Clk:  clk,
		Mux:  server.NewMux(),
		DS:   &recordingDS{},
		sess: map[string]auth.Session{},
	}
	e.Srv = httptest.NewServer(e.authMiddleware(e.Mux))
	t.Cleanup(e.Srv.Close)
	return e
}

// authMiddleware is the test stand-in for auth.Sessions.Middleware: it maps a
// bearer token registered by NewUser to an auth.Session on the request context.
func (e *env) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s, ok := e.sess[tok]; ok {
			r = r.WithContext(auth.WithSession(r.Context(), s))
		}
		next.ServeHTTP(w, r)
	})
}

// NewUser creates a user row plus one device, and returns the bearer token that
// authenticates as that (user, device) with an enrolled session.
func (e *env) NewUser(username string) (id.ID, string) {
	e.t.Helper()
	return e.NewUserWithID(id.New(), username)
}

// NewUserWithID is NewUser for a given user id: the identity an MLS leaf's
// credential already carries, when a test drives a real group.
func (e *env) NewUserWithID(uid id.ID, username string) (id.ID, string) {
	e.t.Helper()
	did := id.New()
	now := e.Clk.Now().Unix()
	if err := e.Repo.CreateUser(context.Background(), newAPITestUser(uid, username, now)); err != nil {
		e.t.Fatalf("CreateUser: %v", err)
	}
	if err := e.Repo.CreateDevice(context.Background(), newAPITestDevice(did, uid, now)); err != nil {
		e.t.Fatalf("CreateDevice: %v", err)
	}
	tok := uid.String()
	e.sess[tok] = auth.Session{UserID: uid, DeviceID: did, Scope: auth.ScopeEnrolled}
	return uid, tok
}

// Do sends one CBOR request and returns the status and the raw body.
func (e *env) Do(method, path, token string, body any) (int, []byte) {
	e.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := cborx.Marshal(body)
		if err != nil {
			e.t.Fatalf("cborx.Marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(e.t.Context(), method, e.Srv.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/cbor")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.Srv.Client().Do(req)
	if err != nil {
		e.t.Fatalf("Do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("ReadAll: %v", err)
	}
	return resp.StatusCode, out
}

// DoRaw sends one request with an arbitrary content type and body (a blob's raw
// octets, which are the one exception to the CBOR rule) and returns the status
// and the raw response body.
func (e *env) DoRaw(method, path, token, contentType string, body []byte) (int, []byte) {
	e.t.Helper()
	var headers map[string]string
	if contentType != "" {
		headers = map[string]string{"Content-Type": contentType}
	}
	resp := e.Request(e.t, method, path, token, headers, body)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("ReadAll: %v", err)
	}
	return resp.StatusCode, out
}

// Request sends one request with the given extra headers and body and returns
// the *http.Response, so a test can assert headers; the caller closes the body.
func (e *env) Request(t *testing.T, method, path, token string, headers map[string]string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, e.Srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.Srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do %s %s: %v", method, path, err)
	}
	return resp
}

// ErrCode decodes an E_* error body and returns its code.
func (e *env) ErrCode(body []byte) string {
	e.t.Helper()
	var arr []cbor.RawMessage
	if err := cborx.Unmarshal(body, &arr); err != nil {
		e.t.Fatalf("error body is not a CBOR array: %v (%x)", err, body)
	}
	if len(arr) == 0 {
		e.t.Fatalf("error body is an empty array")
	}
	var code string
	if err := cborx.Unmarshal(arr[0], &code); err != nil {
		e.t.Fatalf("error code: %v", err)
	}
	return code
}

// newAPITestUser is a store.UserRow with the minimum non-null columns of §4.3.
func newAPITestUser(uid id.ID, username string, created int64) store.UserRow {
	return store.UserRow{
		ID: uid, Username: username + "-" + uid.String()[:8], Display: username,
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64),
		Created: created,
	}
}

// newAPITestDevice is a store.DeviceRow with the minimum non-null columns of §4.3.
func newAPITestDevice(did, uid id.ID, created int64) store.DeviceRow {
	return store.DeviceRow{
		ID: did, UserID: uid, DSKPub: make([]byte, 32), CredentialBlob: []byte{1},
		LastSeen: created, Created: created,
	}
}

// seedTextGroup writes one open mls_groups row of kind text bound to channelID
// in community cid, and returns its id. It is a row, not an MLS group: the
// handlers under test read it through GroupsForTarget and hand its id to the
// delivery service, which the recording double stands behind.
func seedTextGroup(t *testing.T, e *env, channelID, cid id.ID) id.ID {
	t.Helper()
	return seedGroupOfKind(t, e, channelID, cid, 0)
}

// seedGroupOfKind is seedTextGroup for any group kind (1 is a call group).
func seedGroupOfKind(t *testing.T, e *env, target, cid id.ID, kind uint8) id.ID {
	t.Helper()
	c := cid
	g := store.GroupRow{
		GroupID: id.New(), Binding: []byte{0x80}, Kind: kind, CommunityID: &c, TargetID: target,
		Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1,
		PolicyVersion: 1, Created: e.Clk.Now().Unix(),
	}
	if err := e.Repo.CreateGroup(t.Context(), g); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	return g.GroupID
}

// seedMember adds one mls_members row: user at leaf in group, on a fresh device.
func seedMember(t *testing.T, e *env, group, user id.ID, leaf uint32) {
	t.Helper()
	members, err := e.Repo.ListMembers(t.Context(), group)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	members = append(members, store.MemberRow{
		GroupID: group, LeafIndex: leaf, UserID: user, DeviceID: id.New(),
		SignatureKey: make([]byte, 32),
	})
	if err := e.Repo.ReplaceMembers(t.Context(), group, 0, members); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
}

// ownerOf reads the community's owner.
func ownerOf(t *testing.T, e *env, cid id.ID) id.ID {
	t.Helper()
	row, err := e.Repo.GetCommunity(t.Context(), cid)
	if err != nil {
		t.Fatalf("GetCommunity: %v", err)
	}
	return row.Owner
}

// seedDevices gives user n more devices and returns their ids, in creation
// order. They carry no KeyPackage; seedKeyPackage publishes one.
func seedDevices(t *testing.T, e *env, user id.ID, n int) []id.ID {
	t.Helper()
	now := e.Clk.Now().Unix()
	out := make([]id.ID, 0, n)
	for range n {
		did := id.New()
		if err := e.Repo.CreateDevice(t.Context(), newAPITestDevice(did, user, now)); err != nil {
			t.Fatalf("CreateDevice: %v", err)
		}
		out = append(out, did)
	}
	return out
}

// seedKeyPackage publishes one ordinary (not last-resort) KeyPackage row for
// device, valid for a day. It is a row, not a KeyPackage: the code under test
// only counts what is available, and the recording double stands in for the
// delivery service that would take and validate it.
func seedKeyPackage(t *testing.T, e *env, device id.ID) {
	t.Helper()
	now := e.Clk.Now().Unix()
	ref := id.New()
	if err := e.Repo.PutKeyPackages(t.Context(), device, []store.KeyPackageRow{{
		DeviceID: device, KPRef: ref[:], Blob: []byte{1}, Expires: now + 86_400, Created: now,
	}}); err != nil {
		t.Fatalf("PutKeyPackages: %v", err)
	}
}

// userOf is the user a bearer token NewUser minted authenticates as.
func userOf(t *testing.T, e *env, tok string) id.ID {
	t.Helper()
	s, ok := e.sess[tok]
	if !ok {
		t.Fatalf("no session for token %q", tok)
	}
	return s.UserID
}

// codeOf is the E_* code of a *server.Error, or "" for nil or any other error.
func codeOf(err error) string {
	var se *server.Error
	if !errors.As(err, &se) {
		return ""
	}
	return string(se.Code)
}

// repoRoot is the repository root: go test runs in the package directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	return root
}

// attsToAny spells attachments as protocol/04's 9-element arrays, an empty list
// as an empty array (never null).
func attsToAny(atts []api.Attachment) []any {
	out := make([]any, 0, len(atts))
	for _, a := range atts {
		out = append(out, []any{a.BlobID, a.Key, a.Nonce, a.Size, a.Mime, a.W, a.H, a.Thumb, a.Name})
	}
	return out
}

// prevsToAny spells previews as protocol/04's 4-element arrays.
func prevsToAny(prevs []api.Preview) []any {
	out := make([]any, 0, len(prevs))
	for _, p := range prevs {
		out = append(out, []any{p.URL, p.Title, p.Description, p.Image})
	}
	return out
}

// ptr returns a pointer to a copy of v.
func ptr[T any](v T) *T { return &v }

// mint is one token stubSFU minted.
type mint struct {
	Room, Identity string
	Perm           *livekit.ParticipantPermission
	Attrs          map[string]string
}

// permUpdate is one UpdatePermission stubSFU was asked for.
type permUpdate struct {
	Room, Identity string
	Perm           *livekit.ParticipantPermission
}

// stubSFU stands in for internal/sfu.(*Server): the JWT's contents are internal/sfu's own test's
// business, and the gates in front of it are this file's.
type stubSFU struct {
	mu         sync.Mutex
	mints      []mint
	fail       error
	deleted    []string // rooms DeleteRoom was asked to close
	deleteFail error
	created    []string // rooms CreateRoom was asked to open
	createFail error
	updates    []permUpdate
	attrs      []attrUpdate // UpdateAttributes calls (task 8c)
	updateFail error
	absent     map[string]bool  // identities UpdatePermission answers sfu.ErrNoParticipant for
	onUpdate   func(permUpdate) // runs after the update is recorded, outside the lock
	// beforeUpdate runs before the update is recorded (applied), outside the lock: a test parks a
	// push there to order it after a concurrent one.
	beforeUpdate func(permUpdate)
	removed      [][2]string // (room, device) RemoveParticipants was asked for
	removedIDs   [][2]string // (room, identity) RemoveParticipant was asked for
	// live is the permission the stub SFU holds per identity: set by an applied update, dropped by
	// a removal.
	live         map[string]*livekit.ParticipantPermission
	failUpdates  int // the next failUpdates pushes fail (and are not applied)
	failRemovals int // the next failRemovals RemoveParticipants calls fail
	// beforeRemove runs at the start of every RemoveParticipants, outside the lock: a test reads
	// what a client would see while the cut is under way.
	beforeRemove func(room string, device id.ID)
	hang         bool // pushes and removals block until their context ends: a hung SFU
	present      map[string][]*livekit.ParticipantInfo
	listFail     error
	// rooms is every room the stub SFU holds: opened by CreateRoom or setPresent, closed by a
	// DeleteRoom that succeeds. roomsFail fails Rooms.
	rooms     map[string]bool
	roomsFail error
	// sids is the LiveKit room sid of each room the stub holds, set by setSID (none: "").
	sids map[string]string
}

// RoomSID is sfu.Server's: the sid of the room the stub holds under that name now.
func (s *stubSFU) RoomSID(_ context.Context, room string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sids[room], s.rooms[room], nil
}

// setSID names the incarnation of room the stub holds, opening it.
func (s *stubSFU) setSID(room, sid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sids == nil {
		s.sids = map[string]string{}
	}
	if s.rooms == nil {
		s.rooms = map[string]bool{}
	}
	s.sids[room], s.rooms[room] = sid, true
}

func (s *stubSFU) Rooms(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	if s.hang {
		s.mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	defer s.mu.Unlock()
	if s.roomsFail != nil {
		return nil, s.roomsFail
	}
	out := make([]string, 0, len(s.rooms))
	for r := range s.rooms {
		out = append(out, r)
	}
	slices.Sort(out)
	return out, nil
}

// addRoom opens room in the stub SFU with nobody in it, as a room left behind would be.
func (s *stubSFU) addRoom(room string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rooms == nil {
		s.rooms = map[string]bool{}
	}
	s.rooms[room] = true
}

// heldRooms is the rooms the stub holds.
func (s *stubSFU) heldRooms() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.rooms))
	for r := range s.rooms {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

var _ api.CallTokens = (*stubSFU)(nil)

func (s *stubSFU) Token(room, identity string, perm *livekit.ParticipantPermission, attrs map[string]string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return "", s.fail
	}
	s.mints = append(s.mints, mint{Room: room, Identity: identity, Perm: perm, Attrs: attrs})
	return "jwt-for-" + identity, nil
}

func (s *stubSFU) DeleteRoom(_ context.Context, room string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, room)
	if s.deleteFail == nil {
		delete(s.rooms, room)
		delete(s.present, room)
	}
	return s.deleteFail
}

func (s *stubSFU) CreateRoom(_ context.Context, room string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, room)
	if s.createFail == nil {
		if s.rooms == nil {
			s.rooms = map[string]bool{}
		}
		s.rooms[room] = true
	}
	return s.createFail
}

func (s *stubSFU) UpdatePermission(ctx context.Context, room, identity string, perm *livekit.ParticipantPermission) error {
	u := permUpdate{Room: room, Identity: identity, Perm: perm}
	s.mu.Lock()
	before, hang := s.beforeUpdate, s.hang
	s.mu.Unlock()
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	if before != nil {
		before(u)
	}
	s.mu.Lock()
	s.updates = append(s.updates, u)
	hook, fail, absent := s.onUpdate, s.updateFail, s.absent[identity]
	if s.failUpdates > 0 {
		s.failUpdates--
		fail = errors.New("stub SFU: push failed")
	}
	if !absent && fail == nil {
		if s.live == nil {
			s.live = map[string]*livekit.ParticipantPermission{}
		}
		s.live[identity] = perm
	}
	s.mu.Unlock()
	if hook != nil {
		hook(u)
	}
	if absent {
		return sfu.ErrNoParticipant
	}
	return fail
}

func (s *stubSFU) RemoveParticipants(ctx context.Context, room string, device id.ID) error {
	s.mu.Lock()
	before := s.beforeRemove
	s.mu.Unlock()
	if before != nil {
		before(room, device)
	}
	s.mu.Lock()
	if s.hang {
		s.mu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	}
	defer s.mu.Unlock()
	s.removed = append(s.removed, [2]string{room, device.String()})
	if s.failRemovals > 0 {
		s.failRemovals--
		return errors.New("stub SFU: removal failed")
	}
	for identity := range s.live {
		if identity == device.String() || strings.HasPrefix(identity, device.String()+"#") {
			delete(s.live, identity)
		}
	}
	return nil
}

func (s *stubSFU) RemoveParticipant(_ context.Context, room, identity string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removedIDs = append(s.removedIDs, [2]string{room, identity})
	delete(s.live, identity)
	return nil
}

// removals is every RemoveParticipants (device) and RemoveParticipant (identity) call.
func (s *stubSFU) removals() (devices, identities [][2]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]string(nil), s.removed...), append([][2]string(nil), s.removedIDs...)
}

// lastPerms is the permission the SFU holds for each participant now: the last one applied to it,
// for as long as no removal took it out of the room.
func (s *stubSFU) lastPerms() map[string]*livekit.ParticipantPermission {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.live)
}

func (s *stubSFU) Participants(_ context.Context, room string) ([]*livekit.ParticipantInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listFail != nil {
		return nil, s.listFail
	}
	return s.present[room], nil
}

func (s *stubSFU) deletedRooms() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

func (s *stubSFU) createdRooms() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.created...)
}

// minted is every token as (room, identity), in mint order.
func (s *stubSFU) minted() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][2]string, 0, len(s.mints))
	for _, m := range s.mints {
		out = append(out, [2]string{m.Room, m.Identity})
	}
	return out
}

// attrUpdate is one UpdateAttributes call (task 8c's dilla.rtt).
type attrUpdate struct {
	Room, Identity string
	Attrs          map[string]string
}

// UpdateAttributes records the call; an identity in absent answers sfu.ErrNoParticipant.
func (s *stubSFU) UpdateAttributes(_ context.Context, room, identity string, attrs map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.absent[identity] {
		return sfu.ErrNoParticipant
	}
	s.attrs = append(s.attrs, attrUpdate{Room: room, Identity: identity, Attrs: attrs})
	return nil
}

func (s *stubSFU) attrUpdates() []attrUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]attrUpdate(nil), s.attrs...)
}

func (s *stubSFU) lastMint() mint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.mints) == 0 {
		return mint{}
	}
	return s.mints[len(s.mints)-1]
}

func (s *stubSFU) permUpdates() []permUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]permUpdate(nil), s.updates...)
}

func (s *stubSFU) setPresent(room string, parts ...*livekit.ParticipantInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.present == nil {
		s.present = map[string][]*livekit.ParticipantInfo{}
	}
	s.present[room] = parts
	if s.rooms == nil {
		s.rooms = map[string]bool{}
	}
	s.rooms[room] = true
}

const (
	testTURNSecret = "0123456789abcdef0123456789abcdef"
	testLiveKitURL = "wss://chat.example.test"
	callGroupEpoch = 7
)

// callEnv is a voice channel with a call group at epoch 7, the owner's token,
// and the calls routes mounted over a stub SFU with TURN on.
func callEnv(t *testing.T) (*env, id.ID, string, id.ID) {
	t.Helper()
	e, ch, tok, group, _ := callEnvWith(t, api.CallsConfig{
		LiveKitURL: testLiveKitURL, TURNSecret: testTURNSecret,
		TURNURLs:      []string{"turns:chat.example.test:443?transport=tcp"},
		CredentialTTL: time.Hour,
	})
	return e, ch, tok, group
}

func callEnvWith(t *testing.T, cfg api.CallsConfig) (*env, id.ID, string, id.ID, *stubSFU) {
	t.Helper()
	e, ch, tok, group, stub, _ := callEnvCalls(t, cfg)
	return e, ch, tok, group, stub
}

// callEnvCalls is callEnvWith that also hands back the mounted *api.Calls, for the tests that drive
// SyncCallGrants and the call events through it.
func callEnvCalls(t *testing.T, cfg api.CallsConfig) (*env, id.ID, string, id.ID, *stubSFU, *api.Calls) {
	t.Helper()
	e, cid, tok := channelEnv(t)
	ch, _, status := newChannel(t, e, cid, tok, 1 /* voice */, 1, 2, "voice")
	if status != http.StatusCreated {
		t.Fatalf("voice channel = %d", status)
	}
	stub := &stubSFU{}
	calls := api.NewCalls(e.Repo, api.NewResolver(e.Repo), stub, cfg, e.Clk, slog.New(slog.DiscardHandler))
	calls.Register(e.Mux)
	return e, ch, tok, seedCallGroup(t, e, ch, cid, callGroupEpoch), stub, calls
}

// seedCallGroup writes one open call group row bound to the channel at the
// given epoch, with the call id the delivery service gives a call group (R9:
// its target, the channel).
func seedCallGroup(t *testing.T, e *env, ch, cid id.ID, epoch uint64) id.ID {
	t.Helper()
	c, call := cid, ch
	g := store.GroupRow{
		GroupID: id.New(), Binding: []byte{0x80}, Kind: api.GroupCall, CommunityID: &c, TargetID: ch,
		CallID: &call, Ciphersuite: 1, Epoch: epoch, ExternalSenderKeyID: id.New(),
		E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: e.Clk.Now().Unix(),
	}
	if err := e.Repo.CreateGroup(t.Context(), g); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	return g.GroupID
}

// deviceOf is the device a bearer token NewUser minted authenticates as.
func deviceOf(t *testing.T, e *env, tok string) id.ID {
	t.Helper()
	s, ok := e.sess[tok]
	if !ok {
		t.Fatalf("no session for token %q", tok)
	}
	return s.DeviceID
}

// seedLeaf adds dev as the next leaf of group, added at addedEpoch and removed
// at removedEpoch (nil while it is still a member).
func seedLeaf(t *testing.T, e *env, group, dev id.ID, addedEpoch uint64, removedEpoch *uint64) {
	t.Helper()
	members, err := e.Repo.ListMembers(t.Context(), group)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	d, err := e.Repo.GetDevice(t.Context(), dev)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	members = append(members, store.MemberRow{
		GroupID: group, LeafIndex: uint32(len(members)), UserID: d.UserID, DeviceID: dev,
		SignatureKey: make([]byte, 32), AddedEpoch: addedEpoch, RemovedEpoch: removedEpoch,
	})
	if err := e.Repo.ReplaceMembers(t.Context(), group, callGroupEpoch, members); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
}
