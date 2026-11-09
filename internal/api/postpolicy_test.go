package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres"
	pgmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
)

// The post policy (web3.7 SB1, protocol/02 § Upload class): a class-0 upload into a community
// text group needs send_messages and waits out slow mode; a class-1 upload (a fold) is never
// checked; a legacy two-element upload (a nil class) needs send_messages and is never slowed (R28).

func classOf(v uint8) *uint8 { return &v }

// policyEngines is newEnv's SQLite repository, plus a migrated Postgres one in a database of its
// own when DILLA_TEST_PG names a server, so every policy case runs on both engines.
func policyEngines(t *testing.T) map[string]store.Repository {
	t.Helper()
	out := map[string]store.Repository{"sqlite": newEnv(t).Repo}
	dsn := os.Getenv("DILLA_TEST_PG")
	if dsn == "" {
		t.Log("DILLA_TEST_PG is unset: the Postgres leg runs in CI's service container")
		return out
	}
	admin, err := postgres.Open(dsn, 1, time.Hour)
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	name := "dilla_t_" + id.New().String()[:16]
	if _, err := admin.ExecContext(context.Background(), `CREATE DATABASE `+name); err != nil {
		_ = admin.Close()
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DILLA_TEST_PG is not a URL: %v", err)
	}
	u.Path = "/" + name
	db, err := postgres.Open(u.String(), 4, time.Hour)
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, pgmigrations.FS)
	if err != nil {
		t.Fatalf("postgres provider: %v", err)
	}
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatalf("postgres up: %v", err)
	}
	repo := postgres.New(db)
	t.Cleanup(func() { _ = repo.Close() })
	out["postgres"] = repo
	return out
}

// policyFixture is a community owned by someone else, one text channel, the @everyone role, a
// member with two devices, and a kind-0 group bound to the channel.
type policyFixture struct {
	t        *testing.T
	repo     store.Repository
	cid      id.ID
	channel  store.ChannelRow
	group    store.GroupRow
	user     id.ID
	phone    id.ID
	laptop   id.ID
	seq      uint64
	position uint64
}

const policyT0 = int64(1_790_000_000)

func newPolicyFixture(t *testing.T, repo store.Repository, everyone api.Bits, slowmode uint64) *policyFixture {
	t.Helper()
	ctx := context.Background()
	f := &policyFixture{t: t, repo: repo, cid: id.New(), user: id.New(), phone: id.New(), laptop: id.New(), position: 1}
	owner := id.New()
	for _, uid := range []id.ID{owner, f.user} {
		if err := repo.CreateUser(ctx, newAPITestUser(uid, "u", policyT0)); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
	}
	for _, did := range []id.ID{f.phone, f.laptop} {
		if err := repo.CreateDevice(ctx, newAPITestDevice(did, f.user, policyT0)); err != nil {
			t.Fatalf("CreateDevice: %v", err)
		}
	}
	if err := repo.CreateCommunity(ctx, store.CommunityRow{
		ID: f.cid, Owner: owner, Name: "c", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: policyT0,
	}); err != nil {
		t.Fatalf("CreateCommunity: %v", err)
	}
	if err := repo.PutRole(ctx, store.RoleRow{
		ID: id.New(), CommunityID: f.cid, Name: "@everyone", Position: 0, Allow: uint64(everyone), Created: policyT0,
	}); err != nil {
		t.Fatalf("PutRole(@everyone): %v", err)
	}
	if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: f.cid, UserID: f.user, Joined: policyT0}); err != nil {
		t.Fatalf("PutMember: %v", err)
	}
	cid := f.cid
	f.channel = store.ChannelRow{
		ID: id.New(), CommunityID: &cid, Kind: api.ChannelText, Mode: api.ModeE2EE, Name: "general",
		SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, SlowmodeSeconds: slowmode, Created: policyT0,
	}
	if err := repo.CreateChannel(ctx, f.channel); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	f.group = f.newGroup(0, &cid)
	return f
}

func (f *policyFixture) newGroup(kind uint8, community *id.ID) store.GroupRow {
	f.t.Helper()
	g := store.GroupRow{
		GroupID: id.New(), Binding: []byte{0x80}, Kind: kind, CommunityID: community, TargetID: f.channel.ID,
		Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1,
		Created: policyT0,
	}
	if err := f.repo.CreateGroup(context.Background(), g); err != nil {
		f.t.Fatalf("CreateGroup: %v", err)
	}
	return g
}

// grant gives the member a new role allowing bits.
func (f *policyFixture) grant(bits api.Bits) {
	f.t.Helper()
	ctx := context.Background()
	role := id.New()
	if err := f.repo.PutRole(ctx, store.RoleRow{
		ID: role, CommunityID: f.cid, Name: "r", Position: f.position, Allow: uint64(bits), Created: policyT0,
	}); err != nil {
		f.t.Fatalf("PutRole: %v", err)
	}
	f.position++
	if err := f.repo.PutMemberRole(ctx, f.cid, f.user, role); err != nil {
		f.t.Fatalf("PutMemberRole: %v", err)
	}
}

// post stores one app message in g from device at created, as the delivery service would: the row,
// and for a class-0 upload into a community text group the user's slow-mode mark (fix round 1).
func (f *policyFixture) post(g store.GroupRow, device id.ID, created int64, class *uint8) {
	f.t.Helper()
	ctx := context.Background()
	f.seq++
	if err := f.repo.PutAppMessage(ctx, store.AppMessageRow{
		GroupID: g.GroupID, Seq: f.seq, Epoch: 1, UploaderDevice: device, Blob: []byte("ct"),
		FrankingTag: make([]byte, 32), Size: 2, Created: created, Class: class,
	}); err != nil {
		f.t.Fatalf("PutAppMessage: %v", err)
	}
	if class != nil && *class == 0 && g.Kind == api.GroupText && g.CommunityID != nil {
		if err := f.repo.PutSlowmodeMark(ctx, g.TargetID, f.user, created); err != nil {
			f.t.Fatalf("PutSlowmodeMark: %v", err)
		}
	}
}

func (f *policyFixture) check(g store.GroupRow, class uint8, now int64) error {
	return api.PostPolicy{Repo: f.repo, Resolver: api.NewResolver(f.repo)}.Check(context.Background(), g, f.user, class, now)
}

func wantPolicyNil(t *testing.T, label string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: Check = %v, want nil", label, err)
	}
}

func wantPolicyRefusal(t *testing.T, label string, err error, status int, code server.Code) *server.Error {
	t.Helper()
	var se *server.Error
	if !errors.As(err, &se) || se.Status() != status || se.Code != code {
		t.Fatalf("%s: Check = %v, want %d %s", label, err, status, code)
	}
	return se
}

func wantPolicySlowed(t *testing.T, label string, err error) {
	t.Helper()
	se := wantPolicyRefusal(t, label, err, http.StatusTooManyRequests, server.CodeRateLimited)
	if se.RetryAfterMS == nil || *se.RetryAfterMS != 5000 {
		t.Fatalf("%s: retry_after_ms = %v, want 5000", label, se.RetryAfterMS)
	}
	if se.Detail != "slowmode: 5 seconds left" {
		t.Fatalf("%s: detail = %q, want %q", label, se.Detail, "slowmode: 5 seconds left")
	}
}

const viewSend = api.PermViewChannel | api.PermSendMessages

func TestAPostWithoutSendIsForbidden(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, api.PermViewChannel, 0)
			wantPolicyRefusal(t, "no send, class 0", f.check(f.group, 0, policyT0), http.StatusForbidden, server.CodeForbidden)
			f.grant(api.PermSendMessages)
			wantPolicyNil(t, "a role with send, class 0", f.check(f.group, 0, policyT0))
		})
	}
}

func TestAFoldWithoutSendIsAccepted(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, api.PermViewChannel, 0)
			wantPolicyNil(t, "no send, class 1", f.check(f.group, 1, policyT0))
		})
	}
}

func TestSlowModeRefusesTheSecondPostWithItsWait(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, viewSend, 10)
			f.post(f.group, f.phone, policyT0, classOf(0))
			wantPolicySlowed(t, "class 0 at +5", f.check(f.group, 0, policyT0+5))
			wantPolicyNil(t, "class 0 at +10", f.check(f.group, 0, policyT0+10))
		})
	}
}

func TestBypassSlowmodeSkipsTheWait(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, viewSend, 10)
			f.post(f.group, f.phone, policyT0, classOf(0))
			f.grant(api.PermBypassSlowmode)
			wantPolicyNil(t, "bypass, class 0 at +5", f.check(f.group, 0, policyT0+5))
		})
	}
}

func TestFoldsAreNotSlowed(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, viewSend, 10)
			f.post(f.group, f.phone, policyT0, classOf(0))
			wantPolicyNil(t, "class 1 at +5", f.check(f.group, 1, policyT0+5))
		})
	}
}

func TestSlowModeCountsEveryDeviceOfTheUser(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, viewSend, 10)
			f.post(f.group, f.laptop, policyT0, classOf(0))
			// The next upload comes from the phone: the session's device does not matter, the user does.
			wantPolicySlowed(t, "class 0 at +5 after the other device's post", f.check(f.group, 0, policyT0+5))
			wantPolicyNil(t, "class 0 at +10", f.check(f.group, 0, policyT0+10))
		})
	}
}

func TestDirectMessagesAreUnchecked(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, api.PermViewChannel, 10)
			dm := f.newGroup(0, nil)
			f.post(dm, f.phone, policyT0-1, classOf(0))
			wantPolicyNil(t, "a DM group, class 0, a post 1 s earlier", f.check(dm, 0, policyT0))
		})
	}
}

func TestACallGroupIsUnchecked(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, api.PermViewChannel, 10)
			cid := f.cid
			call := f.newGroup(1, &cid)
			wantPolicyNil(t, "a call group, no send, class 0", f.check(call, 0, policyT0))
		})
	}
}

func TestAnAdministratorDoesNotWait(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, api.PermViewChannel, 10)
			f.grant(api.PermAdministrator)
			f.post(f.group, f.phone, policyT0, classOf(0))
			wantPolicyNil(t, "an administrator, class 0 at +5", f.check(f.group, 0, policyT0+5))
		})
	}
}

// web3.8 S1: the legacy row of web3.7 (a nil class) is gone; every upload names its class, and a
// class-0 upload is the one checked for send_messages. The control keeps the same inputs.
func TestAClassZeroUploadNeedsSend(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, api.PermViewChannel, 0)
			wantPolicyRefusal(t, "no send, class 0", f.check(f.group, 0, policyT0), http.StatusForbidden, server.CodeForbidden)
			f.grant(api.PermSendMessages)
			wantPolicyNil(t, "a role with send, class 0", f.check(f.group, 0, policyT0))
		})
	}
}

// slowReadRepo counts the slow-mode reads the policy makes.
type slowReadRepo struct {
	store.Repository
	reads atomic.Int32
}

func (r *slowReadRepo) LastClassZeroUploadAt(ctx context.Context, channelID, userID id.ID) (int64, error) {
	r.reads.Add(1)
	return r.Repository.LastClassZeroUploadAt(ctx, channelID, userID)
}

// web3.8 S1: the legacy row of web3.7 becomes class 1, which never reads the slow-mode mark. The
// control keeps the same inputs.
func TestAClassOneUploadIsNotSlowed(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, viewSend, 10)
			f.post(f.group, f.phone, policyT0, classOf(0))
			rec := &slowReadRepo{Repository: repo}
			p := api.PostPolicy{Repo: rec, Resolver: api.NewResolver(repo)}
			wantPolicyNil(t, "class 1 at +5", p.Check(context.Background(), f.group, f.user, 1, policyT0+5))
			if n := rec.reads.Load(); n != 0 {
				t.Fatalf("a class-1 upload read LastClassZeroUploadAt %d times, want 0", n)
			}
			// Control: the same wrapper sees the read of a class-0 upload, which is slowed.
			wantPolicySlowed(t, "class 0 at +5", p.Check(context.Background(), f.group, f.user, 0, policyT0+5))
			if n := rec.reads.Load(); n != 1 {
				t.Fatalf("a class-0 upload read LastClassZeroUploadAt %d times, want 1", n)
			}
		})
	}
}

// SB1 fix round 1 (review): the policy fails closed. A deleted channel's group that is still open
// (the window before its close, or a close that failed) takes no upload of any class: 403
// E_FORBIDDEN, never an allow. Control: before the delete, the same member with send is accepted.
func TestADeletedChannelsGroupIsRefused(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, viewSend, 0)
			wantPolicyNil(t, "control: class 0 before the delete", f.check(f.group, 0, policyT0))
			if err := repo.DeleteChannel(context.Background(), f.channel.ID, policyT0); err != nil {
				t.Fatalf("DeleteChannel: %v", err)
			}
			for _, c := range []struct {
				name  string
				class uint8
			}{{"class 0", 0}, {"class 1", 1}} {
				wantPolicyRefusal(t, c.name+" into a deleted channel's group", f.check(f.group, c.class, policyT0),
					http.StatusForbidden, server.CodeForbidden)
			}
		})
	}
}

// SB1 fix round 1 (review finding 1): the wait is read from the durable mark, so a post retention
// pruned still holds it. Control: at the end of the window the post is accepted.
func TestAPrunedPostKeepsItsSlowModeMark(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, viewSend, 3600)
			f.post(f.group, f.phone, policyT0, classOf(0))
			n, err := repo.PruneAppMessages(context.Background(), f.group.GroupID, f.seq, 0, policyT0+60)
			if err != nil || n != 1 {
				t.Fatalf("PruneAppMessages = %d, %v; want the post pruned", n, err)
			}
			if err := f.check(f.group, 0, policyT0+60); err == nil {
				t.Fatal("after the prune a post 60 s into a 3600 s slow mode is admitted")
			}
			wantPolicyNil(t, "control: at the end of the window", f.check(f.group, 0, policyT0+3600))
		})
	}
}

func TestManagedBotJoinAndSendGatesUseLiveInstallation(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, viewSend, 0)
			ctx := context.Background()
			botID, deviceID := id.New(), id.New()
			botUser := newAPITestUser(botID, "bot_"+botID.String()[:8], policyT0)
			botUser.Kind = 1
			if err := repo.CreateUser(ctx, botUser); err != nil {
				t.Fatalf("CreateUser bot: %v", err)
			}
			device := newAPITestDevice(deviceID, botID, policyT0)
			device.DSKPub = uniqueAPITestDSK(t)
			device.Tier, device.SignerTier = 0, 0
			if err := repo.CreateDevice(ctx, device); err != nil {
				t.Fatalf("CreateDevice bot: %v", err)
			}
			if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: f.cid, UserID: botID, Joined: policyT0}); err != nil {
				t.Fatalf("PutMember bot: %v", err)
			}
			runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: make([]byte, 32), Created: policyT0, Rotated: policyT0}
			runtime.TokenHash[0] = 1
			if err := repo.CreateBotRuntime(ctx, runtime); err != nil {
				t.Fatalf("CreateBotRuntime: %v", err)
			}
			uid, did, rid := botID, deviceID, runtime.ID
			verified := policyT0
			installation := store.BotInstallationRow{ID: id.New(), CommunityID: f.cid, RuntimeID: &rid,
				UserID: &uid, DeviceID: &did, State: store.BotInstallationJoining, Revision: 1,
				ApprovedBy: f.user, ApprovedAt: policyT0, CredentialVerifiedAt: &verified, Created: policyT0, Updated: policyT0}
			if err := repo.CreateBotInstallation(ctx, installation); err != nil {
				t.Fatalf("CreateBotInstallation: %v", err)
			}
			acl := api.ResolverACL{Repo: repo}
			// The Bot is a member of Bot channels' text groups only (Task 23, R1 b).
			if ok, err := acl.Eligible(ctx, f.group.GroupID, botID); err != nil || ok {
				t.Fatalf("joining bot ACL outside a Bot channel = %v, %v; want denied", ok, err)
			}
			if _, err := repo.MarkBotChannel(ctx, store.BotChannelRow{ChannelID: f.channel.ID, CommunityID: f.cid, MarkedAt: policyT0, MarkedBy: f.user}); err != nil {
				t.Fatalf("MarkBotChannel: %v", err)
			}
			if ok, err := acl.Eligible(ctx, f.group.GroupID, botID); err != nil || !ok {
				t.Fatalf("joining bot own-community ACL = %v, %v; want eligible", ok, err)
			}
			registration := api.StructureChannels{Repo: repo}
			foreignCommunity := store.CommunityRow{ID: id.New(), Owner: f.user, Name: "foreign",
				PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: policyT0}
			if err := repo.CreateCommunity(ctx, foreignCommunity); err != nil {
				t.Fatalf("CreateCommunity foreign: %v", err)
			}
			checkRegistration := func(phase string) {
				t.Helper()
				if err := registration.MayRegister(ctx, botID, binding(&f.cid, f.channel.ID, api.GroupText)); err != nil {
					t.Errorf("%s own-community text registration: %v", phase, err)
				}
				for _, b := range []ds.Binding{
					binding(&foreignCommunity.ID, f.channel.ID, api.GroupText),
					binding(nil, f.channel.ID, api.GroupText),
					binding(nil, id.New(), 3),
				} {
					if err := registration.MayRegister(ctx, botID, b); !errors.Is(err, ds.ErrNotEligible) {
						t.Errorf("%s foreign/DM/interaction registration = %v, want ds.ErrNotEligible", phase, err)
					}
				}
			}
			checkRegistration("joining")
			policy := api.PostPolicy{Repo: repo, Resolver: api.NewResolver(repo)}
			for _, class := range []uint8{0, 1} {
				if err := policy.Check(ctx, f.group, botID, class, policyT0); err == nil {
					t.Fatalf("joining bot class-%d upload was admitted", class)
				}
			}
			dm := f.newGroup(0, nil)
			if ok, err := acl.Eligible(ctx, dm.GroupID, botID); err != nil || ok {
				t.Fatalf("bot DM ACL = %v, %v; want denied", ok, err)
			}
			if err := policy.Check(ctx, dm, botID, 0, policyT0); err == nil {
				t.Fatal("bot DM upload was admitted")
			}
			installation.State = store.BotInstallationReady // ready after final credential, list and package gates
			installation.Revision++
			installation.Updated++
			if err := repo.UpdateBotInstallation(ctx, installation.ID, installation.Revision-1, installation); err != nil {
				t.Fatalf("UpdateBotInstallation ready: %v", err)
			}
			checkRegistration("ready")
			if err := policy.Check(ctx, f.group, botID, 0, policyT0); err != nil {
				t.Fatalf("ready bot own-community class-0 upload: %v", err)
			}
		})
	}
}

func TestMemberTimeoutBlocksClassOneTextUploadsUntilExpiry(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, viewSend, 0)
			until := policyT0 + 60
			if err := repo.PutMemberTimeout(context.Background(), store.MemberTimeoutRow{CommunityID: f.cid, UserID: f.user, ByUser: f.user, Created: policyT0, Expires: until}); err != nil {
				t.Fatal(err)
			}
			for _, class := range []uint8{0, 1} {
				wantPolicyRefusal(t, "timed out class upload", f.check(f.group, class, policyT0+1), http.StatusForbidden, server.CodeForbidden)
			}
			wantPolicyNil(t, "timeout expiry is automatic", f.check(f.group, 0, until))
		})
	}
}
