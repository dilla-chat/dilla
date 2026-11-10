//go:build ignore

// generate writes rows.json: the encrypted-state rows the v1 Bot's own stores write for a fixed community (Bot v2
// Task 17, G7). It must run against the v1 code, so generate.sh copies it into a checkout of tag v0.1.0-signal2.1 and
// runs it there; it never runs against the current tree (build tag ignore). Everything is fixed (ids, fingerprints,
// clocks), so a second run writes the same bytes.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"os"
	"sort"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/moderation"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/polls"
	"github.com/dilla-chat/dilla/internal/bot/reminders"
	"github.com/dilla-chat/dilla/internal/id"
)

func fx(n byte) (v id.ID) { v[15] = n; return }

type rowKey struct {
	area uint64
	key  string
}

// memState is the helper's encrypted state area in memory, with its revision checks.
type memState struct {
	binding native.Binding
	rows    map[rowKey]native.StateValue
}

func (s *memState) Binding() native.Binding { return s.binding }

func (s *memState) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	return s.rows[rowKey{area, string(key)}], nil
}

func (s *memState) StateScan(_ context.Context, area uint64, after []byte, limit uint64) (native.StatePage, error) {
	var keys []string
	for k := range s.rows {
		if k.area == area && (len(after) == 0 || k.key > string(after)) {
			keys = append(keys, k.key)
		}
	}
	sort.Strings(keys)
	if uint64(len(keys)) > limit {
		keys = keys[:limit]
	}
	var page native.StatePage
	for _, k := range keys {
		v := s.rows[rowKey{area, k}]
		page.Entries = append(page.Entries, native.StateEntry{Key: []byte(k), Revision: v.Revision, Value: v.Value, Tombstone: v.Tombstone})
	}
	if len(keys) > 0 {
		page.Next = []byte(keys[len(keys)-1])
	}
	return page, nil
}

func (s *memState) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	for _, c := range changes {
		if s.rows[rowKey{c.Area, string(c.Key)}].Revision != c.ExpectedRevision {
			return nil, errors.New("revision conflict")
		}
	}
	out := make([]uint64, len(changes))
	for i, c := range changes {
		k := rowKey{c.Area, string(c.Key)}
		v := s.rows[k]
		v.Revision++
		v.Value = append([]byte(nil), c.Value...)
		v.Tombstone = len(c.Value) == 0
		s.rows[k] = v
		out[i] = v.Revision
	}
	return out, nil
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	ctx := context.Background()
	s := &memState{binding: native.Binding{Instance: fx(0xaa), Community: fx(0xc1), Installation: fx(0xb1)}, rows: map[rowKey]native.StateValue{}}
	community := s.binding.Community
	t0 := time.Unix(1_700_000_000, 0).UTC()

	// Reminders: one far in the future, and one that is already due when v2 first starts (1_700_000_200).
	rs := reminders.NewStore(s, s.binding, func() time.Time { return t0 })
	_, err := rs.Create(ctx, fx(0x10), fx(0x20), [32]byte{1}, reminders.Reminder{Text: "water the plants", Timezone: "Europe/Stockholm"}, time.Unix(4_000_000_000, 0))
	must(err)
	_, err = rs.Create(ctx, fx(0x12), fx(0x22), [32]byte{4}, reminders.Reminder{Text: "stretch", Timezone: "UTC"}, time.Unix(1_700_000_100, 0))
	must(err)

	// A published poll and one vote.
	ps := polls.NewStore(s, community)
	poll, err := ps.Create(ctx, fx(0x30), [32]byte{2}, polls.Poll{ID: fx(0x30), CommunityID: community, ChannelID: fx(0x40), CreatorID: fx(0x20),
		Question: "Lunch?", Answers: []string{"Soup", "Salad", "Pie"}, CreatedAt: 1_700_000_000, DueAt: 1_700_604_800, Published: true}, t0)
	must(err)
	_, err = ps.Vote(ctx, poll.ID, fx(0x21), fx(0x31), [32]byte{3}, []uint64{1}, poll.Revision, t0.Add(100*time.Second))
	must(err)

	// The music queue: two tracks, paused.
	qs, err := persistence.New(s, community)
	must(err)
	queue := music.InitialState()
	queue.Revision = 1
	queue.Queue = []music.Track{
		{ID: "q1", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Sea shanties", DurationMS: 214_000, RequesterID: fx(0x20).String()},
		{ID: "q2", Provider: music.ProviderYouTube, CanonicalID: "12345678901", Title: "Night drive", DurationMS: 180_000, RequesterID: fx(0x21).String()},
	}
	_, err = qs.Save(ctx, fx(0x50), 0, queue)
	must(err)

	// The automatic moderation settings, and one piece of evidence.
	must(moderation.NewStore(s).SaveWithID(ctx, fx(0x60), moderation.Config{Enabled: true,
		Actions: map[moderation.Rule]moderation.Action{moderation.RuleRate: moderation.ActionLog, moderation.RuleDuplicate: moderation.ActionLog, moderation.RuleMentions: moderation.ActionLog},
		Timeout: 10 * time.Minute}, 0))
	must(moderation.NewEvidenceWriter(s).Save(ctx, moderation.EvidenceInput{OperationID: fx(0x70), MessageID: fx(0x71), Action: moderation.ActionWarn,
		Rule: moderation.RuleMentions, Reason: "too many mentions", Excerpt: "@a @b @c", CreatedAt: time.Unix(1_794_089_081, 0)}))

	type row struct {
		Area     uint64 `json:"area"`
		Key      string `json:"key"`
		Revision uint64 `json:"revision"`
		Value    string `json:"value"`
	}
	var out []row
	for k, v := range s.rows {
		out = append(out, row{Area: k.area, Key: base64.StdEncoding.EncodeToString([]byte(k.key)), Revision: v.Revision, Value: base64.StdEncoding.EncodeToString(v.Value)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Area != out[j].Area {
			return out[i].Area < out[j].Area
		}
		return out[i].Key < out[j].Key
	})
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	must(enc.Encode(out))
}
