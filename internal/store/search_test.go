package store_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// corpus is shared by both engines, so a divergence shows up as a difference in
// hits rather than as a difference in fixtures.
var corpus = []struct {
	channel int
	body    string
}{
	{0, "hej vad händer med krypteringen"},
	{0, "the server never sees plaintext"},
	{0, "håller på med möten hela dagen"},
	{1, "join raids are a metadata problem"},
	{1, "c++ is not a search term"},
}

func TestSearchAgreesAcrossEngines(t *testing.T) {
	type hit struct {
		channel int
		seq     uint64
	}
	results := map[string][]hit{}
	channels := map[string][]id.ID{}

	for engine, repo := range engines(t) {
		ctx := context.Background()
		chans := seedSearchCorpus(t, repo)
		channels[engine] = chans
		for _, raw := range []string{"kryptering", "haller", "never sees", `"never sees"`, "krypt*", "c++", "raids"} {
			q, err := store.ParseQuery(raw)
			if err != nil {
				t.Fatalf("%s: ParseQuery(%q): %v", engine, raw, err)
			}
			hits, err := repo.SearchReadable(ctx, store.ReadableSearchQuery{
				ChannelIDs: chans, Query: q, Limit: 20,
			})
			if err != nil {
				t.Fatalf("%s: SearchReadable(%q): %v", engine, raw, err)
			}
			for _, h := range hits {
				results[engine+"|"+raw] = append(results[engine+"|"+raw], hit{indexOf(chans, h.ChannelID), h.Seq})
			}
		}
	}
	if len(results) == 0 {
		t.Skip("no engine available")
	}
	// håller must be found by haller on both engines: fts5 remove_diacritics 2
	// and the unaccent dictionary inside dilla_simple are the matching pair.
	for engine := range channels {
		if len(results[engine+"|haller"]) != 1 {
			t.Fatalf("%s: 'haller' found %d rows, want 1 (håller)", engine, len(results[engine+"|haller"]))
		}
	}
	if _, ok := channels["postgres"]; ok {
		for _, raw := range []string{"kryptering", "haller", `"never sees"`, "krypt*", "c++", "raids"} {
			a, b := results["sqlite|"+raw], results["postgres|"+raw]
			if len(a) != len(b) {
				t.Fatalf("query %q: sqlite %d hits, postgres %d", raw, len(a), len(b))
			}
		}
	}
}

func TestHostileInputNeverReachesTheEngine(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			chans := seedSearchCorpus(t, repo)
			for _, raw := range []string{`c++ & stuff`, `AND OR NOT`, `unbalanced " quote`, `krypt*`, `a & | ! ( " :*`} {
				q, err := store.ParseQuery(raw)
				if err != nil {
					t.Fatalf("ParseQuery(%q): %v", raw, err)
				}
				if _, err := repo.SearchReadable(t.Context(), store.ReadableSearchQuery{
					ChannelIDs: chans, Query: q, Limit: 10,
				}); err != nil {
					t.Fatalf("SearchReadable(%q): %v", raw, err)
				}
			}
		})
	}
}

func TestSearchIsScopedToTheGivenChannels(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			chans := seedSearchCorpus(t, repo)
			q, _ := store.ParseQuery("raids") // lives in channel 1 only
			hits, err := repo.SearchReadable(t.Context(), store.ReadableSearchQuery{
				ChannelIDs: chans[:1], Query: q, Limit: 10,
			})
			if err != nil {
				t.Fatalf("SearchReadable: %v", err)
			}
			if len(hits) != 0 {
				t.Fatalf("a hit leaked from an out-of-scope channel: %+v", hits)
			}
			if hits, err = repo.SearchReadable(t.Context(), store.ReadableSearchQuery{
				ChannelIDs: nil, Query: q, Limit: 10,
			}); err != nil || len(hits) != 0 {
				t.Fatalf("an empty scope returned %d hits (err %v); it must return none", len(hits), err)
			}
		})
	}
}

func TestEditAndDeleteUpdateTheIndex(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			chans := seedSearchCorpus(t, repo)
			q, _ := store.ParseQuery("plaintext")
			before, err := repo.SearchReadable(t.Context(), store.ReadableSearchQuery{
				ChannelIDs: chans, Query: q, Limit: 10,
			})
			if err != nil || len(before) != 1 {
				t.Fatalf("setup: %d hits, %v", len(before), err)
			}
			// P2-D13's body parameter, task 9's tag and key id, and a real CBOR
			// envelope: the column holds deterministic CBOR (P2-D27), and the body
			// is a separate indexed column that the store cannot derive from the
			// envelope.
			if err := repo.EditReadableMessage(t.Context(), before[0].ChannelID, before[0].Seq,
				testEnvelope(t, "edited body"), "edited body",
				store.ReadableFranking{Tag: make([]byte, 32), KeyID: id.New()}, 2000); err != nil {
				t.Fatalf("EditReadableMessage: %v", err)
			}
			after, _ := repo.SearchReadable(t.Context(), store.ReadableSearchQuery{
				ChannelIDs: chans, Query: q, Limit: 10,
			})
			if len(after) != 0 {
				t.Fatal("the edited body is still in the index")
			}
			q2, _ := store.ParseQuery("raids")
			hits, _ := repo.SearchReadable(t.Context(), store.ReadableSearchQuery{
				ChannelIDs: chans, Query: q2, Limit: 10,
			})
			if len(hits) != 1 {
				t.Fatalf("raids = %d hits before delete", len(hits))
			}
			if err := repo.DeleteReadableMessage(t.Context(), hits[0].ChannelID, hits[0].Seq, 3000); err != nil {
				t.Fatalf("DeleteReadableMessage: %v", err)
			}
			hits, _ = repo.SearchReadable(t.Context(), store.ReadableSearchQuery{
				ChannelIDs: chans, Query: q2, Limit: 10,
			})
			if len(hits) != 0 {
				t.Fatal("a deleted message is still searchable")
			}
		})
	}
}

// `-raids` is the query a moderator types most, and it is where the two engines
// could disagree: FTS5's empty phrase matches nothing, Postgres's `!'raids'`
// matches everything else. Both must answer every in-scope message without it.
func TestANegationOnlySearchAnswersEverythingElseInScope(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			chans := seedSearchCorpus(t, repo)
			q, err := store.ParseQuery("-raids")
			if err != nil {
				t.Fatalf("ParseQuery: %v", err)
			}
			hits, err := repo.SearchReadable(t.Context(), store.ReadableSearchQuery{
				ChannelIDs: chans, Query: q, Limit: 20,
			})
			if err != nil {
				t.Fatalf("SearchReadable: %v", err)
			}
			if len(hits) != len(corpus)-1 {
				t.Fatalf("-raids found %d messages, want %d (every one but the raids message)", len(hits), len(corpus)-1)
			}
			// Scoped: only channel 0's three.
			hits, err = repo.SearchReadable(t.Context(), store.ReadableSearchQuery{
				ChannelIDs: chans[:1], Query: q, Limit: 20,
			})
			if err != nil || len(hits) != 3 {
				t.Fatalf("-raids in channel 0 found %d (err %v), want 3", len(hits), err)
			}
		})
	}
}

// Both engines mark the match the same way, with '[' and ']', so the API can
// hand the snippet to a client without knowing which engine made it.
func TestTheSnippetMarksTheMatch(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			chans := seedSearchCorpus(t, repo)
			q, _ := store.ParseQuery("plaintext")
			hits, err := repo.SearchReadable(t.Context(), store.ReadableSearchQuery{
				ChannelIDs: chans, Query: q, Limit: 10,
			})
			if err != nil || len(hits) != 1 {
				t.Fatalf("SearchReadable = %d hits, %v", len(hits), err)
			}
			h := hits[0]
			if !strings.Contains(h.Snippet, "[plaintext]") {
				t.Fatalf("snippet = %q, want the match marked as [plaintext]", h.Snippet)
			}
			if h.ChannelID != chans[0] || h.Seq != 2 || h.Created != 1_700_000_002 || h.Score <= 0 {
				t.Fatalf("hit = %+v, want channel 0, seq 2, created 1700000002 and a positive score", h)
			}
		})
	}
}

// Hits are best first by the Score they report (protocol/09 "Hits are best
// first, ties broken by the newer seq"), and Limit keeps the best. The two
// channels differ in size on purpose: every SQLite hit also matches its
// channel's hex token, whose IDF grows as the channel shrinks, so an ordering
// by any rank that weights channel_hex above zero favours the small channel's
// weaker body match over the big channel's stronger one.
func TestHitsAreBestFirstByTheReportedScore(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			sender := seedUser(ctx, t, repo).ID
			big, small := channelIn(cid, 0, 1, 2, "big", 0), channelIn(cid, 0, 1, 2, "small", 1)
			for _, ch := range []store.ChannelRow{big, small} {
				if err := repo.CreateChannel(ctx, ch); err != nil {
					t.Fatalf("CreateChannel: %v", err)
				}
			}
			for range 40 {
				putReadable(t, repo, big.ID, sender, "filler chatter", 0)
			}
			// Measured on SQLite: FTS5's default rank puts the small channel's
			// message first (-4.12 against -2.70), while the body-only bm25 the
			// hit reports puts the big channel's first (2.70 against 1.88).
			strong := putReadable(t, repo, big.ID, sender, "needle needle and some more words here", 0)
			putReadable(t, repo, small.ID, sender, "a needle in a short note", 0)
			q, _ := store.ParseQuery("needle")
			scope := []id.ID{big.ID, small.ID}
			hits, err := repo.SearchReadable(ctx, store.ReadableSearchQuery{ChannelIDs: scope, Query: q, Limit: 10})
			if err != nil || len(hits) != 2 {
				t.Fatalf("SearchReadable = %d hits, %v; want 2", len(hits), err)
			}
			for i := 1; i < len(hits); i++ {
				if hits[i].Score > hits[i-1].Score {
					t.Fatalf("hit %d scores %v above hit %d's %v: hits are not best first",
						i, hits[i].Score, i-1, hits[i-1].Score)
				}
			}
			if hits[0].ChannelID != big.ID || hits[0].Seq != strong {
				t.Fatalf("best hit = channel %s seq %d, want the big channel's seq %d", hits[0].ChannelID, hits[0].Seq, strong)
			}
			top, err := repo.SearchReadable(ctx, store.ReadableSearchQuery{ChannelIDs: scope, Query: q, Limit: 1})
			if err != nil || len(top) != 1 || top[0].ChannelID != big.ID || top[0].Seq != strong {
				t.Fatalf("Limit 1 = %+v, %v; want only the best hit, the big channel's seq %d", top, err, strong)
			}
		})
	}
}

// BeforeSeq pages backwards: only hits strictly below it come back.
func TestBeforeSeqPagesBackwards(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			chans := seedSearchCorpus(t, repo)
			q, _ := store.ParseQuery("-nothingmatchesthis")
			hits, err := repo.SearchReadable(t.Context(), store.ReadableSearchQuery{
				ChannelIDs: chans[:1], Query: q, Limit: 10, BeforeSeq: 3,
			})
			if err != nil {
				t.Fatalf("SearchReadable: %v", err)
			}
			if len(hits) != 2 {
				t.Fatalf("BeforeSeq 3 found %d hits, want seqs 1 and 2", len(hits))
			}
			for _, h := range hits {
				if h.Seq >= 3 {
					t.Fatalf("BeforeSeq 3 returned seq %d", h.Seq)
				}
			}
		})
	}
}

// A scope wider than one MATCH may name is chunked, never truncated: a hit in
// the last channel of a 70-channel scope is found, and Limit still holds.
func TestAWideScopeIsChunkedNotTruncated(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			sender := seedUser(ctx, t, repo).ID
			var chans []id.ID
			for i := range 70 {
				ch := channelIn(cid, 0, 1, 2, "c", uint64(i))
				if err := repo.CreateChannel(ctx, ch); err != nil {
					t.Fatalf("CreateChannel: %v", err)
				}
				chans = append(chans, ch.ID)
				body := "filler"
				if i == 69 || i == 3 {
					body = "needle in channel"
				}
				putReadable(t, repo, ch.ID, sender, body, 1_700_000_000)
			}
			q, _ := store.ParseQuery("needle")
			hits, err := repo.SearchReadable(ctx, store.ReadableSearchQuery{ChannelIDs: chans, Query: q, Limit: 10})
			if err != nil {
				t.Fatalf("SearchReadable: %v", err)
			}
			if len(hits) != 2 {
				t.Fatalf("found %d needles across 70 channels, want 2", len(hits))
			}
			q, _ = store.ParseQuery("filler")
			hits, err = repo.SearchReadable(ctx, store.ReadableSearchQuery{ChannelIDs: chans, Query: q, Limit: 5})
			if err != nil || len(hits) != 5 {
				t.Fatalf("Limit 5 over 68 matches returned %d (err %v)", len(hits), err)
			}
		})
	}
}

// The rest of Readable: the row round trip, the edit and delete guards, the
// slowmode read, the audience and the monotone read state.
func TestReadableMessagesRoundTrip(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			ch := channelIn(cid, 0, 1, 2, "readable", 0)
			if err := repo.CreateChannel(ctx, ch); err != nil {
				t.Fatalf("CreateChannel: %v", err)
			}
			sender := seedUser(ctx, t, repo).ID
			keyID := id.New()
			env := testEnvelope(t, "hello")
			in := store.ReadableMessageRow{
				ChannelID: ch.ID, ChannelHex: ch.ID.String(), Seq: 1, Sender: sender,
				Envelope: env, Body: "hello", FrankingTag: make([]byte, 32), FrankingKeyID: keyID,
				MentionCount: 2, Created: 1_700_000_000,
			}
			rowID, err := repo.PutReadableMessage(ctx, in)
			if err != nil || rowID <= 0 {
				t.Fatalf("PutReadableMessage = %d, %v", rowID, err)
			}
			if _, err := repo.PutReadableMessage(ctx, in); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("a second seq 1 = %v, want ErrConflict", err)
			}
			rows, err := repo.ListReadableMessages(ctx, ch.ID, 1, 10)
			if err != nil || len(rows) != 1 {
				t.Fatalf("ListReadableMessages = %d rows, %v", len(rows), err)
			}
			got := rows[0]
			if got.ID != rowID || got.ChannelID != ch.ID || got.ChannelHex != ch.ID.String() || got.Seq != 1 ||
				got.Sender != sender || !bytes.Equal(got.Envelope, env) || got.Body != "hello" ||
				got.FrankingKeyID != keyID || got.MentionCount != 2 || got.Created != 1_700_000_000 ||
				got.Edited != nil || got.Deleted != nil {
				t.Fatalf("row = %+v", got)
			}
			if rows, err := repo.ListReadableMessages(ctx, ch.ID, 2, 10); err != nil || len(rows) != 0 {
				t.Fatalf("from 2 = %d rows, %v; want none", len(rows), err)
			}

			at, err := repo.LastReadableMessageAt(ctx, ch.ID, sender)
			if err != nil || at != 1_700_000_000 {
				t.Fatalf("LastReadableMessageAt = %d, %v", at, err)
			}
			if _, err := repo.LastReadableMessageAt(ctx, ch.ID, id.New()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("LastReadableMessageAt(stranger) = %v, want ErrNotFound", err)
			}

			// An edit moves the franking tuple with the bytes: a new tag, made
			// under whichever key is current at the edit (task 9).
			edited := testEnvelope(t, "hello again")
			editTag := bytes.Repeat([]byte{0x5e}, 32)
			editKeyID := id.New()
			editF := store.ReadableFranking{Tag: editTag, KeyID: editKeyID}
			if err := repo.EditReadableMessage(ctx, ch.ID, 1, edited, "hello again", editF, 1_700_000_100); err != nil {
				t.Fatalf("EditReadableMessage: %v", err)
			}
			if err := repo.EditReadableMessage(ctx, ch.ID, 9, edited, "x", editF, 1); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("EditReadableMessage(unknown seq) = %v, want ErrNotFound", err)
			}
			rows, _ = repo.ListReadableMessages(ctx, ch.ID, 1, 1)
			if !bytes.Equal(rows[0].Envelope, edited) || rows[0].Body != "hello again" ||
				!bytes.Equal(rows[0].FrankingTag, editTag) || rows[0].FrankingKeyID != editKeyID ||
				rows[0].Created != 1_700_000_000 || rows[0].Edited == nil || *rows[0].Edited != 1_700_000_100 {
				t.Fatalf("after edit = %+v", rows[0])
			}

			if err := repo.DeleteReadableMessage(ctx, ch.ID, 1, 1_700_000_200); err != nil {
				t.Fatalf("DeleteReadableMessage: %v", err)
			}
			rows, _ = repo.ListReadableMessages(ctx, ch.ID, 1, 1)
			// The envelope and body are gone; the franking tuple survives for a report.
			if len(rows[0].Envelope) != 0 || rows[0].Body != "" || rows[0].Deleted == nil ||
				*rows[0].Deleted != 1_700_000_200 || !bytes.Equal(rows[0].FrankingTag, editTag) ||
				rows[0].FrankingKeyID != editKeyID || rows[0].Sender != sender {
				t.Fatalf("after delete = %+v", rows[0])
			}
			if err := repo.DeleteReadableMessage(ctx, ch.ID, 1, 1); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("a second delete = %v, want ErrNotFound", err)
			}
			if err := repo.EditReadableMessage(ctx, ch.ID, 1, edited, "revived", editF, 1); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("an edit of a deleted message = %v, want ErrNotFound", err)
			}
			// Deleting the last message does not reset the slowmode gate.
			if at, err := repo.LastReadableMessageAt(ctx, ch.ID, sender); err != nil || at != 1_700_000_000 {
				t.Fatalf("LastReadableMessageAt after delete = %d, %v", at, err)
			}

			// read_state only moves forward.
			reader := seedUser(ctx, t, repo).ID
			if _, err := repo.GetReadState(ctx, reader, ch.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetReadState before any write = %v, want ErrNotFound", err)
			}
			for _, step := range []struct{ put, want uint64 }{{5, 5}, {3, 5}, {9, 9}} {
				if err := repo.PutReadState(ctx, reader, ch.ID, step.put); err != nil {
					t.Fatalf("PutReadState(%d): %v", step.put, err)
				}
				if got, err := repo.GetReadState(ctx, reader, ch.ID); err != nil || got != step.want {
					t.Fatalf("after PutReadState(%d): %d, %v; want %d", step.put, got, err, step.want)
				}
			}
		})
	}
}

// The fan-out audience is channel_members intersected with the community's
// members, so a user who left the community stops receiving before anything
// re-materialises the channel.
func TestListReadableAudience(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			ch := channelIn(cid, 0, 1, 2, "readable", 0)
			if err := repo.CreateChannel(ctx, ch); err != nil {
				t.Fatalf("CreateChannel: %v", err)
			}
			a, b, gone := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID
			for _, u := range []id.ID{a, b, gone} {
				if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: u, Joined: 1}); err != nil {
					t.Fatalf("PutMember: %v", err)
				}
				if err := repo.PutChannelMember(ctx, ch.ID, u, 1); err != nil {
					t.Fatalf("PutChannelMember: %v", err)
				}
			}
			if err := repo.DeleteMember(ctx, cid, gone); err != nil {
				t.Fatalf("DeleteMember: %v", err)
			}
			got, err := repo.ListReadableAudience(ctx, ch.ID)
			if err != nil {
				t.Fatalf("ListReadableAudience: %v", err)
			}
			want := []id.ID{a, b}
			slices.SortFunc(want, func(x, y id.ID) int { return bytes.Compare(x[:], y[:]) })
			if !slices.Equal(got, want) {
				t.Fatalf("audience = %v, want %v (the departed member excluded)", got, want)
			}
		})
	}
}

// seedSearchCorpus creates one community with two readable channels and writes
// corpus into them in order, so channel 0 holds seqs 1-3 and channel 1 seqs 1-2.
// Message n (0-based, across the corpus) is created at 1_700_000_000 + its seq.
func seedSearchCorpus(t *testing.T, repo store.Repository) []id.ID {
	t.Helper()
	ctx := context.Background()
	cid := seedCommunity(ctx, t, repo)
	sender := seedUser(ctx, t, repo).ID
	chans := make([]id.ID, 2)
	for i := range chans {
		ch := channelIn(cid, 0, 1, 2, "readable", uint64(i))
		if err := repo.CreateChannel(ctx, ch); err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
		chans[i] = ch.ID
	}
	for _, m := range corpus {
		putReadable(t, repo, chans[m.channel], sender, m.body, 0)
	}
	return chans
}

// putReadable appends one message at the channel's next seq. created 0 means
// 1_700_000_000 + seq.
func putReadable(t *testing.T, repo store.Repository, ch, sender id.ID, body string, created int64) uint64 {
	t.Helper()
	ctx := context.Background()
	seq, err := repo.NextChannelSeq(ctx, ch)
	if err != nil {
		t.Fatalf("NextChannelSeq: %v", err)
	}
	if created == 0 {
		created = 1_700_000_000 + int64(seq)
	}
	if _, err := repo.PutReadableMessage(ctx, store.ReadableMessageRow{
		ChannelID: ch, ChannelHex: ch.String(), Seq: seq, Sender: sender,
		Envelope: testEnvelope(t, body), Body: body, FrankingTag: make([]byte, 32),
		FrankingKeyID: id.New(), Created: created,
	}); err != nil {
		t.Fatalf("PutReadableMessage: %v", err)
	}
	return seq
}

// testEnvelope is protocol/04's nine-element deterministic CBOR envelope with the
// given body and a 32-byte k_f: the column holds CBOR (P2-D27), never JSON text.
func testEnvelope(t *testing.T, body string) []byte {
	t.Helper()
	b, err := cborx.Marshal([]any{
		uint64(1), id.New(), uint64(0), nil, nil, body, []any{}, []any{}, bytes.Repeat([]byte{0x06}, 32),
	})
	if err != nil {
		t.Fatalf("cborx.Marshal: %v", err)
	}
	return b
}

func indexOf(chans []id.ID, c id.ID) int {
	return slices.Index(chans, c)
}
