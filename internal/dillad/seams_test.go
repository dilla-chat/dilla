package dillad_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/dillad/dilladtest"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres"
	pgmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
)

// Fix wave I18: the composition root, built as `dillad serve` builds it (no ACL and no channel
// source in Options), answers invariant 4's eligibility with the permission resolver. The group
// GroupInfo read external joiners need is the probe: a participant of a group DM who holds no leaf
// yet may read it (the resolver admits a DM's participants), and an unrelated account may not (404,
// as for a group that does not exist). An admit-everyone seam would let the stranger in; Plan 1's
// ds.DenyUnlessMember would shut the participant out.
func TestTheCompositionRootWiresTheResolverACL(t *testing.T) {
	srv, h, _ := newInstance(t)
	ctx := t.Context()
	seeded, err := dilladtest.SeedUsers(ctx, srv, []dilladtest.SeedRequest{
		seedRequest("ana"), seedRequest("ben"), seedRequest("stranger"),
	})
	if err != nil {
		t.Fatalf("SeedUsers: %v", err)
	}
	token := func(i int) string {
		for _, tok := range seeded[i].Tokens {
			return tok
		}
		t.Fatalf("no session for %s", seeded[i].UserID)
		return ""
	}
	user := func(i int) id.ID {
		u, err := id.Parse(seeded[i].UserID)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	// A seeded human owes a second factor as a real sign-up does (web3.6 S1); this test is about
	// the ACL, so the factor is satisfied.
	for i := range seeded {
		satisfyFactor(t, srv, user(i))
	}

	repo := srv.Repo()
	now := srv.Now().Unix()
	dm, group := id.New(), id.New()
	if err := repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateChannel(ctx, store.ChannelRow{
			ID: dm, Kind: api.ChannelGroupDM, SettingsJSON: []byte("{}"), HostPolicyVersion: 1, Created: now,
		}); err != nil {
			return err
		}
		for _, u := range []id.ID{user(0), user(1)} {
			if err := tx.PutChannelMember(ctx, dm, u, now); err != nil {
				return err
			}
		}
		return tx.CreateGroup(ctx, store.GroupRow{
			GroupID: group, Binding: []byte{0x80}, Kind: api.GroupText, TargetID: dm, Ciphersuite: 1,
			Epoch: 1, GroupInfoBlob: []byte("group info"), ExternalSenderKeyID: id.New(),
			E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: now,
		})
	}); err != nil {
		t.Fatalf("seed the group DM: %v", err)
	}

	info := "/v1/groups/" + group.String() + "/info"
	if rec := call(t, h, http.MethodGet, info, token(1), nil); rec.Code != http.StatusOK {
		t.Errorf("GET info as a participant with no leaf = %d %s, want 200: the resolver admits a DM's participants",
			rec.Code, errorCode(rec))
	}
	rec := call(t, h, http.MethodGet, info, token(2), nil)
	if rec.Code != http.StatusNotFound || errorCode(rec) != "E_NOT_FOUND" {
		t.Errorf("GET info as an unrelated account = %d %s, want 404 E_NOT_FOUND", rec.Code, errorCode(rec))
	}
}

// seedRequest is one account with one device and all-zero public keys: the reads here verify no
// signature.
func seedRequest(name string) dilladtest.SeedRequest {
	return dilladtest.SeedRequest{
		Username: name, Display: name,
		UMKPub: strings.Repeat("00", 32), SSKPub: strings.Repeat("00", 32), SigUMKSSK: strings.Repeat("00", 64),
		Devices: []dilladtest.Device{{DeviceID: id.New().String(), DSKPub: strings.Repeat("00", 32), Tier: 1, Credential: "80"}},
	}
}

// ------------------------------------------------------------------ the post policy (web3.7 SB1)

// postInstance is a composition root built as `dillad serve` builds it, over a frozen clock, on
// SQLite or, when engine is "postgres", on a fresh Postgres database holding the same instance row.
func postInstance(t *testing.T, engine string, clk clock.Clock) (*dillad.Server, http.Handler) {
	t.Helper()
	ctx := context.Background()
	cfg, _ := testConfigInvite(t)
	var repo store.Repository
	if engine == "postgres" {
		read, err := sqlite.OpenRead(cfg.DB.Path)
		if err != nil {
			t.Fatalf("OpenRead: %v", err)
		}
		inst, err := sqlite.New(read, read).GetInstance(ctx)
		_ = read.Close()
		if err != nil {
			t.Fatalf("GetInstance: %v", err)
		}
		pg := freshPostgres(t)
		if err := pg.CreateInstance(ctx, inst); err != nil {
			t.Fatalf("CreateInstance on Postgres: %v", err)
		}
		repo = pg
	}
	srv, err := dillad.New(ctx, dillad.Options{Config: cfg, Clock: clk, Wasm: sharedRuntime(t), Repo: repo})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv, socketHandler(srv)
}

// freshPostgres is a migrated repository in a database of its own on the server DILLA_TEST_PG
// names, dropped at cleanup.
func freshPostgres(t *testing.T) store.Repository {
	t.Helper()
	dsn := os.Getenv("DILLA_TEST_PG")
	admin, err := postgres.Open(dsn, 1, time.Hour)
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	name := "dilla_t_" + id.New().String()[:16]
	if _, err := admin.ExecContext(context.Background(), `CREATE DATABASE `+name); err != nil {
		_ = admin.Close()
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DILLA_TEST_PG is not a URL: %v", err)
	}
	u.Path = "/" + name
	db, err := postgres.Open(u.String(), 16, time.Hour)
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
	})
	p, err := goose.NewProvider(goose.DialectPostgres, db, pgmigrations.FS)
	if err != nil {
		t.Fatalf("postgres provider: %v", err)
	}
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatalf("postgres up: %v", err)
	}
	return postgres.New(db)
}

// postEngines is SQLite, plus Postgres when DILLA_TEST_PG names a server.
func postEngines(t *testing.T) []string {
	if os.Getenv("DILLA_TEST_PG") == "" {
		t.Log("DILLA_TEST_PG is unset: the Postgres leg runs in CI's service container")
		return []string{"sqlite"}
	}
	return []string{"sqlite", "postgres"}
}

// postFixture is a community owned by someone else, one text channel with slowmode seconds of slow
// mode, @everyone allowing everyone, and a member whose one device holds leaf 0 of the channel's
// text group at epoch 1. grant gives the member a role allowing more.
type postFixture struct {
	srv    *dillad.Server
	h      http.Handler
	token  string
	group  id.ID
	grant  func(api.Bits)
	upload func(body []any) *httptest.ResponseRecorder
}

func newPostFixture(t *testing.T, engine string, clk clock.Clock, everyone api.Bits, slowmode uint64) *postFixture {
	t.Helper()
	srv, h := postInstance(t, engine, clk)
	ctx := t.Context()
	seeded, err := dilladtest.SeedUsers(ctx, srv, []dilladtest.SeedRequest{seedRequest("owner"), seedRequest("member")})
	if err != nil {
		t.Fatalf("SeedUsers: %v", err)
	}
	ids := make([]id.ID, len(seeded))
	for i := range seeded {
		u, err := id.Parse(seeded[i].UserID)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = u
		satisfyFactor(t, srv, u)
	}
	owner, member := ids[0], ids[1]
	var token string
	var device id.ID
	for dev, tok := range seeded[1].Tokens {
		token = tok
		if device, err = id.Parse(dev); err != nil {
			t.Fatal(err)
		}
	}
	repo := srv.Repo()
	now := srv.Now().Unix()
	cid, channel, group := id.New(), id.New(), id.New()
	if err := repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateCommunity(ctx, store.CommunityRow{
			ID: cid, Owner: owner, Name: "c", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now,
		}); err != nil {
			return err
		}
		if err := tx.PutRole(ctx, store.RoleRow{
			ID: id.New(), CommunityID: cid, Name: "@everyone", Position: 0, Allow: uint64(everyone), Created: now,
		}); err != nil {
			return err
		}
		if err := tx.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: member, Joined: now}); err != nil {
			return err
		}
		c := cid
		if err := tx.CreateChannel(ctx, store.ChannelRow{
			ID: channel, CommunityID: &c, Kind: api.ChannelText, Mode: api.ModeE2EE, Name: "general",
			SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, SlowmodeSeconds: slowmode, Created: now,
		}); err != nil {
			return err
		}
		if err := tx.CreateGroup(ctx, store.GroupRow{
			GroupID: group, Binding: []byte{0x80}, Kind: api.GroupText, CommunityID: &c, TargetID: channel,
			Ciphersuite: 1, Epoch: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1,
			PolicyVersion: 1, Created: now,
		}); err != nil {
			return err
		}
		return tx.ReplaceMembers(ctx, group, 1, []store.MemberRow{{
			GroupID: group, LeafIndex: 0, UserID: member, DeviceID: device,
			SignatureKey: bytes.Repeat([]byte{0x5c}, 32), AddedEpoch: 1,
		}})
	}); err != nil {
		t.Fatalf("seed the community and its text group: %v", err)
	}
	position := uint64(1)
	return &postFixture{
		srv: srv, h: h, token: token, group: group,
		grant: func(bits api.Bits) {
			t.Helper()
			role := id.New()
			if err := repo.PutRole(ctx, store.RoleRow{
				ID: role, CommunityID: cid, Name: "r", Position: position, Allow: uint64(bits), Created: now,
			}); err != nil {
				t.Fatalf("PutRole: %v", err)
			}
			position++
			if err := repo.PutMemberRole(ctx, cid, member, role); err != nil {
				t.Fatalf("PutMemberRole: %v", err)
			}
		},
		upload: func(body []any) *httptest.ResponseRecorder {
			return call(t, h, http.MethodPost, "/v1/groups/"+group.String()+"/message", token, body)
		},
	}
}

// postBody is endpoint 7's body at epoch 1, with the class when class is not nil: a hand-framed
// RFC 9420 PrivateMessage whose header the guest parses without decrypting.
func postBody(class *uint64) []any {
	var pm []byte
	vl := func(v []byte) { pm = append(pm, byte(len(v))); pm = append(pm, v...) }
	pm = binary.BigEndian.AppendUint16(pm, 1) // protocol_version: MLS 1.0
	pm = binary.BigEndian.AppendUint16(pm, 2) // wire_format: PrivateMessage
	vl([]byte("group-id"))
	pm = binary.BigEndian.AppendUint64(pm, 1) // epoch
	pm = append(pm, 1)                        // content_type: application
	vl(bytes.Repeat([]byte{5}, 32))           // authenticated_data: the commitment
	vl(bytes.Repeat([]byte{7}, 16))           // encrypted_sender_data
	vl(bytes.Repeat([]byte{9}, 48))           // ciphertext
	if class == nil {
		return []any{uint64(1), pm}
	}
	return []any{uint64(1), pm, *class}
}

func classPtr(v uint64) *uint64 { return &v }

// The composition root wires api.PostPolicy into the delivery service: a member without
// send_messages is refused a post (class 0); with a role that grants it, the post is accepted. A
// two-element body is 400 E_INVALID_REQUEST even then (web3.8 S1).
func TestTheCompositionRootWiresThePostPolicy(t *testing.T) {
	f := newPostFixture(t, "sqlite", clock.System(), api.PermViewChannel, 0)
	for _, c := range []struct {
		name string
		body []any
	}{{"class 0", postBody(classPtr(0))}} {
		if rec := f.upload(c.body); rec.Code != http.StatusForbidden || errorCode(rec) != "E_FORBIDDEN" {
			t.Fatalf("%s without send_messages = %d %s, want 403 E_FORBIDDEN", c.name, rec.Code, rec.Body.String())
		}
	}
	f.grant(api.PermSendMessages)
	for _, c := range []struct {
		name string
		body []any
	}{{"class 0", postBody(classPtr(0))}} {
		if rec := f.upload(c.body); rec.Code != http.StatusOK {
			t.Fatalf("%s with send_messages = %d %s, want 200", c.name, rec.Code, rec.Body.String())
		}
	}
	if rec := f.upload(postBody(nil)); rec.Code != http.StatusBadRequest || errorCode(rec) != "E_INVALID_REQUEST" {
		t.Fatalf("a two-element body with send_messages = %d %s, want 400 E_INVALID_REQUEST", rec.Code, rec.Body.String())
	}
}

// Slow mode is decided under the group lock: eight posts by one user from one device at one frozen
// instant admit exactly one; the other seven wait. Control: without slow mode all eight land.
func TestConcurrentPostsInSlowModeAdmitOne(t *testing.T) {
	for _, engine := range postEngines(t) {
		for _, c := range []struct {
			slowmode uint64
			ok, slow int
		}{{60, 1, 7}, {0, 8, 0}} {
			t.Run(engine+"/slowmode "+strconv.FormatUint(c.slowmode, 10), func(t *testing.T) {
				clk := clock.NewFake(time.Unix(time.Now().Unix(), 0))
				f := newPostFixture(t, engine, clk, api.PermViewChannel|api.PermSendMessages, c.slowmode)
				codes := make([]int, 8)
				var wg sync.WaitGroup
				for i := range codes {
					wg.Add(1)
					go func() {
						defer wg.Done()
						codes[i] = f.upload(postBody(classPtr(0))).Code
					}()
				}
				wg.Wait()
				var ok, slow int
				for _, code := range codes {
					switch code {
					case http.StatusOK:
						ok++
					case http.StatusTooManyRequests:
						slow++
					}
				}
				if ok != c.ok || slow != c.slow {
					t.Fatalf("slow mode %d: %d accepted and %d slowed (%v), want %d and %d",
						c.slowmode, ok, slow, codes, c.ok, c.slow)
				}
			})
		}
	}
}

// SB1 fix round 1: only an accepted post (class 0) sets the mark. A fold (class 1) is never
// slowed, and does not start the wait for the post after it.
func TestOnlyAPostSetsTheSlowModeMark(t *testing.T) {
	clk := clock.NewFake(time.Unix(time.Now().Unix(), 0))
	f := newPostFixture(t, "sqlite", clk, api.PermViewChannel|api.PermSendMessages, 3600)
	for _, c := range []struct {
		name string
		body []any
		want int
	}{
		{"a fold", postBody(classPtr(1)), http.StatusOK},
		{"the first post", postBody(classPtr(0)), http.StatusOK},
		{"the second post", postBody(classPtr(0)), http.StatusTooManyRequests},
	} {
		if rec := f.upload(c.body); rec.Code != c.want {
			t.Fatalf("%s = %d %s, want %d", c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
}

// SB1 fix round 1 (review finding 1): slow mode keeps its mark when retention prunes the post. The
// sweep deletes a message every eligible cursor has passed, so a mark read from mls_app_messages
// would reset within a minute in a caught-up channel; the durable mark holds the wait. Control:
// once the window has passed, the next post is accepted.
func TestAPrunedPostStillHoldsSlowMode(t *testing.T) {
	for _, engine := range postEngines(t) {
		t.Run(engine, func(t *testing.T) {
			clk := clock.NewFake(time.Unix(time.Now().Unix(), 0))
			f := newPostFixture(t, engine, clk, api.PermViewChannel|api.PermSendMessages, 3600)
			rec := f.upload(postBody(classPtr(0)))
			if rec.Code != http.StatusOK {
				t.Fatalf("the first post = %d %s", rec.Code, rec.Body.String())
			}
			var out []any
			if err := cborx.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out) != 3 {
				t.Fatalf("the upload response: %v", err)
			}
			seq, _ := out[0].(uint64)
			n, err := f.srv.Repo().PruneAppMessages(t.Context(), f.group, seq, 0, clk.Now().Unix())
			if err != nil || n != 1 {
				t.Fatalf("PruneAppMessages = %d, %v; want the post pruned", n, err)
			}
			clk.Advance(60 * time.Second)
			if rec := f.upload(postBody(classPtr(0))); rec.Code != http.StatusTooManyRequests || errorCode(rec) != "E_RATE_LIMITED" {
				t.Fatalf("a post 60 s into a 3600 s slow mode after the prune = %d %s, want 429 E_RATE_LIMITED",
					rec.Code, rec.Body.String())
			}
			clk.Advance(3600 * time.Second)
			if rec := f.upload(postBody(classPtr(0))); rec.Code != http.StatusOK {
				t.Fatalf("control: a post after the window = %d %s, want 200", rec.Code, rec.Body.String())
			}
		})
	}
}
