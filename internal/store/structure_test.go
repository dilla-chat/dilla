package store_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// The plan names the helpers newRepos/newTestUser; Plan 1 shipped them as
// engines(t) (one migrated repository per available engine, keyed by engine
// name) and seedUser, and the shipped spelling wins.

func TestCommunityRoundTrip(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			owner := seedUser(ctx, t, repo).ID

			cid := id.New()
			in := store.CommunityRow{
				ID:                   cid,
				Owner:                owner,
				Name:                 "Kryptering",
				IconBlob:             nil,
				PolicyJSON:           []byte(`{"retention_days":0}`),
				PolicyVersion:        1,
				MinAccountAgeSeconds: 0,
				RequireMod2FA:        0,
				Created:              1000,
			}
			if err := repo.CreateCommunity(ctx, in); err != nil {
				t.Fatalf("CreateCommunity: %v", err)
			}

			got, err := repo.GetCommunity(ctx, cid)
			if err != nil {
				t.Fatalf("GetCommunity: %v", err)
			}
			if got.Name != "Kryptering" || got.Owner != owner || got.PolicyVersion != 1 {
				t.Fatalf("GetCommunity = %+v", got)
			}
			if string(got.PolicyJSON) != `{"retention_days":0}` {
				t.Fatalf("PolicyJSON = %q", got.PolicyJSON)
			}

			if err := repo.UpdateCommunityPolicy(ctx, cid, []byte(`{"retention_days":30}`), 2); err != nil {
				t.Fatalf("UpdateCommunityPolicy: %v", err)
			}
			got, err = repo.GetCommunity(ctx, cid)
			if err != nil {
				t.Fatalf("GetCommunity after update: %v", err)
			}
			if got.PolicyVersion != 2 || string(got.PolicyJSON) != `{"retention_days":30}` {
				t.Fatalf("after update = %+v", got)
			}

			if _, err := repo.GetCommunity(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetCommunity(unknown) = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestCommunityMembers(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := id.New()
			owner := seedUser(ctx, t, repo).ID
			if err := repo.CreateCommunity(ctx, store.CommunityRow{
				ID: cid, Owner: owner, Name: "c", PolicyJSON: []byte(`{}`),
				PolicyVersion: 1, Created: 1,
			}); err != nil {
				t.Fatalf("CreateCommunity: %v", err)
			}

			users := make([]id.ID, 3)
			for i := range users {
				users[i] = seedUser(ctx, t, repo).ID
				if err := repo.PutMember(ctx, store.MemberOfCommunityRow{
					CommunityID: cid, UserID: users[i], Joined: int64(100 + i), Nick: "",
				}); err != nil {
					t.Fatalf("PutMember: %v", err)
				}
			}

			// PutMember is an upsert: a second call updates the nick and adds no row.
			if err := repo.PutMember(ctx, store.MemberOfCommunityRow{
				CommunityID: cid, UserID: users[0], Joined: 100, Nick: "nick",
			}); err != nil {
				t.Fatalf("PutMember upsert: %v", err)
			}

			var all []store.MemberOfCommunityRow
			var after id.ID
			for {
				page, err := repo.ListMembersOfCommunity(ctx, cid, after, 2)
				if err != nil {
					t.Fatalf("ListMembersOfCommunity: %v", err)
				}
				if len(page) == 0 {
					break
				}
				all = append(all, page...)
				after = page[len(page)-1].UserID
			}
			if len(all) != 3 {
				t.Fatalf("members = %d, want 3", len(all))
			}
			for i := 1; i < len(all); i++ {
				if string(all[i-1].UserID[:]) >= string(all[i].UserID[:]) {
					t.Fatalf("members not ordered by user_id: %x then %x", all[i-1].UserID, all[i].UserID)
				}
			}

			if err := repo.DeleteMember(ctx, cid, users[1]); err != nil {
				t.Fatalf("DeleteMember: %v", err)
			}
			rest, err := repo.ListMembersOfCommunity(ctx, cid, id.ID{}, 10)
			if err != nil {
				t.Fatalf("ListMembersOfCommunity: %v", err)
			}
			if len(rest) != 2 {
				t.Fatalf("after delete = %d, want 2", len(rest))
			}
		})
	}
}

func TestRolesPerCommunity(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := id.New()
			owner := seedUser(ctx, t, repo).ID
			if err := repo.CreateCommunity(ctx, store.CommunityRow{
				ID: cid, Owner: owner, Name: "c", PolicyJSON: []byte(`{}`),
				PolicyVersion: 1, Created: 1,
			}); err != nil {
				t.Fatalf("CreateCommunity: %v", err)
			}
			everyone := store.RoleRow{
				ID: id.New(), CommunityID: cid, Name: "@everyone",
				Position: 0, Allow: 0x401c3, Deny: 0, Created: 1,
			}
			mod := store.RoleRow{
				ID: id.New(), CommunityID: cid, Name: "mod",
				Position: 10, Allow: 0x4, Deny: 0, Hoist: 1, Created: 2,
			}
			for _, r := range []store.RoleRow{mod, everyone} {
				if err := repo.PutRole(ctx, r); err != nil {
					t.Fatalf("PutRole: %v", err)
				}
			}
			got, err := repo.ListRoles(ctx, cid)
			if err != nil {
				t.Fatalf("ListRoles: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("ListRoles = %d rows, want 2", len(got))
			}
			if got[0].Position != 0 || got[1].Position != 10 {
				t.Fatalf("ListRoles is not ordered by position: %+v", got)
			}
			if got[0].Allow != 0x401c3 {
				t.Fatalf("@everyone allow = %#x", got[0].Allow)
			}
		})
	}
}

// TestCommunityMetaMembershipAndRoleGrants covers the methods the plan adds to
// §4.1 (P2-D7, P2-D7b, and P2-D9's GetRole, which task 1's grant route needs),
// and the policy-version guard: two writers that read the same version must
// not both land a policy under the same number.
func TestCommunityMetaMembershipAndRoleGrants(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := id.New()
			owner := seedUser(ctx, t, repo).ID
			if err := repo.CreateCommunity(ctx, store.CommunityRow{
				ID: cid, Owner: owner, Name: "c", PolicyJSON: []byte(`{}`),
				PolicyVersion: 1, Created: 1,
			}); err != nil {
				t.Fatalf("CreateCommunity: %v", err)
			}

			// UpdateCommunityMeta writes the three mutable columns.
			if err := repo.UpdateCommunityMeta(ctx, cid, "renamed", 3600, 1); err != nil {
				t.Fatalf("UpdateCommunityMeta: %v", err)
			}
			got, err := repo.GetCommunity(ctx, cid)
			if err != nil {
				t.Fatalf("GetCommunity: %v", err)
			}
			if got.Name != "renamed" || got.MinAccountAgeSeconds != 3600 || got.RequireMod2FA != 1 {
				t.Fatalf("after UpdateCommunityMeta = %+v", got)
			}
			if err := repo.UpdateCommunityMeta(ctx, id.New(), "x", 0, 0); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("UpdateCommunityMeta(unknown) = %v, want ErrNotFound", err)
			}

			// The policy version is monotone: a writer that lost the race to the
			// same version is refused with ErrConflict, not silently accepted.
			if err := repo.UpdateCommunityPolicy(ctx, cid, []byte(`{"retention_days":7}`), 2); err != nil {
				t.Fatalf("UpdateCommunityPolicy v2: %v", err)
			}
			if err := repo.UpdateCommunityPolicy(ctx, cid, []byte(`{"retention_days":9}`), 2); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("UpdateCommunityPolicy stale v2 = %v, want ErrConflict", err)
			}
			if err := repo.UpdateCommunityPolicy(ctx, id.New(), []byte(`{}`), 2); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("UpdateCommunityPolicy(unknown) = %v, want ErrNotFound", err)
			}
			got, _ = repo.GetCommunity(ctx, cid)
			if string(got.PolicyJSON) != `{"retention_days":7}` || got.PolicyVersion != 2 {
				t.Fatalf("the stale write landed: %+v", got)
			}

			// GetMember, and ErrNotFound for a non-member.
			member := seedUser(ctx, t, repo).ID
			if err := repo.PutMember(ctx, store.MemberOfCommunityRow{
				CommunityID: cid, UserID: member, Joined: 5, Nick: "Åsa",
			}); err != nil {
				t.Fatalf("PutMember: %v", err)
			}
			m, err := repo.GetMember(ctx, cid, member)
			if err != nil {
				t.Fatalf("GetMember: %v", err)
			}
			if m.Joined != 5 || m.Nick != "Åsa" {
				t.Fatalf("GetMember = %+v", m)
			}
			if _, err := repo.GetMember(ctx, cid, id.New()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetMember(non-member) = %v, want ErrNotFound", err)
			}

			// Role grants: GetRole, PutMemberRole (idempotent), ListMemberRoles,
			// DeleteMemberRole.
			role := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "mod", Position: 3, Allow: 0x4, Created: 7}
			if err := repo.PutRole(ctx, role); err != nil {
				t.Fatalf("PutRole: %v", err)
			}
			gotRole, err := repo.GetRole(ctx, role.ID)
			if err != nil {
				t.Fatalf("GetRole: %v", err)
			}
			if gotRole != role {
				t.Fatalf("GetRole = %+v, want %+v", gotRole, role)
			}
			if _, err := repo.GetRole(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetRole(unknown) = %v, want ErrNotFound", err)
			}
			for range 2 {
				if err := repo.PutMemberRole(ctx, cid, member, role.ID); err != nil {
					t.Fatalf("PutMemberRole: %v", err)
				}
			}
			held, err := repo.ListMemberRoles(ctx, cid, member)
			if err != nil {
				t.Fatalf("ListMemberRoles: %v", err)
			}
			if len(held) != 1 || held[0] != role.ID {
				t.Fatalf("ListMemberRoles = %v, want [%v]", held, role.ID)
			}
			if err := repo.DeleteMemberRole(ctx, cid, member, role.ID); err != nil {
				t.Fatalf("DeleteMemberRole: %v", err)
			}
			if err := repo.DeleteMemberRole(ctx, cid, member, role.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("DeleteMemberRole twice = %v, want ErrNotFound", err)
			}

			// Leaving drops the member's grants with the membership row, so a
			// re-join does not resurrect a role the member held before.
			if err := repo.PutMemberRole(ctx, cid, member, role.ID); err != nil {
				t.Fatalf("PutMemberRole: %v", err)
			}
			if err := repo.DeleteMember(ctx, cid, member); err != nil {
				t.Fatalf("DeleteMember: %v", err)
			}
			if err := repo.DeleteMember(ctx, cid, member); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("DeleteMember twice = %v, want ErrNotFound", err)
			}
			held, err = repo.ListMemberRoles(ctx, cid, member)
			if err != nil {
				t.Fatalf("ListMemberRoles after leave: %v", err)
			}
			if len(held) != 0 {
				t.Fatalf("grants survived the membership: %v", held)
			}

			// SoftDeleteCommunity tombstones: the community is gone to every read
			// and a second delete is ErrNotFound.
			if err := repo.SoftDeleteCommunity(ctx, cid, 99); err != nil {
				t.Fatalf("SoftDeleteCommunity: %v", err)
			}
			if _, err := repo.GetCommunity(ctx, cid); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetCommunity after delete = %v, want ErrNotFound", err)
			}
			if err := repo.SoftDeleteCommunity(ctx, cid, 100); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("SoftDeleteCommunity twice = %v, want ErrNotFound", err)
			}
		})
	}
}

// SetCommunityOwner writes the owner of a live community; on a tombstoned one it is a no-op that
// neither fails nor brings the community back.
func TestSetCommunityOwnerRoundTrip(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			owner := seedUser(ctx, t, repo).ID
			heir := seedUser(ctx, t, repo).ID
			live, gone := id.New(), id.New()
			for _, cid := range []id.ID{live, gone} {
				if err := repo.CreateCommunity(ctx, store.CommunityRow{
					ID: cid, Owner: owner, Name: "c", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: 10,
				}); err != nil {
					t.Fatalf("CreateCommunity: %v", err)
				}
			}
			if err := repo.SetCommunityOwner(ctx, live, heir); err != nil {
				t.Fatalf("SetCommunityOwner: %v", err)
			}
			got, err := repo.GetCommunity(ctx, live)
			if err != nil || got.Owner != heir {
				t.Fatalf("owner after SetCommunityOwner = %v, %v, want %v", got.Owner, err, heir)
			}
			if got.Name != "c" || got.PolicyVersion != 1 {
				t.Fatalf("SetCommunityOwner changed another column: %+v", got)
			}
			if err := repo.SoftDeleteCommunity(ctx, gone, 99); err != nil {
				t.Fatalf("SoftDeleteCommunity: %v", err)
			}
			if err := repo.SetCommunityOwner(ctx, gone, heir); err != nil {
				t.Fatalf("SetCommunityOwner on a tombstone = %v, want a no-op", err)
			}
			if _, err := repo.GetCommunity(ctx, gone); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetCommunity after SetCommunityOwner on a tombstone = %v, want ErrNotFound", err)
			}
		})
	}
}

// ListCommunities is what `dillad admin community list` pages through: the live
// communities in id order, a soft-deleted one left out, and the cursor and limit
// honoured the way ListUsers honours them.
func TestListCommunitiesPagesLiveCommunitiesInIDOrder(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			owner := seedUser(ctx, t, repo).ID
			var ids []id.ID
			for i := range 4 {
				cid := id.New()
				ids = append(ids, cid)
				if err := repo.CreateCommunity(ctx, store.CommunityRow{
					ID: cid, Owner: owner, Name: "c", PolicyJSON: []byte(`{}`),
					PolicyVersion: 1, Created: int64(10 + i),
				}); err != nil {
					t.Fatalf("CreateCommunity: %v", err)
				}
			}
			if err := repo.SoftDeleteCommunity(ctx, ids[0], 99); err != nil {
				t.Fatalf("SoftDeleteCommunity: %v", err)
			}
			sorted := slices.SortedFunc(slices.Values(ids[1:]), func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })

			all, err := repo.ListCommunities(ctx, id.ID{}, 100)
			if err != nil {
				t.Fatalf("ListCommunities: %v", err)
			}
			var got []id.ID
			for _, c := range all {
				if c.ID == ids[0] {
					t.Fatalf("the soft-deleted community %s was listed", ids[0])
				}
				got = append(got, c.ID)
			}
			if !slices.Equal(got, sorted) {
				t.Fatalf("ListCommunities = %v, want the live ones in id order %v", got, sorted)
			}
			if all[0].Owner != owner || all[0].PolicyVersion != 1 {
				t.Fatalf("the listed row lost its columns: %+v", all[0])
			}

			page, err := repo.ListCommunities(ctx, sorted[0], 1)
			if err != nil {
				t.Fatalf("ListCommunities after a cursor: %v", err)
			}
			if len(page) != 1 || page[0].ID != sorted[1] {
				t.Fatalf("page after %s = %v, want exactly %s", sorted[0], page, sorted[1])
			}
		})
	}
}

// listCommunitiesForUserSQLite is the statement of `-- name: ListCommunitiesForUser` in
// sqlite/queries/structure.sql, without its semicolon. The index test asserts that the file still
// contains it, so the plan checked below is the plan of the query the adapter runs.
const listCommunitiesForUserSQLite = `SELECT c.id, c.owner, c.name, c.icon_blob, c.policy_json, c.policy_version,
       c.min_account_age_seconds, c.require_mod_2fa, c.created, c.deleted_at
FROM members m
JOIN communities c ON c.id = m.community_id
WHERE m.user_id = ? AND c.deleted_at IS NULL
ORDER BY c.id`

// dilla-web-1 task 8 (L-SQL-02): GET /v1/communities reads the live communities a user is a member
// of, in id order, and nothing else: not a soft-deleted one, not one of someone else, not one the
// user left.
func TestListCommunitiesForUserListsOnlyLiveMemberships(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			owner := seedUser(ctx, t, repo).ID
			alice := seedUser(ctx, t, repo).ID
			stranger := seedUser(ctx, t, repo).ID
			mk := func(name string, created int64) id.ID {
				t.Helper()
				cid := id.New()
				if err := repo.CreateCommunity(ctx, store.CommunityRow{
					ID: cid, Owner: owner, Name: name, PolicyJSON: []byte(`{"join":"open"}`),
					PolicyVersion: 3, Created: created,
				}); err != nil {
					t.Fatalf("CreateCommunity %s: %v", name, err)
				}
				return cid
			}
			join := func(cid, uid id.ID) {
				t.Helper()
				if err := repo.PutMember(ctx, store.MemberOfCommunityRow{
					CommunityID: cid, UserID: uid, Joined: 100, Nick: "",
				}); err != nil {
					t.Fatalf("PutMember: %v", err)
				}
			}
			alpha, beta, gone, other := mk("alpha", 10), mk("beta", 11), mk("gamma", 12), mk("delta", 13)
			for _, cid := range []id.ID{alpha, beta, gone} {
				join(cid, alice)
			}
			join(other, owner)
			if err := repo.SoftDeleteCommunity(ctx, gone, 99); err != nil {
				t.Fatalf("SoftDeleteCommunity: %v", err)
			}

			got, err := repo.ListCommunitiesForUser(ctx, alice)
			if err != nil {
				t.Fatalf("ListCommunitiesForUser(alice): %v", err)
			}
			var ids []id.ID
			for _, c := range got {
				if c.ID == gone {
					t.Fatalf("the soft-deleted community %s was listed", gone)
				}
				if c.ID == other {
					t.Fatalf("community %s, which alice is not a member of, was listed", other)
				}
				ids = append(ids, c.ID)
			}
			want := slices.SortedFunc(slices.Values([]id.ID{alpha, beta}), func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
			if !slices.Equal(ids, want) {
				t.Fatalf("ListCommunitiesForUser(alice) = %v, want her two live communities in id order %v", ids, want)
			}
			for _, c := range got {
				if c.ID != alpha {
					continue
				}
				if c.Name != "alpha" || c.Owner != owner || c.PolicyVersion != 3 ||
					string(c.PolicyJSON) != `{"join":"open"}` || c.Created != 10 || c.DeletedAt != nil {
					t.Fatalf("the listed row lost its columns: %+v", c)
				}
			}

			for name, uid := range map[string]id.ID{"stranger": stranger, "unknown": id.New()} {
				rows, err := repo.ListCommunitiesForUser(ctx, uid)
				if err != nil {
					t.Fatalf("ListCommunitiesForUser(%s) = %v, want an empty list and no error", name, err)
				}
				if len(rows) != 0 {
					t.Fatalf("ListCommunitiesForUser(%s) = %+v, want nothing", name, rows)
				}
			}

			if err := repo.DeleteMember(ctx, alpha, alice); err != nil {
				t.Fatalf("DeleteMember: %v", err)
			}
			got, err = repo.ListCommunitiesForUser(ctx, alice)
			if err != nil {
				t.Fatalf("ListCommunitiesForUser after leaving: %v", err)
			}
			if len(got) != 1 || got[0].ID != beta {
				t.Fatalf("after leaving alpha alice lists %+v, want only %s", got, beta)
			}
		})
	}
}

// L-SQL-01: members_by_user exists on both engines with (user_id, community_id), and on SQLite the
// query ListCommunitiesForUser runs searches it by user_id instead of scanning every membership.
func TestMembersByUserIndexServesListCommunitiesForUser(t *testing.T) {
	queries, err := os.ReadFile(filepath.Join("sqlite", "queries", "structure.sql"))
	if err != nil {
		t.Fatalf("read the SQLite queries: %v", err)
	}
	if !strings.Contains(string(queries), listCommunitiesForUserSQLite) {
		t.Fatal("sqlite/queries/structure.sql no longer contains the ListCommunitiesForUser statement this test explains")
	}
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			db := rawDB(t, repo)
			switch engine {
			case "sqlite":
				var ddl string
				if err := db.QueryRowContext(ctx,
					`SELECT sql FROM sqlite_schema WHERE type = 'index' AND name = 'members_by_user'`).Scan(&ddl); err != nil {
					t.Fatalf("read members_by_user: %v", err)
				}
				if ddl != "CREATE INDEX members_by_user ON members (user_id, community_id)" {
					t.Fatalf("members_by_user is %q", ddl)
				}
				rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+listCommunitiesForUserSQLite, make([]byte, 16))
				if err != nil {
					t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
				}
				defer rows.Close()
				var plan []string
				for rows.Next() {
					var node, parent, unused int64
					var detail string
					if err := rows.Scan(&node, &parent, &unused, &detail); err != nil {
						t.Fatalf("scan plan: %v", err)
					}
					plan = append(plan, detail)
				}
				if err := rows.Err(); err != nil {
					t.Fatalf("iterate plan: %v", err)
				}
				if !slices.ContainsFunc(plan, func(d string) bool { return strings.Contains(d, "members_by_user (user_id=?)") }) {
					t.Fatalf("ListCommunitiesForUser does not search members_by_user by user_id; plan: %q", plan)
				}
			case "postgres":
				var def string
				if err := db.QueryRowContext(ctx,
					`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'members_by_user'`).Scan(&def); err != nil {
					t.Fatalf("read members_by_user: %v", err)
				}
				if def != "CREATE INDEX members_by_user ON public.members USING btree (user_id, community_id)" {
					t.Fatalf("members_by_user is %q", def)
				}
			default:
				t.Fatalf("unknown engine %q", engine)
			}
		})
	}
}
