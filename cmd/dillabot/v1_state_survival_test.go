//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/moderation"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/polls"
	"github.com/dilla-chat/dilla/internal/bot/reminders"
	"github.com/dilla-chat/dilla/internal/id"
)

// G7 (Bot v2 Task 17): an upgrade keeps the community's polls, reminders, settings, moderation evidence and music queue.
// testdata/v1-state/rows.json holds the rows the v1 Bot's stores wrote. testdata/v1-state/generate.sh regenerates it:
// it runs generate.go against the stores' own code at tag v0.1.0-signal2.1 (the last v1 release) with fixed ids and
// clocks (fix round 2, M9). The test starts v2's stores on exactly those rows: each reads back what v1 wrote, through
// its own reader, and what v2 does on start (the reminders tick, the poll deadline pass, every read) rewrites nothing
// but the one reminder that has come due, whose due transition is its only write.

func fx(n byte) (v id.ID) { v[15] = n; return }

type fixtureRow struct {
	Area     uint64 `json:"area"`
	Key      string `json:"key"`
	Revision uint64 `json:"revision"`
	Value    string `json:"value"`
}

type fixtureKey struct {
	area uint64
	key  string
}

// fixtureState is the helper's encrypted state area as a map; it applies the writes it is asked for (with the helper's
// revision check) and records which rows they touched.
type fixtureState struct {
	mu      sync.Mutex
	rows    map[fixtureKey]native.StateValue
	binding native.Binding
	writes  int
	written map[fixtureKey]bool
}

func loadV1State(t *testing.T) *fixtureState {
	t.Helper()
	raw, err := os.ReadFile("testdata/v1-state/rows.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []fixtureRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	s := &fixtureState{rows: map[fixtureKey]native.StateValue{}, binding: native.Binding{Instance: fx(0xaa), Community: fx(0xc1), Installation: fx(0xb1)}}
	for _, r := range rows {
		key, errKey := base64.StdEncoding.DecodeString(r.Key)
		value, errValue := base64.StdEncoding.DecodeString(r.Value)
		if errKey != nil || errValue != nil || r.Revision == 0 {
			t.Fatalf("fixture row %+v", r)
		}
		s.rows[fixtureKey{r.Area, string(key)}] = native.StateValue{Revision: r.Revision, Value: value}
	}
	return s
}

func (s *fixtureState) Binding() native.Binding { return s.binding }
func (s *fixtureState) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[fixtureKey{area, string(key)}], nil
}
func (s *fixtureState) StateScan(_ context.Context, area uint64, after []byte, limit uint64) (native.StatePage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
		v := s.rows[fixtureKey{area, k}]
		page.Entries = append(page.Entries, native.StateEntry{Key: []byte(k), Revision: v.Revision, Value: v.Value})
	}
	if len(keys) > 0 {
		page.Next = []byte(keys[len(keys)-1])
	}
	return page, nil
}
func (s *fixtureState) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	for _, c := range changes {
		if s.rows[fixtureKey{c.Area, string(c.Key)}].Revision != c.ExpectedRevision {
			return nil, persistence.ErrConflict
		}
	}
	if s.written == nil {
		s.written = map[fixtureKey]bool{}
	}
	out := make([]uint64, len(changes))
	for i, c := range changes {
		k := fixtureKey{c.Area, string(c.Key)}
		v := s.rows[k]
		v.Revision++
		v.Value = bytes.Clone(c.Value)
		s.rows[k] = v
		s.written[k] = true
		out[i] = v.Revision
	}
	return out, nil
}

// evidenceViewer is an actor holding every permission, view_audit_log included.
type evidenceViewer struct{}

func (evidenceViewer) BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error) {
	return client.ActorAuthority{EffectiveBits: ^uint64(0)}, true, nil
}

func (s *fixtureState) snapshot() map[fixtureKey]native.StateValue {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[fixtureKey]native.StateValue, len(s.rows))
	for k, v := range s.rows {
		v.Value = bytes.Clone(v.Value)
		out[k] = v
	}
	return out
}

func TestPollsRemindersSettingsEvidenceAndQueueSurviveUpgrade(t *testing.T) {
	ctx := context.Background()
	state := loadV1State(t)
	before := state.snapshot()
	binding := state.binding
	if len(before) != 14 {
		t.Fatalf("the fixture holds %d rows, want the 14 v1 wrote", len(before))
	}
	before1 := time.Unix(1_700_000_200, 0).UTC() // v1's clock when it wrote them, a little later

	// Reminders: the reminder is there, still scheduled, with its text, owner, zone and due time.
	rs := reminders.NewStore(state, binding, func() time.Time { return before1 })
	reminder, err := rs.Get(ctx, fx(0x10), fx(0x20))
	if err != nil {
		t.Fatalf("v1 reminder: %v", err)
	}
	if reminder.Text != "water the plants" || reminder.Timezone != "Europe/Stockholm" || reminder.DueAt != 4_000_000_000 ||
		reminder.State != reminders.StateScheduled || reminder.OwnerID != fx(0x20) || reminder.CommunityID != binding.Community {
		t.Fatalf("v1 reminder read back as %+v", reminder)
	}
	if list, _, err := rs.List(ctx, fx(0x20), false, "", before1); err != nil || len(list) != 1 || list[0].ID != fx(0x10) {
		t.Fatalf("v1 reminder list = %+v %v", list, err)
	}

	// Polls: the question, the answers and the vote.
	ps := polls.NewStore(state, binding.Community)
	poll, err := ps.Get(ctx, fx(0x30))
	if err != nil {
		t.Fatalf("v1 poll: %v", err)
	}
	if poll.Question != "Lunch?" || !slices.Equal(poll.Answers, []string{"Soup", "Salad", "Pie"}) || poll.ChannelID != fx(0x40) || poll.CreatorID != fx(0x20) || !poll.Published {
		t.Fatalf("v1 poll read back as %+v", poll)
	}
	counts, mine, err := ps.Counts(ctx, poll.ID, fx(0x21))
	if err != nil || !slices.Equal(counts, []uint64{0, 1, 0}) || !slices.Equal(mine, []uint64{1}) {
		t.Fatalf("v1 votes = %v mine %v (%v)", counts, mine, err)
	}

	// The music queue: both tracks, in order, restored paused.
	qs, err := persistence.New(state, binding.Community)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := qs.Load(ctx, fx(0x50))
	if err != nil || queue.Revision != 1 || len(queue.State.Queue) != 2 || queue.State.Queue[0].Title != "Sea shanties" || queue.State.Queue[1].Title != "Night drive" || !queue.State.Paused {
		t.Fatalf("v1 queue read back as %+v (%v)", queue, err)
	}

	// The automatic moderation configuration v1 saved is no longer read: Bot v2 Task 23 removed AutoModeration, and the
	// helper's one-time cleanup deletes the rows.

	// Moderation evidence, read back through v2's reader, on a clock inside its 90-day retention window.
	created := time.Unix(1_794_089_081, 0).UTC()
	reader := moderation.NewEvidenceReader(state, evidenceViewer{}).WithClock(func() time.Time { return created.Add(time.Hour) })
	item, err := reader.Read(ctx, fx(0x20), fx(0x70))
	if err != nil || item.Action != moderation.ActionWarn || item.Rule != moderation.RuleMentions || item.Reason != "too many mentions" ||
		item.Excerpt != "@a @b @c" || item.MessageID != fx(0x71) || !item.CreatedAt.Equal(created) {
		t.Fatalf("v1 evidence read back as %+v (%v)", item, err)
	}

	// The reminder v1 scheduled that has come due by v2's first start.
	due, err := rs.Get(ctx, fx(0x12), fx(0x22))
	if err != nil || due.Text != "stretch" || due.DueAt != 1_700_000_100 || due.State != reminders.StateScheduled {
		t.Fatalf("v1 due reminder read back as %+v (%v)", due, err)
	}

	// What v2 does on start with this state: the reminders tick and the poll deadline pass.
	if err := rs.Tick(ctx); err != nil {
		t.Fatalf("v2 reminders tick over v1 rows: %v", err)
	}
	if err := ps.Reconcile(ctx, before1, noPollPublisher{}); err != nil {
		t.Fatalf("v2 poll pass over v1 rows: %v", err)
	}

	// The only write is the due reminder's own transition; every other row is byte for byte what v1 left.
	dueKey := fixtureKey{8, "dilla.bot.reminder.v1/" + fx(0x12).String()}
	if state.writes != 1 || len(state.written) != 1 || !state.written[dueKey] {
		t.Fatalf("v2 wrote %d transactions to %v, want only the due reminder's transition", state.writes, state.written)
	}
	if fired, err := rs.Get(ctx, fx(0x12), fx(0x22)); err != nil || fired.State != reminders.StateDue || fired.Text != "stretch" {
		t.Fatalf("the due reminder after v2's tick = %+v (%v), want due", fired, err)
	}
	after := state.snapshot()
	if len(after) != len(before) {
		t.Fatalf("%d rows after, %d before", len(after), len(before))
	}
	for k, v := range before {
		if k == dueKey {
			continue
		}
		if got := after[k]; got.Revision != v.Revision || !bytes.Equal(got.Value, v.Value) {
			t.Fatalf("row %q (area %d) changed", k.key, k.area)
		}
	}
	// A second tick is quiet: the transition is not repeated.
	if err := rs.Tick(ctx); err != nil || state.writes != 1 {
		t.Fatalf("a second tick wrote again (%d transactions, %v)", state.writes, err)
	}
}

// noPollPublisher publishes nothing: every v1 poll in the fixture is already published.
type noPollPublisher struct{}

func (noPollPublisher) PublishPoll(context.Context, polls.Poll) error {
	panic("v2 tried to publish a poll v1 had already published")
}
