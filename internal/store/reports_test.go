package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// P2-D21: a ciphertext row records the id of the franking key its tag was made
// under, so a report still verifies after the key rotates.
func TestAnAppMessageKeepsItsFrankingKeyID(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			g := seedGroup(ctx, t, repo, 1_700_000_000)
			keyID := id.New()
			if err := repo.PutAppMessage(ctx, store.AppMessageRow{
				GroupID: g.GroupID, Seq: 1, Epoch: 1, UploaderDevice: id.New(), Blob: []byte("ct"),
				CommitmentC: make([]byte, 32), FrankingTag: make([]byte, 32), Size: 2, Created: 1,
				FrankingKeyID: keyID,
			}); err != nil {
				t.Fatalf("PutAppMessage: %v", err)
			}
			got, err := repo.GetAppMessage(ctx, g.GroupID, 1)
			if err != nil || got.FrankingKeyID != keyID {
				t.Fatalf("GetAppMessage = %v (key %v), %v; want key %v", got, got.FrankingKeyID, err, keyID)
			}
			// A tombstone keeps it, as it keeps the rest of the franking tuple.
			if err := repo.TombstoneAppMessage(ctx, g.GroupID, 1, 2); err != nil {
				t.Fatalf("TombstoneAppMessage: %v", err)
			}
			rows, err := repo.ListAppMessages(ctx, g.GroupID, 1, 10)
			if err != nil || len(rows) != 1 || rows[0].FrankingKeyID != keyID || rows[0].Blob != nil {
				t.Fatalf("ListAppMessages after tombstone = %+v, %v", rows, err)
			}
		})
	}
}

// A readable message stores the whole franking tuple, as mls_app_messages does:
// the uploading device (sender is a user) and C. An edit moves both with the
// tag, and a delete keeps them.
func TestAReadableMessageKeepsItsWholeFrankingTuple(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			ch := channelIn(cid, 0, 1, 2, "readable", 0)
			if err := repo.CreateChannel(ctx, ch); err != nil {
				t.Fatalf("CreateChannel: %v", err)
			}
			device, c := id.New(), bytes.Repeat([]byte{0xc0}, 32)
			if _, err := repo.PutReadableMessage(ctx, store.ReadableMessageRow{
				ChannelID: ch.ID, ChannelHex: ch.ID.String(), Seq: 1, Sender: seedUser(ctx, t, repo).ID,
				Envelope: testEnvelope(t, "hello"), Body: "hello", FrankingTag: make([]byte, 32),
				FrankingKeyID: id.New(), Created: 1, UploaderDevice: device, CommitmentC: c,
			}); err != nil {
				t.Fatalf("PutReadableMessage: %v", err)
			}
			rows, err := repo.ListReadableMessages(ctx, ch.ID, 1, 1)
			if err != nil || len(rows) != 1 || rows[0].UploaderDevice != device || !bytes.Equal(rows[0].CommitmentC, c) {
				t.Fatalf("ListReadableMessages = %+v, %v", rows, err)
			}
			f := store.ReadableFranking{
				Tag: bytes.Repeat([]byte{0x7a}, 32), KeyID: id.New(), UploaderDevice: id.New(),
				CommitmentC: bytes.Repeat([]byte{0xc1}, 32),
			}
			if err := repo.EditReadableMessage(ctx, ch.ID, 1, testEnvelope(t, "edited"), "edited", f, 5); err != nil {
				t.Fatalf("EditReadableMessage: %v", err)
			}
			if err := repo.DeleteReadableMessage(ctx, ch.ID, 1, 6); err != nil {
				t.Fatalf("DeleteReadableMessage: %v", err)
			}
			rows, _ = repo.ListReadableMessages(ctx, ch.ID, 1, 1)
			got := rows[0]
			if !bytes.Equal(got.FrankingTag, f.Tag) || got.FrankingKeyID != f.KeyID || got.UploaderDevice != f.UploaderDevice ||
				!bytes.Equal(got.CommitmentC, f.CommitmentC) || got.Edited == nil || *got.Edited != 5 || got.Deleted == nil {
				t.Fatalf("after edit and delete = %+v", got)
			}
		})
	}
}

// The report queue: newest first, ties broken by id descending, at most limit.
func TestListReportsIsNewestFirstAndBounded(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			if rows, err := repo.ListReports(ctx, 10); err != nil || rows == nil || len(rows) != 0 {
				t.Fatalf("an empty queue = %#v, %v; want an empty non-nil slice", rows, err)
			}
			var ids []id.ID
			for i := range 3 {
				r := store.ReportRow{
					ID: id.New(), Reporter: id.New(), GroupID: id.New(), Seq: uint64(i),
					RevealedEnvelope: []byte{}, KF: make([]byte, 32), FrankingKeyID: id.New(),
					VerificationResult: "verified", Created: int64(100 + i),
				}
				if err := repo.PutReport(ctx, r); err != nil {
					t.Fatalf("PutReport: %v", err)
				}
				ids = append(ids, r.ID)
			}
			rows, err := repo.ListReports(ctx, 2)
			if err != nil || len(rows) != 2 || rows[0].ID != ids[2] || rows[1].ID != ids[1] {
				t.Fatalf("ListReports(2) = %+v, %v", rows, err)
			}
			if len(rows[0].RevealedEnvelope) != 0 || rows[0].VerificationResult != "verified" || rows[0].Created != 102 {
				t.Fatalf("row = %+v", rows[0])
			}
		})
	}
}

// Migration 00012 on a database that already holds a ciphertext and a readable
// message: both rows survive, and the new columns read back as the all-zero id
// (P2-D21's "franked before the column existed"), the all-zero device and a
// NULL commitment.
func TestMigration00012BackfillsTheFrankingColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sqlitemigrations.FS)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	ctx := context.Background()
	if _, err := p.UpTo(ctx, 11); err != nil {
		t.Fatalf("up to 11: %v", err)
	}
	g, cid, ch, u := id.New(), id.New(), id.New(), id.New()
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO users (id, username, display, kind, umk_pub, ssk_pub, sig_umk_ssk, created)
		  VALUES (?, 'u', 'u', 0, ?, ?, ?, 1)`, []any{u, make([]byte, 32), make([]byte, 32), make([]byte, 64)}},
		{`INSERT INTO communities (id, owner, name, policy_json, created) VALUES (?, ?, 'c', '{}', 1)`, []any{cid, u}},
		{`INSERT INTO channels (id, community_id, kind, mode, visibility, name, created)
		  VALUES (?, ?, 0, 1, 2, 'r', 1)`, []any{ch, cid}},
		{`INSERT INTO mls_groups (group_id, binding, kind, target_id, external_sender_key_id, e2ee_version,
		  media_version, policy_version, created) VALUES (?, x'80', 0, ?, ?, 1, 1, 1, 1)`, []any{g, id.New(), id.New()}},
		{`INSERT INTO mls_app_messages (group_id, seq, epoch, uploader_device, blob, commitment_c, franking_tag, size, created)
		  VALUES (?, 1, 1, ?, x'00', ?, ?, 1, 1)`, []any{g, id.New(), make([]byte, 32), make([]byte, 32)}},
		{`INSERT INTO readable_messages (channel_id, channel_hex, seq, sender, envelope, body, franking_tag, franking_key_id, created)
		  VALUES (?, ?, 1, ?, x'80', 'b', ?, ?, 1)`, []any{ch, ch.String(), u, make([]byte, 32), id.New()}},
	} {
		if _, err := db.ExecContext(ctx, stmt.q, stmt.args...); err != nil {
			t.Fatalf("seed %q: %v", stmt.q, err)
		}
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	_ = db.Close()

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
	msg, err := repo.GetAppMessage(ctx, g, 1)
	if err != nil || msg.FrankingKeyID != (id.ID{}) {
		t.Fatalf("the migrated ciphertext row = %+v, %v; want the all-zero key id", msg, err)
	}
	rows, err := repo.ListReadableMessages(ctx, ch, 1, 1)
	if err != nil || len(rows) != 1 || rows[0].UploaderDevice != (id.ID{}) || rows[0].CommitmentC != nil {
		t.Fatalf("the migrated readable row = %+v, %v", rows, err)
	}
	if _, err := repo.GetReport(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetReport(unknown) = %v, want ErrNotFound", err)
	}
}
