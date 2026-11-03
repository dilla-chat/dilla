package store_test

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

func TestRoleGrantMigrationBackfillsLegacyAssignmentsAsManual(t *testing.T) {
	db := openSQLite(t)
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sqlitemigrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := p.UpTo(ctx, 28); err != nil {
		t.Fatalf("migrate through 28: %v", err)
	}
	cid, user, role := id.New(), id.New(), id.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id,username,display,kind,umk_pub,ssk_pub,sig_umk_ssk,created) VALUES(?,?,?,?,?,?,?,?)`,
		user, "legacy", "Legacy", 0, make([]byte, 32), make([]byte, 32), make([]byte, 64), 1); err != nil {
		t.Fatalf("insert legacy user: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO communities(id,owner,name,policy_json,created) VALUES(?,?,?,?,?)`, cid, user, "legacy", `{}`, 1); err != nil {
		t.Fatalf("insert community: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO members(community_id,user_id,joined) VALUES(?,?,?)`, cid, user, 1); err != nil {
		t.Fatalf("insert member: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO roles(id,community_id,name,created) VALUES(?,?,?,?)`, role, cid, "legacy", 1); err != nil {
		t.Fatalf("insert role: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO member_roles(community_id,user_id,role_id) VALUES(?,?,?)`, cid, user, role); err != nil {
		t.Fatalf("insert legacy assignment: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("finish migrations: %v", err)
	}
	var source string
	if err := db.QueryRowContext(ctx, `SELECT source FROM member_role_grant_sources WHERE community_id=? AND user_id=? AND role_id=?`, cid, user, role).Scan(&source); err != nil {
		t.Fatalf("read backfilled source: %v", err)
	}
	if source != string(store.RoleGrantManual) {
		t.Fatalf("backfilled source = %q, want manual", source)
	}
}

func TestRoleGrantSourcesPreserveEffectiveRoleUntilLastSourceIsRemoved(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			user := seedUser(ctx, t, repo).ID
			if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: user, Joined: 1}); err != nil {
				t.Fatal(err)
			}
			role := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "member", Created: 1}
			if err := repo.PutRole(ctx, role); err != nil {
				t.Fatal(err)
			}

			if err := repo.AddMemberRoleGrant(ctx, cid, user, role.ID, store.RoleGrantSelf); err != nil {
				t.Fatalf("add self grant: %v", err)
			}
			if users, err := repo.ListSelfRoleGrantUsers(ctx, cid, role.ID); err != nil || len(users) != 1 || users[0] != user {
				t.Fatalf("self grant users = %v, %v; want only member", users, err)
			}
			if has, err := repo.HasMemberRoleGrant(ctx, cid, user, role.ID, store.RoleGrantSelf); err != nil || !has {
				t.Fatalf("self source exists = %v, %v; want true", has, err)
			}
			if has, err := repo.HasMemberRoleGrant(ctx, cid, user, role.ID, store.RoleGrantManual); err != nil || has {
				t.Fatalf("manual source exists = %v, %v; want false", has, err)
			}
			if err := repo.PutMemberRole(ctx, cid, user, role.ID); err != nil {
				t.Fatalf("add manual grant: %v", err)
			}
			if err := repo.RemoveMemberRoleGrant(ctx, cid, user, role.ID, store.RoleGrantSelf); err != nil {
				t.Fatalf("remove self grant: %v", err)
			}
			if users, err := repo.ListSelfRoleGrantUsers(ctx, cid, role.ID); err != nil || len(users) != 0 {
				t.Fatalf("self grant users after removal = %v, %v; want none", users, err)
			}
			if has, err := repo.HasMemberRoleGrant(ctx, cid, user, role.ID, store.RoleGrantSelf); err != nil || has {
				t.Fatalf("self source after removal = %v, %v; want false", has, err)
			}
			held, err := repo.ListMemberRoles(ctx, cid, user)
			if err != nil || !slices.Contains(held, role.ID) {
				t.Fatalf("role after self removal = %v, %v; want manual role retained", held, err)
			}

			if err := repo.DeleteMemberRole(ctx, cid, user, role.ID); err != nil {
				t.Fatalf("manual revoke: %v", err)
			}
			held, err = repo.ListMemberRoles(ctx, cid, user)
			if err != nil || slices.Contains(held, role.ID) {
				t.Fatalf("role after manual revoke = %v, %v; want absent", held, err)
			}
			if err := repo.RemoveMemberRoleGrant(ctx, cid, user, role.ID, store.RoleGrantSelf); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("remove absent self grant = %v, want ErrNotFound", err)
			}

			if err := repo.PutMemberRole(ctx, cid, user, role.ID); err != nil {
				t.Fatalf("add manual grant again: %v", err)
			}
			if err := repo.AddMemberRoleGrant(ctx, cid, user, role.ID, store.RoleGrantSelf); err != nil {
				t.Fatalf("add self grant again: %v", err)
			}
			if err := repo.DeleteMemberRole(ctx, cid, user, role.ID); err != nil {
				t.Fatalf("manual revoke with self grant present: %v", err)
			}
			held, err = repo.ListMemberRoles(ctx, cid, user)
			if err != nil || !slices.Contains(held, role.ID) {
				t.Fatalf("role after manual revoke = %v, %v; want self grant retained", held, err)
			}
			if err := repo.RemoveMemberRoleGrant(ctx, cid, user, role.ID, store.RoleGrantSelf); err != nil {
				t.Fatalf("remove remaining self grant: %v", err)
			}
			held, err = repo.ListMemberRoles(ctx, cid, user)
			if err != nil || slices.Contains(held, role.ID) {
				t.Fatalf("role after final source removal = %v, %v; want absent", held, err)
			}

			if err := repo.PutMemberRole(ctx, cid, user, role.ID); err != nil {
				t.Fatalf("add manual grant for revoke-all: %v", err)
			}
			if err := repo.AddMemberRoleGrant(ctx, cid, user, role.ID, store.RoleGrantSelf); err != nil {
				t.Fatalf("add self grant for revoke-all: %v", err)
			}
			if err := repo.AddMemberRoleGrant(ctx, cid, user, role.ID, store.RoleGrantReaction); err != nil {
				t.Fatalf("add reaction grant for revoke-all: %v", err)
			}
			if err := repo.RevokeMemberRole(ctx, cid, user, role.ID); err != nil {
				t.Fatalf("revoke all sources: %v", err)
			}
			held, err = repo.ListMemberRoles(ctx, cid, user)
			if err != nil || slices.Contains(held, role.ID) {
				t.Fatalf("role after revoke all sources = %v, %v; want absent", held, err)
			}
			for _, source := range []store.RoleGrantSource{store.RoleGrantManual, store.RoleGrantSelf, store.RoleGrantReaction} {
				if err := repo.RemoveMemberRoleGrant(ctx, cid, user, role.ID, source); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("remove %s after revoke all = %v, want ErrNotFound", source, err)
				}
			}
		})
	}
}

func TestSelfRolePolicyIsAtomicPersistentAndCommunityScoped(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			approver := seedUser(ctx, t, repo).ID
			first := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "first", Created: 1}
			second := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "second", Created: 1}
			if err := repo.PutRole(ctx, first); err != nil {
				t.Fatal(err)
			}
			if err := repo.PutRole(ctx, second); err != nil {
				t.Fatal(err)
			}
			if err := repo.SetSelfRolePolicy(ctx, cid, approver, []id.ID{first.ID}, 10); err == nil {
				t.Fatal("policy write outside transaction succeeded")
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.LockCommunity(ctx, cid); err != nil {
					return err
				}
				return tx.SetSelfRolePolicy(ctx, cid, approver, []id.ID{second.ID, first.ID}, 11)
			}); err != nil {
				t.Fatalf("write policy: %v", err)
			}
			rows, err := repo.ListSelfRolePolicy(ctx, cid)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 || rows[0].ApprovedBy != approver || rows[0].ApprovedAt != 11 || rows[1].ApprovedAt != 11 {
				t.Fatalf("policy rows = %+v", rows)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.LockCommunity(ctx, cid); err != nil {
					return err
				}
				return tx.SetSelfRolePolicy(ctx, cid, approver, []id.ID{first.ID}, 12)
			}); err != nil {
				t.Fatalf("replace policy: %v", err)
			}
			rows, err = repo.ListSelfRolePolicy(ctx, cid)
			if err != nil || len(rows) != 1 || rows[0].RoleID != first.ID || rows[0].ApprovedAt != 12 {
				t.Fatalf("replaced policy = %+v, %v", rows, err)
			}
		})
	}
}

func TestRolePanelsArePersistentAndReplaceEmojiMappingsAtomically(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			bot := seedUser(ctx, t, repo).ID
			roleA := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "alpha", Created: 1}
			roleB := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "beta", Created: 1}
			if err := repo.PutRole(ctx, roleA); err != nil {
				t.Fatal(err)
			}
			if err := repo.PutRole(ctx, roleB); err != nil {
				t.Fatal(err)
			}
			installation := id.New()
			if err := repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installation, CommunityID: cid, State: store.BotInstallationPending, Revision: 1, ApprovedBy: bot, ApprovedAt: 1, Created: 1, Updated: 1}); err != nil {
				t.Fatal(err)
			}
			channel := channelIn(cid, 0, 1, 2, "roles", 1)
			if err := repo.CreateChannel(ctx, channel); err != nil {
				t.Fatal(err)
			}
			panel := store.RolePanelRow{CommunityID: cid, ChannelID: channel.ID, MessageSeq: 17, AuthorID: bot, InstallationID: installation, SendOperationID: id.New()}
			// The API returns role-panel options in bytewise emoji order, independent
			// of insertion order and database locale.
			options := []store.RolePanelOptionRow{{Emoji: "🟦", RoleID: roleB.ID}, {Emoji: "✅", RoleID: roleA.ID}}
			if err := repo.SetRolePanel(ctx, panel, options); err == nil {
				t.Fatal("panel write outside transaction succeeded")
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.SetRolePanel(ctx, panel, options) }); err != nil {
				t.Fatalf("set panel: %v", err)
			}
			got, err := repo.GetRolePanel(ctx, panel.ChannelID, panel.MessageSeq)
			if err != nil || got != panel {
				t.Fatalf("panel = %+v, %v; want %+v", got, err, panel)
			}
			gotOptions, err := repo.ListRolePanelOptions(ctx, panel.ChannelID, panel.MessageSeq)
			if err != nil || len(gotOptions) != 2 || gotOptions[0].Emoji != "✅" || gotOptions[0].RoleID != roleA.ID || gotOptions[1].Emoji != "🟦" || gotOptions[1].RoleID != roleB.ID {
				t.Fatalf("options = %+v, %v", gotOptions, err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.SetRolePanel(ctx, panel, options[1:2]) }); err != nil {
				t.Fatalf("replace panel: %v", err)
			}
			gotOptions, err = repo.ListRolePanelOptions(ctx, panel.ChannelID, panel.MessageSeq)
			if err != nil || len(gotOptions) != 1 || gotOptions[0].RoleID != roleA.ID {
				t.Fatalf("replaced options = %+v, %v", gotOptions, err)
			}
			lookalike := panel
			lookalike.AuthorID = seedUser(ctx, t, repo).ID
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.SetRolePanel(ctx, lookalike, options) }); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("re-register message under another author = %v, want ErrConflict", err)
			}
			tooLong := options[:1]
			tooLong[0].Emoji = "🟦🟦🟦🟦🟦🟦🟦🟦🟦"
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.SetRolePanel(ctx, panel, tooLong) }); err == nil {
				t.Fatal("emoji mapping longer than 32 UTF-8 bytes was accepted")
			}
			tooLargeSeq := uint64(math.MaxInt64) + 1
			tooLargePanel := panel
			tooLargePanel.MessageSeq = tooLargeSeq
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.SetRolePanel(ctx, tooLargePanel, options) }); err == nil {
				t.Fatal("panel sequence above MaxInt64 was accepted")
			}
			if _, err := repo.GetRolePanel(ctx, panel.ChannelID, tooLargeSeq); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("get panel above MaxInt64 = %v, want ErrNotFound", err)
			}
			if _, err := repo.ListRolePanelOptions(ctx, panel.ChannelID, tooLargeSeq); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("list panel options above MaxInt64 = %v, want ErrNotFound", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.DeleteRolePanel(ctx, panel.ChannelID, tooLargeSeq) }); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("delete panel above MaxInt64 = %v, want ErrNotFound", err)
			}
			if got, err := repo.GetRolePanel(ctx, panel.ChannelID, panel.MessageSeq); err != nil || got != panel {
				t.Fatalf("panel after oversized delete = %+v, %v; want original panel", got, err)
			}
			maxPanel := panel
			maxPanel.MessageSeq = uint64(math.MaxInt64)
			maxPanel.SendOperationID = id.New()
			maxOptions := []store.RolePanelOptionRow{{Emoji: "✅", RoleID: roleA.ID}}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.SetRolePanel(ctx, maxPanel, maxOptions) }); err != nil {
				t.Fatalf("set panel at MaxInt64: %v", err)
			}
			if got, err := repo.GetRolePanel(ctx, panel.ChannelID, maxPanel.MessageSeq); err != nil || got != maxPanel {
				t.Fatalf("panel at MaxInt64 = %+v, %v; want %+v", got, err, maxPanel)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.DeleteRolePanel(ctx, panel.ChannelID, maxPanel.MessageSeq) }); err != nil {
				t.Fatalf("delete panel at MaxInt64: %v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.DeleteRolePanel(ctx, panel.ChannelID, panel.MessageSeq) }); err != nil {
				t.Fatalf("delete role panel: %v", err)
			}
			if _, err := repo.GetRolePanel(ctx, panel.ChannelID, panel.MessageSeq); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("panel after delete = %v, want ErrNotFound", err)
			}
			if got, err := repo.ListRolePanelOptions(ctx, panel.ChannelID, panel.MessageSeq); err != nil || len(got) != 0 {
				t.Fatalf("options after delete = %+v, %v; want empty", got, err)
			}
		})
	}
}
