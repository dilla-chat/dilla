package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// seedGroup creates one open text group created at `created`.
func seedGroup(ctx context.Context, t *testing.T, repo store.Repository, created int64) store.GroupRow {
	t.Helper()
	g := store.GroupRow{
		GroupID: id.New(), Binding: []byte{0x80}, Kind: 0, TargetID: id.New(), Ciphersuite: 1,
		ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1,
		Created: created,
	}
	if err := repo.CreateGroup(ctx, g); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	return g
}

func appendHandshake(ctx context.Context, t *testing.T, repo store.Repository, g id.ID, seq uint64, created int64) {
	t.Helper()
	if err := repo.AppendHandshake(ctx, store.HandshakeRow{
		GroupID: g, Seq: seq, Epoch: 1, Kind: 1, Blob: []byte("commit"), Created: created,
	}); err != nil {
		t.Fatalf("AppendHandshake(%d): %v", seq, err)
	}
}

func putMessage(ctx context.Context, t *testing.T, repo store.Repository, g id.ID, seq uint64, created int64, expires *int64) {
	t.Helper()
	if err := repo.PutAppMessage(ctx, store.AppMessageRow{
		GroupID: g, Seq: seq, Epoch: 1, UploaderDevice: id.New(), Blob: []byte("ct"),
		FrankingTag: make([]byte, 32), Size: 2, Created: created, Expires: expires,
	}); err != nil {
		t.Fatalf("PutAppMessage(%d): %v", seq, err)
	}
}

func groupMarks(ctx context.Context, t *testing.T, repo store.Repository, g id.ID) (messages, handshakes uint64) {
	t.Helper()
	row, err := repo.GetGroup(ctx, g)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	return row.PrunedBelow, row.HandshakesPruned
}

// The MLS, Messages and Cursors methods the delivery service leans on, on BOTH engines: until the
// final review only the account and auth surface ran against Postgres in CI, so a Postgres-only
// defect in any of these surfaced — if at all — as a delivery-service symptom three packages away.
// Every assertion names what the delivery service relies on.
func TestMLSMessagesAndCursorsConformance(t *testing.T) {
	ctx := context.Background()
	for name, repo := range engines(t) {
		t.Run(name, func(t *testing.T) {
			t.Run("NextSeq is dense and per group", func(t *testing.T) {
				a, b := seedGroup(ctx, t, repo, 1_000), seedGroup(ctx, t, repo, 1_000)
				for want := uint64(1); want <= 3; want++ {
					got, err := repo.NextSeq(ctx, a.GroupID)
					if err != nil || got != want {
						t.Fatalf("NextSeq(a) = %d, %v; want %d", got, err, want)
					}
				}
				if got, err := repo.NextSeq(ctx, b.GroupID); err != nil || got != 1 {
					t.Fatalf("NextSeq(b) = %d, %v; want 1, its own space", got, err)
				}
				row, err := repo.GetGroup(ctx, a.GroupID)
				if err != nil || row.Seq != 3 {
					t.Fatalf("GetGroup(a).Seq = %d, %v; want the high-water 3", row.Seq, err)
				}
			})

			t.Run("ReplaceMembers rewrites the leaves and GroupsForDevice follows", func(t *testing.T) {
				g := seedGroup(ctx, t, repo, 1_000)
				stay, leave := id.New(), id.New()
				member := func(leaf uint32, device id.ID, removed *uint64) store.MemberRow {
					return store.MemberRow{GroupID: g.GroupID, LeafIndex: leaf, UserID: id.New(),
						DeviceID: device, SignatureKey: make([]byte, 32), AddedEpoch: 1, RemovedEpoch: removed}
				}
				if err := repo.ReplaceMembers(ctx, g.GroupID, 1, []store.MemberRow{
					member(0, stay, nil), member(1, leave, nil),
				}); err != nil {
					t.Fatalf("ReplaceMembers: %v", err)
				}
				if ms, err := repo.ListMembers(ctx, g.GroupID); err != nil || len(ms) != 2 {
					t.Fatalf("ListMembers = %v, %v; want two leaves", ms, err)
				}
				removed := uint64(2)
				if err := repo.ReplaceMembers(ctx, g.GroupID, 2, []store.MemberRow{
					member(0, stay, nil), member(1, leave, &removed),
				}); err != nil {
					t.Fatalf("ReplaceMembers: %v", err)
				}
				// ListMembers is the CURRENT tree: a removed leaf is not in it.
				ms, err := repo.ListMembers(ctx, g.GroupID)
				if err != nil || len(ms) != 1 || ms[0].DeviceID != stay {
					t.Fatalf("ListMembers after the removal = %+v, %v; want the one current leaf", ms, err)
				}
				if gs, err := repo.GroupsForDevice(ctx, stay); err != nil || len(gs) != 1 || gs[0] != g.GroupID {
					t.Fatalf("GroupsForDevice(stay) = %v, %v", gs, err)
				}
				if gs, err := repo.GroupsForDevice(ctx, leave); err != nil || len(gs) != 0 {
					t.Fatalf("GroupsForDevice(removed) = %v, %v; want none", gs, err)
				}
			})

			t.Run("ListBarredMembers is the live leaves of quarantined or revoked devices", func(t *testing.T) {
				g := seedGroup(ctx, t, repo, 1_000)
				user := seedUser(ctx, t, repo).ID
				fine, quarantined, revoked, gone := seedDevice(ctx, t, repo, user), seedDevice(ctx, t, repo, user),
					seedDevice(ctx, t, repo, user), seedDevice(ctx, t, repo, user)
				if err := repo.QuarantineDevice(ctx, quarantined, 2_000, "fork quorum"); err != nil {
					t.Fatalf("QuarantineDevice: %v", err)
				}
				for _, d := range []id.ID{revoked, gone} {
					if err := repo.RevokeDevice(ctx, d, 2_000); err != nil {
						t.Fatalf("RevokeDevice: %v", err)
					}
				}
				removed := uint64(2)
				rows := []store.MemberRow{}
				for leaf, d := range []id.ID{fine, quarantined, revoked, gone} {
					m := store.MemberRow{GroupID: g.GroupID, LeafIndex: uint32(leaf), UserID: user, DeviceID: d,
						SignatureKey: make([]byte, 32), AddedEpoch: 1}
					if d == gone {
						m.RemovedEpoch = &removed
					}
					rows = append(rows, m)
				}
				if err := repo.ReplaceMembers(ctx, g.GroupID, 2, rows); err != nil {
					t.Fatalf("ReplaceMembers: %v", err)
				}
				barred, err := repo.ListBarredMembers(ctx, g.GroupID)
				if err != nil || len(barred) != 2 || barred[0].DeviceID != quarantined || barred[0].LeafIndex != 1 ||
					barred[1].DeviceID != revoked || barred[1].LeafIndex != 2 {
					t.Fatalf("ListBarredMembers = %+v, %v; want the quarantined leaf 1 and the revoked leaf 2", barred, err)
				}
			})

			t.Run("proposals: put, list live and void, reissue keeps the action", func(t *testing.T) {
				g := seedGroup(ctx, t, repo, 1_000)
				leaf := uint32(3)
				action := id.New()
				p := store.ProposalRow{GroupID: g.GroupID, Ref: []byte("ref-1"), Epoch: 0, Kind: 2,
					TargetLeaf: &leaf, Origin: 0, ActionID: action, IssuedAt: 1_000, TTL: 60}
				if err := repo.PutProposal(ctx, p); err != nil {
					t.Fatalf("PutProposal: %v", err)
				}
				if live, err := repo.ListProposals(ctx, g.GroupID, 0, false); err != nil || len(live) != 1 {
					t.Fatalf("ListProposals(live) = %v, %v", live, err)
				}
				fresh := p
				fresh.Ref, fresh.ActionID = []byte("ref-2"), id.New()
				if err := repo.ReissueProposal(ctx, []byte("ref-1"), fresh); err != nil {
					t.Fatalf("ReissueProposal: %v", err)
				}
				live, err := repo.ListProposals(ctx, g.GroupID, 0, false)
				if err != nil || len(live) != 1 || string(live[0].Ref) != "ref-2" || live[0].ActionID != action {
					t.Fatalf("after the reissue = %+v, %v; want ref-2 carrying the original action", live, err)
				}
				if err := repo.VoidProposal(ctx, g.GroupID, []byte("ref-2"), 2_000); err != nil {
					t.Fatalf("VoidProposal: %v", err)
				}
				if live, _ := repo.ListProposals(ctx, g.GroupID, 0, false); len(live) != 0 {
					t.Fatalf("a void proposal is still live: %+v", live)
				}
				all, err := repo.ListProposals(ctx, g.GroupID, 0, true)
				if err != nil || len(all) != 1 || all[0].VoidAt == nil || *all[0].VoidAt != 2_000 {
					t.Fatalf("ListProposals(all) = %+v, %v", all, err)
				}
				if err := repo.DeleteProposals(ctx, g.GroupID, [][]byte{[]byte("ref-2")}); err != nil {
					t.Fatalf("DeleteProposals: %v", err)
				}
				if all, _ := repo.ListProposals(ctx, g.GroupID, 0, true); len(all) != 0 {
					t.Fatalf("a deleted proposal is still listed: %+v", all)
				}
			})

			t.Run("welcomes: one payload, per-device rows, the epoch tree, delivery", func(t *testing.T) {
				g := seedGroup(ctx, t, repo, 1_000)
				sum := make([]byte, 32)
				copy(sum, id.New().String())
				if err := repo.PutWelcomePayload(ctx, store.WelcomePayloadRow{
					BlobSHA256: sum, GroupID: g.GroupID, Epoch: 4, Blob: []byte("welcome"), Created: 1_000,
				}); err != nil {
					t.Fatalf("PutWelcomePayload: %v", err)
				}
				if err := repo.PutEpochTree(ctx, store.EpochTreeRow{
					GroupID: g.GroupID, Epoch: 4, RatchetTree: []byte("tree"), TreeHash: make([]byte, 32), Created: 1_000,
				}); err != nil {
					t.Fatalf("PutEpochTree: %v", err)
				}
				alice, bob := id.New(), id.New()
				if err := repo.PutWelcomes(ctx, []store.WelcomeRow{
					{DeviceID: alice, GroupID: g.GroupID, Epoch: 4, CommitSeq: 9, BlobSHA256: sum, Created: 1_000, Expires: 5_000},
					{DeviceID: bob, GroupID: g.GroupID, Epoch: 4, CommitSeq: 9, BlobSHA256: sum, Created: 1_000, Expires: 5_000},
				}); err != nil {
					t.Fatalf("PutWelcomes: %v", err)
				}
				ws, err := repo.ListWelcomes(ctx, alice, 0, 10)
				if err != nil || len(ws) != 1 || string(ws[0].Blob) != "welcome" || string(ws[0].RatchetTree) != "tree" ||
					ws[0].CommitSeq != 9 {
					t.Fatalf("ListWelcomes(alice) = %+v, %v; want the payload joined to its epoch tree", ws, err)
				}
				if err := repo.DeleteWelcome(ctx, alice, ws[0].WelcomeID, 2_000); err != nil {
					t.Fatalf("DeleteWelcome: %v", err)
				}
				if ws, _ := repo.ListWelcomes(ctx, alice, 0, 10); len(ws) != 0 {
					t.Fatalf("an acknowledged Welcome is still pending: %+v", ws)
				}
				if ws, _ := repo.ListWelcomes(ctx, bob, 0, 10); len(ws) != 1 {
					t.Fatalf("bob's Welcome went with alice's acknowledgement: %+v", ws)
				}
				if n, err := repo.PruneWelcomes(ctx, 6_000); err != nil || n < 1 {
					t.Fatalf("PruneWelcomes = %d, %v; want the expired rows gone", n, err)
				}
				if ws, _ := repo.ListWelcomes(ctx, bob, 0, 10); len(ws) != 0 {
					t.Fatalf("an expired Welcome is still listed: %+v", ws)
				}
			})

			t.Run("MinCursor is the lowest eligible cursor", func(t *testing.T) {
				g := seedGroup(ctx, t, repo, 1_000)
				if floor, err := repo.MinCursor(ctx, g.GroupID, 0); err != nil || floor != 0 {
					t.Fatalf("MinCursor with no cursors = %d, %v; want 0", floor, err)
				}
				u := seedUser(ctx, t, repo)
				revoked := id.New()
				if err := repo.CreateDevice(ctx, store.DeviceRow{ID: revoked, UserID: u.ID,
					DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, Created: 1_000}); err != nil {
					t.Fatalf("CreateDevice: %v", err)
				}
				if err := repo.RevokeDevice(ctx, revoked, 1_500); err != nil {
					t.Fatalf("RevokeDevice: %v", err)
				}
				for _, c := range []struct {
					device  id.ID
					seq     uint64
					updated int64
				}{
					{id.New(), 7, 2_000}, // eligible, and the floor
					{id.New(), 9, 2_000}, // eligible
					{id.New(), 3, 100},   // idle past the horizon
					{revoked, 1, 2_000},  // revoked
				} {
					if err := repo.PutCursor(ctx, c.device, g.GroupID, c.seq, 1, c.updated); err != nil {
						t.Fatalf("PutCursor: %v", err)
					}
				}
				if floor, err := repo.MinCursor(ctx, g.GroupID, 1_000); err != nil || floor != 7 {
					t.Fatalf("MinCursor = %d, %v; want 7 (the idle and the revoked cursor do not hold it)", floor, err)
				}
			})

			t.Run("MarkAllGroupsEpochUnknown and SetGeneration", func(t *testing.T) {
				g := seedGroup(ctx, t, repo, 1_000)
				if err := repo.MarkAllGroupsEpochUnknown(ctx, 9_000); err != nil {
					t.Fatalf("MarkAllGroupsEpochUnknown: %v", err)
				}
				row, err := repo.GetGroup(ctx, g.GroupID)
				if err != nil || !row.EpochUnknown || row.HealDeadline == nil || *row.HealDeadline != 9_000 {
					t.Fatalf("after the restore mark = %+v, %v", row, err)
				}
				if err := repo.ClearEpochUnknown(ctx, g.GroupID); err != nil {
					t.Fatalf("ClearEpochUnknown: %v", err)
				}
				if row, _ := repo.GetGroup(ctx, g.GroupID); row.EpochUnknown || row.HealDeadline != nil {
					t.Fatalf("a healed group kept its flag or deadline: %+v", row)
				}

				// The instance row is one per database and the Postgres leg shares its database
				// across tests (TestRepositoryConformance asserts the generation it bumps), so
				// the generation is exercised inside a transaction that is rolled back.
				errRollback := errors.New("roll back")
				err = repo.Tx(ctx, func(tx store.Repository) error {
					in, err := tx.GetInstance(ctx)
					if errors.Is(err, store.ErrNotFound) {
						in = store.InstanceRow{InstanceID: id.New(), ExternalSenderKeyID: id.New(), KeyHistory: []byte{1},
							FrankingKeyID: id.New(), Generation: 1, PolicyVersion: 1, Created: 1}
						if err := tx.CreateInstance(ctx, in); err != nil {
							t.Errorf("CreateInstance: %v", err)
							return errRollback
						}
					} else if err != nil {
						t.Errorf("GetInstance: %v", err)
						return errRollback
					}
					target := in.Generation + 5
					if err := tx.SetGeneration(ctx, target); err != nil {
						t.Errorf("SetGeneration: %v", err)
					}
					if err := tx.SetGeneration(ctx, target-3); err != nil {
						t.Errorf("SetGeneration(lower): %v", err)
					}
					if got, err := tx.GetInstance(ctx); err != nil || got.Generation != target {
						t.Errorf("generation = %d, %v; want %d, monotone", got.Generation, err, target)
					}
					return errRollback
				})
				if !errors.Is(err, errRollback) {
					t.Fatalf("Tx: %v", err)
				}
			})
		})
	}
}

// Invariant 10's two high-waters record EXACTLY what retention deleted, per group and per stream,
// on both engines: the catch-up's E_PRUNED is decided against them (deviation B20's exact floor).
// A prune that deletes nothing moves nothing, a later prune never walks a mark back, and each
// DELIVERY trigger — the cursor floor and the delivery window — records the highest seq it took.
// An archival expiry deletes without moving the mark (Plan 2 task 8's retention ruling, see
// TestAnArchivalExpiryNeverRaisesThePrunedFloor).
func TestRetentionHighWatersRecordExactlyWhatWasDeleted(t *testing.T) {
	ctx := context.Background()
	for name, repo := range engines(t) {
		t.Run(name, func(t *testing.T) {
			const day = int64(24 * 60 * 60)
			old, young := seedGroup(ctx, t, repo, 1_000), seedGroup(ctx, t, repo, 1_000)

			// Handshakes: seqs 2 and 5 are old, 9 is young; the other group's are all young.
			appendHandshake(ctx, t, repo, old.GroupID, 2, 1_000)
			appendHandshake(ctx, t, repo, old.GroupID, 5, 1_000)
			appendHandshake(ctx, t, repo, old.GroupID, 9, 40*day)
			appendHandshake(ctx, t, repo, young.GroupID, 3, 40*day)

			n, err := repo.PruneHandshakes(ctx, 10*day)
			if err != nil || n != 2 {
				t.Fatalf("PruneHandshakes = %d, %v; want the two old rows", n, err)
			}
			if _, hs := groupMarks(ctx, t, repo, old.GroupID); hs != 5 {
				t.Errorf("handshakes_pruned_through = %d, want 5, the highest seq deleted", hs)
			}
			if _, hs := groupMarks(ctx, t, repo, young.GroupID); hs != 0 {
				t.Errorf("a group that lost nothing has handshakes_pruned_through = %d, want 0", hs)
			}
			// Nothing more to delete: the mark stays where the deletion put it.
			if n, err := repo.PruneHandshakes(ctx, 10*day); err != nil || n != 0 {
				t.Fatalf("second PruneHandshakes = %d, %v", n, err)
			}
			if _, hs := groupMarks(ctx, t, repo, old.GroupID); hs != 5 {
				t.Errorf("an empty prune moved the mark to %d", hs)
			}

			// Messages: the cursor floor takes seq 3, the delivery window takes seq 4, an archival
			// expiry takes seq 8; seq 6 stays.
			expired := 50 * day
			putMessage(ctx, t, repo, young.GroupID, 3, 45*day, nil)
			putMessage(ctx, t, repo, young.GroupID, 4, 1_000, nil)
			putMessage(ctx, t, repo, young.GroupID, 6, 45*day, nil)
			putMessage(ctx, t, repo, young.GroupID, 8, 45*day, &expired)

			n, err = repo.PruneAppMessages(ctx, young.GroupID, 3, 10*day, 60*day)
			if err != nil || n != 3 {
				t.Fatalf("PruneAppMessages = %d, %v; want seqs 3, 4 and 8", n, err)
			}
			// 4, not 8: seq 6 survives, and a mark at 8 would refuse a catch-up from 5 with
			// E_PRUNED although everything it asks for that still exists is there.
			if msgs, _ := groupMarks(ctx, t, repo, young.GroupID); msgs != 4 {
				t.Errorf("pruned_below = %d, want 4, the highest seq a delivery trigger deleted", msgs)
			}
			rows, err := repo.ListAppMessages(ctx, young.GroupID, 0, 10)
			if err != nil || len(rows) != 1 || rows[0].Seq != 6 {
				t.Fatalf("survivors = %v, %v; want seq 6 alone", rows, err)
			}
			// A later prune under a lower floor deletes nothing and does not walk the mark back.
			if n, err := repo.PruneAppMessages(ctx, young.GroupID, 1, 10*day, 60*day); err != nil || n != 0 {
				t.Fatalf("second PruneAppMessages = %d, %v", n, err)
			}
			if msgs, _ := groupMarks(ctx, t, repo, young.GroupID); msgs != 4 {
				t.Errorf("pruned_below walked back to %d", msgs)
			}
			if msgs, _ := groupMarks(ctx, t, repo, old.GroupID); msgs != 0 {
				t.Errorf("a group whose messages were never pruned has pruned_below = %d", msgs)
			}
		})
	}
}

// The retention ruling of Plan 2 task 8: a per-message archival expiry must never create a hole
// below surviving seqs in the pruned floor. `expires` is not monotone in seq — a community that
// shortens its retention makes newer messages expire before older ones — so a floor raised to the
// highest seq an expiry took would stand ABOVE messages that still exist, and the catch-up would
// answer E_PRUNED for a range it can serve. The choice made: archival deletions are exempt from
// pruned_below. They remove rows; they never move the mark. A catch-up across an expired message
// simply does not return it — the policy deleted it for every device alike, and there is nothing a
// resync could recover.
func TestAnArchivalExpiryNeverRaisesThePrunedFloor(t *testing.T) {
	ctx := context.Background()
	for name, repo := range engines(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGroup(ctx, t, repo, 1_000)
			early := int64(1_500)
			// The NEWEST message expires first, the older two are retained indefinitely.
			putMessage(ctx, t, repo, g.GroupID, 1, 1_000, nil)
			putMessage(ctx, t, repo, g.GroupID, 2, 1_000, nil)
			putMessage(ctx, t, repo, g.GroupID, 3, 1_000, &early)

			n, err := repo.PruneAppMessages(ctx, g.GroupID, 0, 0, 2_000)
			if err != nil || n != 1 {
				t.Fatalf("PruneAppMessages = %d, %v; want the expired seq 3 alone", n, err)
			}
			if msgs, _ := groupMarks(ctx, t, repo, g.GroupID); msgs != 0 {
				t.Fatalf("pruned_below = %d after an archival expiry; seqs 1 and 2 survive below it", msgs)
			}
			rows, err := repo.ListAppMessages(ctx, g.GroupID, 1, 10)
			if err != nil || len(rows) != 2 || rows[0].Seq != 1 || rows[1].Seq != 2 {
				t.Fatalf("survivors = %v, %v; want seqs 1 and 2", rows, err)
			}

			// A delivery trigger in the same call still records exactly what IT took: the cursor
			// floor takes seq 1, an expiry takes seq 4, and the mark is 1 — seq 2 still survives.
			putMessage(ctx, t, repo, g.GroupID, 4, 1_000, &early)
			n, err = repo.PruneAppMessages(ctx, g.GroupID, 1, 0, 2_000)
			if err != nil || n != 2 {
				t.Fatalf("second PruneAppMessages = %d, %v; want seqs 1 and 4", n, err)
			}
			if msgs, _ := groupMarks(ctx, t, repo, g.GroupID); msgs != 1 {
				t.Fatalf("pruned_below = %d, want 1, the highest seq the cursor floor took", msgs)
			}
			rows, err = repo.ListAppMessages(ctx, g.GroupID, 2, 10)
			if err != nil || len(rows) != 1 || rows[0].Seq != 2 {
				t.Fatalf("survivors = %v, %v; want seq 2", rows, err)
			}
		})
	}
}
