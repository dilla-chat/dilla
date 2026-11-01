package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	helperpkg "github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

func TestMusicAnnounceRegistersPausedDestinationBeforeStart(t *testing.T) {
	actor := id.New()
	f := newMusicAnnounceFixture(actor, true)
	notices := &MusicSkipNotices{}
	h := MusicAnnouncements{Music: &MusicInteractions{Store: f.store, Authority: f.authority}, Authority: f.member, Channels: f.member, Destination: &rolePanelDestinationFake{group: f.group}, Sender: f.sender, Discovery: &rolePanelDiscoveryFake{generation: 7}, Session: &client.Session{}, CommunityID: f.community, Notices: notices}
	zero, expires := uint64(0), uint64(130)
	request := protocol.Request{RequestID: f.request, CommunityID: f.community, ChannelID: &f.voice, Operation: protocol.OpMusicAnnounce, Revision: &zero, Expires: &expires, Args: []any{f.text}}
	result, err := h.announce(context.Background(), InteractionActor{User: actor}, request)
	if err != nil || result.Status != 0 || f.sender.calls != 0 {
		t.Fatalf("registration=%+v err=%v sends=%d", result, err, f.sender.calls)
	}
	route, ok := notices.destinations[f.voice]
	if !ok || route.text != f.text || route.actor.User != actor {
		t.Fatalf("route=%+v known=%v", route, ok)
	}
}

func TestMusicAnnounceRefusedRequestCannotChangeNoticeDestination(t *testing.T) {
	actor := id.New()
	f := newMusicAnnounceFixture(actor, false)
	notices := &MusicSkipNotices{}
	previous := id.New()
	notices.Register(f.voice, previous, InteractionActor{User: actor})
	h := MusicAnnouncements{Music: &MusicInteractions{Store: f.store, Authority: f.authority}, Authority: f.member, Channels: f.member, Destination: &rolePanelDestinationFake{group: f.group}, Sender: f.sender, Discovery: &rolePanelDiscoveryFake{generation: 7}, Session: &client.Session{}, CommunityID: f.community, Notices: notices}
	zero, expires := uint64(0), uint64(130)
	request := protocol.Request{RequestID: f.request, CommunityID: f.community, ChannelID: &f.voice, Operation: protocol.OpMusicAnnounce, Revision: &zero, Expires: &expires, Args: []any{f.text}}
	result, err := h.announce(context.Background(), InteractionActor{User: id.New()}, request)
	if err != nil || result.Status != interactionStatusForbidden {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if got := notices.destinations[f.voice].text; got != previous {
		t.Fatalf("refusal replaced destination: %s", got)
	}
}

type musicNoticeSender struct {
	community id.ID
	inputs    chan SendInput
	attempts  chan SendInput
	before    func(context.Context) error
	failures  int
	mu        sync.Mutex
}

func (s *musicNoticeSender) Send(context.Context, *client.Session, SendInput) (protocol.SendReceipt, error) {
	return protocol.SendReceipt{}, errors.New("unauthorized send path")
}
func (s *musicNoticeSender) SendAuthorized(ctx context.Context, _ SendSession, in SendInput, preflight func(context.Context) error) (protocol.SendReceipt, error) {
	if s.attempts != nil {
		s.attempts <- in
	}
	if s.before != nil {
		if err := s.before(ctx); err != nil {
			return protocol.SendReceipt{}, err
		}
	}
	if err := preflight(ctx); err != nil {
		return protocol.SendReceipt{}, err
	}
	s.inputs <- in
	s.mu.Lock()
	fail := s.failures > 0
	if fail {
		s.failures--
	}
	s.mu.Unlock()
	if fail {
		return protocol.SendReceipt{}, context.DeadlineExceeded
	}
	return protocol.SendReceipt{OperationID: in.OperationID, AttemptID: in.AttemptID, GroupID: in.GroupID, CommunityID: s.community, Outcome: protocol.ReceiptAccepted}, nil
}
func noticeFixture(t *testing.T) (*MusicSkipNotices, *musicAnnounceFixture, *musicNoticeSender) {
	return noticeFixtureWithClock(t, nil)
}
func noticeFixtureWithClock(t *testing.T, clock func() time.Time) (*MusicSkipNotices, *musicAnnounceFixture, *musicNoticeSender) {
	t.Helper()
	f := newMusicAnnounceFixture(id.New(), true)
	sender := &musicNoticeSender{community: f.community, inputs: make(chan SendInput, 100)}
	n := &MusicSkipNotices{}
	n.Configure(MusicAnnouncements{Music: &MusicInteractions{Authority: f.authority}, Channels: f.member, Destination: &rolePanelDestinationFake{group: f.group}, Sender: sender, Discovery: &rolePanelDiscoveryFake{generation: 7}, Session: &client.Session{}, CommunityID: f.community, Now: clock}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	n.Register(f.voice, f.text, InteractionActor{User: f.actor})
	t.Cleanup(n.Close)
	return n, f, sender
}
func noticeSkips(items ...string) []music.SkippedTrack {
	result := make([]music.SkippedTrack, 0, len(items))
	for _, item := range items {
		result = append(result, music.SkippedTrack{Track: music.Track{ID: item, Title: "private-title", RequesterID: "private-requester", CanonicalID: "private-url"}, Failure: &music.StartError{Stage: "provider_check", Cause: errors.New("private cause")}})
	}
	return result
}
func nextNotice(t *testing.T, s *musicNoticeSender) SendInput {
	t.Helper()
	select {
	case in := <-s.inputs:
		return in
	case <-time.After(time.Second):
		t.Fatal("notice worker did not send")
		return SendInput{}
	}
}
func TestMusicSkipNoticesCoalesceAndNeverPublishQueueMetadata(t *testing.T) {
	n, f, s := noticeFixture(t)
	n.Notify(context.Background(), f.voice, noticeSkips("one", "two", "three"))
	in := nextNotice(t, s)
	var body []any
	if err := cborx.Unmarshal(in.RequestBody, &body); err != nil {
		t.Fatal(err)
	}
	if got := body[2]; got != "Couldn't play 3 tracks. Skipping." {
		t.Fatalf("public notice=%q", got)
	}
	n.Notify(context.Background(), f.voice, noticeSkips("one", "two", "three"))
	n.Close()
	select {
	case extra := <-s.inputs:
		t.Fatalf("batch repeated: %+v", extra)
	default:
	}
}
func TestMusicSkipNoticesBoundedAsyncQueueAndClose(t *testing.T) {
	n, f, s := noticeFixture(t)
	entered := make(chan struct{})
	s.before = func(ctx context.Context) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	n.Notify(context.Background(), f.voice, noticeSkips("first"))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker not running")
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			n.Notify(context.Background(), f.voice, noticeSkips(id.New().String()))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Notify waited for delivery")
	}
	n.Close()
}
func TestMusicSkipNoticesRecheckAuthorityAtSendBoundary(t *testing.T) {
	n, f, s := noticeFixture(t)
	checked := make(chan struct{})
	s.before = func(context.Context) error { n.Clear(f.voice); close(checked); return nil }
	n.Notify(context.Background(), f.voice, noticeSkips("revoked"))
	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("send boundary never reached")
	}
	n.Close()
	select {
	case <-s.inputs:
		t.Fatal("revoked destination was sent")
	default:
	}
}
func TestMusicSkipNoticeRetryKeepsImmutableInput(t *testing.T) {
	n, f, s := noticeFixture(t)
	// Exercise the same worker job twice: the first send times out after preparing.
	s.failures = 1
	job := musicNoticeJob{Voice: f.voice, Destination: noticeRoute(n, f.voice), Items: []string{"a"}, Reason: "provider unavailable"}
	if err := n.send(context.Background(), &job); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first send=%v", err)
	}
	first := nextNotice(t, s)
	if err := n.send(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	second := nextNotice(t, s)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("retry changed immutable send input:\n%+v\n%+v", first, second)
	}
}
func TestMusicSkipNoticeIDsDeduplicateBatchOrderAndScope(t *testing.T) {
	community, voice := id.New(), id.New()
	a, b := musicSkipNoticeIDs(community, voice, []string{"a", "b"})
	c, d := musicSkipNoticeIDs(community, voice, []string{"b", "a", "a"})
	if a != c || b != d {
		t.Fatal("batch order or duplicate item changed notice identity")
	}
	other, _ := musicSkipNoticeIDs(id.New(), voice, []string{"a", "b"})
	if a == other {
		t.Fatal("notice crossed community scope")
	}
}
func TestMusicSkipNoticesRateLimitIsPerVoice(t *testing.T) {
	n, f, s := noticeFixture(t)
	n.Notify(context.Background(), f.voice, noticeSkips("a"))
	nextNotice(t, s)
	n.Notify(context.Background(), f.voice, noticeSkips("b"))
	other := id.New()
	n.Register(other, f.text, InteractionActor{User: f.actor})
	n.Notify(context.Background(), other, noticeSkips("c"))
	nextNotice(t, s)
	n.Close()
	select {
	case extra := <-s.inputs:
		t.Fatalf("more than one notice per voice in ten seconds: %+v", extra)
	default:
	}
}

type musicNoticeState struct {
	mu      sync.Mutex
	binding native.Binding
	value   native.StateValue
	rows    map[string]native.StateValue
}

func (s *musicNoticeState) Binding() native.Binding { return s.binding }
func (s *musicNoticeState) StateGet(_ context.Context, _ uint64, key []byte) (native.StateValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.rows[string(key)]
	v.Value = append([]byte(nil), v.Value...)
	return v, nil
}
func (s *musicNoticeState) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = make(map[string]native.StateValue)
	}
	for _, change := range changes {
		if change.Area != 8 || change.ExpectedRevision != s.rows[string(change.Key)].Revision {
			return nil, errors.New("journal state conflict")
		}
		if err := cborx.ExpectMajor(change.Value, cborx.MajorArray); err != nil {
			return nil, err
		}
		if err := cborx.CheckDeterministic(change.Value); err != nil {
			return nil, err
		}
		if len(change.Value) > 256*1024 {
			return nil, errors.New("state value exceeds limit")
		}
	}
	revisions := make([]uint64, 0, len(changes))
	for _, change := range changes {
		v := native.StateValue{Revision: s.rows[string(change.Key)].Revision + 1, Value: append([]byte(nil), change.Value...)}
		s.rows[string(change.Key)] = v
		revisions = append(revisions, v.Revision)
		if bytes.HasPrefix(change.Key, []byte("dilla.bot.music.skip-notices.v1/")) {
			s.value = v
		}
	}
	return revisions, nil
}
func TestMusicSkipNoticeJournalResumesExactPreparedInputAfterRestart(t *testing.T) {
	f := newMusicAnnounceFixture(id.New(), true)
	state := &musicNoticeState{binding: native.Binding{Instance: id.New(), Community: f.community, Installation: id.New()}}
	firstSender := &musicNoticeSender{community: f.community, inputs: make(chan SendInput, 10), failures: 1}
	h := MusicAnnouncements{Music: &MusicInteractions{Authority: f.authority}, Channels: f.member, Destination: &rolePanelDestinationFake{group: f.group}, Sender: firstSender, Discovery: &rolePanelDiscoveryFake{generation: 7}, Session: &client.Session{}, CommunityID: f.community}
	n := &MusicSkipNotices{}
	n.SetState(state)
	n.Configure(h, nil)
	n.Register(f.voice, f.text, InteractionActor{User: f.actor})
	n.Notify(context.Background(), f.voice, noticeSkips("a", "b"))
	first := nextNotice(t, firstSender)
	n.Close()
	state.mu.Lock()
	saved := append([]byte(nil), state.value.Value...)
	state.mu.Unlock()
	if len(saved) == 0 {
		t.Fatal("attempt was not journaled before sending")
	}
	secondSender := &musicNoticeSender{community: f.community, inputs: make(chan SendInput, 10), attempts: make(chan SendInput, 10)}
	h.Sender = secondSender
	h.Discovery = &rolePanelDiscoveryFake{generation: 99}
	h.Destination = &rolePanelDestinationFake{group: id.New()}
	restarted := &MusicSkipNotices{}
	restarted.SetState(state)
	restarted.Configure(h, nil)
	select {
	case second := <-secondSender.attempts:
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("restart replaced immutable input:\n%+v\n%+v", first, second)
		}
	case <-time.After(time.Second):
		t.Fatal("prepared attempt not resumed")
	}
	restarted.Close()
	select {
	case <-secondSender.inputs:
		t.Fatal("restart silently restored stale registration authority")
	default:
	}
}
func TestMusicSkipNoticesNormalReconfigurePreservesRegistrationAndRate(t *testing.T) {
	n, f, s := noticeFixture(t)
	n.Notify(context.Background(), f.voice, noticeSkips("a"))
	nextNotice(t, s)
	h := MusicAnnouncements{Music: &MusicInteractions{Authority: f.authority}, Channels: f.member, Destination: &rolePanelDestinationFake{group: f.group}, Sender: s, Discovery: &rolePanelDiscoveryFake{generation: 7}, Session: &client.Session{}, CommunityID: f.community}
	n.Configure(h, nil)
	n.mu.Lock()
	route, known := n.destinations[f.voice]
	last := n.last[f.voice]
	n.mu.Unlock()
	if !known || route.text != f.text || last.IsZero() {
		t.Fatal("normal reconcile erased destination or rate limit")
	}
	n.Notify(context.Background(), f.voice, noticeSkips("b"))
	n.Close()
	select {
	case <-s.inputs:
		t.Fatal("normal reconcile reset the ten-second limiter")
	default:
	}
	n.Configure(h, nil)
	n.mu.Lock()
	_, known = n.destinations[f.voice]
	n.mu.Unlock()
	if known {
		t.Fatal("explicit Close preserved stale authority")
	}
}

type musicNoticeCoordinatorSender struct {
	coordinator *SendCoordinator
	session     SendSession
}

func (s musicNoticeCoordinatorSender) Send(context.Context, *client.Session, SendInput) (protocol.SendReceipt, error) {
	return protocol.SendReceipt{}, errors.New("wrong send path")
}
func (s musicNoticeCoordinatorSender) SendAuthorized(ctx context.Context, _ SendSession, in SendInput, preflight func(context.Context) error) (protocol.SendReceipt, error) {
	return s.coordinator.SendAuthorized(ctx, s.session, in, preflight)
}

type musicNoticeRevokeNative struct {
	*sendNativeFake
	revoke func()
}

func (n musicNoticeRevokeNative) PrepareSend(ctx context.Context, operation, attempt, group id.ID, body []byte, now, generation uint64) (native.PreparedSend, error) {
	n.revoke()
	return n.sendNativeFake.PrepareSend(ctx, operation, attempt, group, body, now, generation)
}
func TestMusicSkipNoticeCoordinatorRechecksRevocationAfterPrepare(t *testing.T) {
	coordinator, helper, session, _ := sendFixture()
	f := newMusicAnnounceFixture(id.New(), true)
	f.community = helper.binding.Community
	f.group = helper.group.ID
	n := &MusicSkipNotices{}
	coordinator.native = musicNoticeRevokeNative{sendNativeFake: helper, revoke: func() { delete(f.member.channel, f.text) }}
	sender := musicNoticeCoordinatorSender{coordinator: coordinator, session: session}
	h := MusicAnnouncements{Music: &MusicInteractions{Authority: f.authority}, Channels: f.member, Destination: &rolePanelDestinationFake{group: f.group}, Sender: sender, Discovery: &rolePanelDiscoveryFake{generation: 12}, Session: &client.Session{}, CommunityID: f.community}
	n.Configure(h, nil)
	defer n.Close()
	n.Register(f.voice, f.text, InteractionActor{User: f.actor})
	job := musicNoticeJob{Voice: f.voice, Destination: noticeRoute(n, f.voice), Items: []string{"private-item"}, Reason: "provider unavailable"}
	operation, attempt := musicSkipNoticeIDs(f.community, f.voice, job.Items)
	closed := sendReceipt(operation, attempt, f.group, 12, helper.prepared.Epoch, protocol.ReceiptClosedNotAccepted)
	session.resolveReceipt = &closed
	err := n.send(context.Background(), &job)
	if !errors.Is(err, ErrSendTerminal) || helper.prepareCalls != 2 || session.posts != 0 || session.resolves != 1 || helper.confirmCalls != 1 {
		t.Fatalf("revocation after prepare: error=%v prepares=%d uploads=%d resolves=%d confirms=%d", err, helper.prepareCalls, session.posts, session.resolves, helper.confirmCalls)
	}
}

func TestMusicSkipNoticesDeduplicateItemsAcrossOverlappingBatches(t *testing.T) {
	var instant atomic.Int64
	instant.Store(time.Now().UnixNano())
	n, f, s := noticeFixtureWithClock(t, func() time.Time { return time.Unix(0, instant.Load()) })
	n.Notify(context.Background(), f.voice, noticeSkips("a", "b"))
	nextNotice(t, s)
	instant.Add(int64(11 * time.Second))
	n.Notify(context.Background(), f.voice, noticeSkips("b", "c"))
	in := nextNotice(t, s)
	var body []any
	if err := cborx.Unmarshal(in.RequestBody, &body); err != nil {
		t.Fatal(err)
	}
	if got := body[2]; got != "Couldn't play a track. Skipping." {
		t.Fatalf("already-notified item repeated in overlapping batch: %q", got)
	}
	n.Notify(context.Background(), f.voice, noticeSkips("c", "a", "b"))
	n.Close()
	select {
	case extra := <-s.inputs:
		t.Fatalf("individual item repeated: %+v", extra)
	default:
	}
}

func TestMusicSkipNoticesBoundDedupeAndQueuedState(t *testing.T) {
	n, f, s := noticeFixture(t)
	entered := make(chan struct{})
	s.before = func(ctx context.Context) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	n.Notify(context.Background(), f.voice, noticeSkips("first"))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker not running")
	}
	for i := 0; i < 3000; i++ {
		n.Notify(context.Background(), f.voice, noticeSkips(id.New().String()))
	}
	n.mu.Lock()
	seen, queued := len(n.seen), len(n.jobs)
	n.mu.Unlock()
	if seen > 1024 || queued > 32 {
		t.Fatalf("unbounded notice state: dedupe=%d queued=%d", seen, queued)
	}
	n.Close()
}

func TestMusicSkipNoticeDurableDedupeSurvivesColdCache(t *testing.T) {
	f := newMusicAnnounceFixture(id.New(), true)
	state := &musicNoticeState{binding: native.Binding{Instance: id.New(), Community: f.community, Installation: id.New()}}
	sender := &musicNoticeSender{community: f.community, inputs: make(chan SendInput, 10)}
	h := MusicAnnouncements{Music: &MusicInteractions{Authority: f.authority}, Channels: f.member, Destination: &rolePanelDestinationFake{group: f.group}, Sender: sender, Discovery: &rolePanelDiscoveryFake{generation: 7}, Session: &client.Session{}, CommunityID: f.community}
	n := &MusicSkipNotices{}
	n.SetState(state)
	n.Configure(h, nil)
	n.Register(f.voice, f.text, InteractionActor{User: f.actor})
	job := musicNoticeJob{Voice: f.voice, Destination: noticeRoute(n, f.voice), Items: []string{"a", "b"}, Reason: "provider unavailable"}
	if err := n.send(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	nextNotice(t, sender)
	if err := n.persist(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	n.Close()
	restarted := &MusicSkipNotices{}
	restarted.SetState(state)
	restarted.Configure(h, nil)
	defer restarted.Close()
	restarted.Register(f.voice, f.text, InteractionActor{User: f.actor})
	restarted.Notify(context.Background(), f.voice, noticeSkips("b", "c"))
	in := nextNotice(t, sender)
	var body []any
	if err := cborx.Unmarshal(in.RequestBody, &body); err != nil {
		t.Fatal(err)
	}
	if got := body[2]; got != "Couldn't play a track. Skipping." {
		t.Fatalf("cold-cache overlapping batch repeated reserved item: %q", got)
	}
}
func noticeRoute(n *MusicSkipNotices, voice id.ID) musicNoticeDestination {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.destinations[voice]
}
func TestMusicSkipNoticeClearRevokesOldGrantEvenAfterReregister(t *testing.T) {
	n, f, s := noticeFixture(t)
	old := noticeRoute(n, f.voice)
	n.Clear(f.voice)
	n.Register(f.voice, f.text, InteractionActor{User: f.actor})
	job := musicNoticeJob{Voice: f.voice, Destination: old, Items: []string{"old-call-item"}, Reason: "provider unavailable"}
	if err := n.send(context.Background(), &job); err == nil {
		t.Fatal("cleared registration was resurrected by a later call")
	}
	select {
	case <-s.inputs:
		t.Fatal("old call's notice uploaded under a later registration")
	default:
	}
}
func TestMusicSkipNoticesConcurrentConfigureAndClose(t *testing.T) {
	n, f, s := noticeFixture(t)
	h := MusicAnnouncements{Music: &MusicInteractions{Authority: f.authority}, Channels: f.member, Destination: &rolePanelDestinationFake{group: f.group}, Sender: s, Discovery: &rolePanelDiscoveryFake{generation: 7}, Session: &client.Session{}, CommunityID: f.community}
	var group sync.WaitGroup
	for i := 0; i < 10; i++ {
		group.Add(2)
		go func() { defer group.Done(); n.Configure(h, nil) }()
		go func() { defer group.Done(); n.Close() }()
	}
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("notice lifecycle deadlocked")
	}
	n.Close()
}

func TestMusicSkipNoticesFullRateCacheRetainsUnexpiredVoices(t *testing.T) {
	n, f, s := noticeFixture(t)
	now := time.Now()
	n.mu.Lock()
	n.last[f.voice] = now.Add(-time.Second)
	for i := 1; i < 128; i++ {
		n.last[id.New()] = now
	}
	n.mu.Unlock()
	extra := id.New()
	n.Register(extra, f.text, InteractionActor{User: f.actor})
	n.Notify(context.Background(), extra, noticeSkips("new-voice"))
	deadline := time.Now().Add(time.Second)
	for {
		n.mu.Lock()
		queued := len(n.jobs)
		n.mu.Unlock()
		if queued == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("full rate cache blocked worker")
		}
		time.Sleep(time.Millisecond)
	}
	n.mu.Lock()
	_, retained := n.last[f.voice]
	count := len(n.last)
	n.mu.Unlock()
	if !retained || count > 128 {
		t.Fatalf("unexpired rate entry evicted: retained=%v count=%d", retained, count)
	}
	n.Notify(context.Background(), f.voice, noticeSkips("same-voice-again"))
	n.Close()
	select {
	case <-s.inputs:
		t.Fatal("rate-cache overflow posted a fresh notice")
	default:
	}
}

type musicNoticeLostAckState struct {
	*musicNoticeState
	guard       sync.Mutex
	transaction id.ID
	changes     []native.StateChange
	revisions   []uint64
	committed   chan struct{}
	release     chan struct{}
	failRead    bool
	replayed    bool
}

func (s *musicNoticeLostAckState) StateTransaction(ctx context.Context, transaction id.ID, changes []native.StateChange) ([]uint64, error) {
	s.guard.Lock()
	if s.transaction == transaction && !transaction.IsZero() {
		if !reflect.DeepEqual(changes, s.changes) {
			s.guard.Unlock()
			return nil, errors.New("immutable transaction changed")
		}
		s.replayed = true
		revisions := append([]uint64(nil), s.revisions...)
		s.guard.Unlock()
		return revisions, nil
	}
	first := s.transaction.IsZero()
	s.guard.Unlock()
	revisions, err := s.musicNoticeState.StateTransaction(ctx, transaction, changes)
	if err != nil || !first {
		return revisions, err
	}
	s.guard.Lock()
	s.transaction = transaction
	s.changes = changes
	s.revisions = revisions
	s.failRead = true
	s.guard.Unlock()
	close(s.committed)
	<-s.release
	return nil, context.DeadlineExceeded
}
func (s *musicNoticeLostAckState) StateGet(ctx context.Context, area uint64, key []byte) (native.StateValue, error) {
	s.guard.Lock()
	fail := s.failRead
	if fail {
		s.failRead = false
	}
	s.guard.Unlock()
	if fail {
		return native.StateValue{}, context.DeadlineExceeded
	}
	return s.musicNoticeState.StateGet(ctx, area, key)
}
func TestMusicSkipNoticeJournalRetriesExactLostAckBeforeNewSeenSnapshot(t *testing.T) {
	community, voice, text := id.New(), id.New(), id.New()
	state := &musicNoticeLostAckState{musicNoticeState: &musicNoticeState{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}}, committed: make(chan struct{}), release: make(chan struct{})}
	route := musicNoticeDestination{text: text, actor: InteractionActor{User: id.New()}, grant: id.New()}
	n := &MusicSkipNotices{state: state, journalKey: []byte("dilla.bot.music.skip-notices.v1/" + community.String()), seen: map[string]bool{}, destinations: map[id.ID]musicNoticeDestination{voice: route}, logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), send: func(context.Context, *musicNoticeJob) error { return nil }}
	n.rememberLocked(voice.String() + "/a")
	operation, attempt := musicSkipNoticeIDs(community, voice, []string{"a"})
	job := musicNoticeJob{Voice: voice, Destination: route, Items: []string{"a"}, Reason: "provider unavailable", Input: &SendInput{OperationID: operation, AttemptID: attempt, GroupID: id.New(), RequestBody: []byte{0x80}, OriginalNow: 100, ExpectedGeneration: 7}}
	result := make(chan error, 1)
	go func() { result <- n.persist(context.Background(), &job) }()
	select {
	case <-state.committed:
	case <-time.After(time.Second):
		t.Fatal("journal transaction did not commit")
	}
	n.Notify(context.Background(), voice, noticeSkips("concurrent-item"))
	close(state.release)
	if err := <-result; err == nil {
		t.Fatal("lost acknowledgement unexpectedly resolved despite failed readback")
	}
	if err := n.persist(context.Background(), &job); err != nil {
		t.Fatalf("lost acknowledgement trapped journal behind stale CAS: %v", err)
	}
	state.guard.Lock()
	replayed := state.replayed
	state.guard.Unlock()
	if !replayed {
		t.Fatal("journal replaced unresolved transaction with a different body")
	}
	state.mu.Lock()
	value := state.value
	state.mu.Unlock()
	var envelope []any
	if err := cborx.Unmarshal(value.Value, &envelope); err != nil {
		t.Fatal(err)
	}
	var journal musicNoticeJournal
	if err := json.Unmarshal(envelope[1].([]byte), &journal); err != nil {
		t.Fatal(err)
	}
	if len(journal.Pending) != 1 || len(journal.Seen) != 2 || value.Revision < 2 {
		t.Fatalf("journal failed to merge concurrent notification after acknowledgement: pending=%d seen=%d revision=%d", len(journal.Pending), len(journal.Seen), value.Revision)
	}
}
func TestMusicSkipNoticeRevokedPreparedAttemptClosesWithoutUpload(t *testing.T) {
	coordinator, helper, session, in := sendFixture()
	f := newMusicAnnounceFixture(id.New(), true)
	f.community = helper.binding.Community
	f.group = helper.group.ID
	closed := sendReceipt(in.OperationID, in.AttemptID, in.GroupID, in.ExpectedGeneration, helper.prepared.Epoch, protocol.ReceiptClosedNotAccepted)
	session.resolveReceipt = &closed
	n := &MusicSkipNotices{}
	sender := musicNoticeCoordinatorSender{coordinator: coordinator, session: session}
	h := MusicAnnouncements{Music: &MusicInteractions{Authority: f.authority}, Channels: f.member, Destination: &rolePanelDestinationFake{group: f.group}, Sender: sender, Discovery: &rolePanelDiscoveryFake{generation: 12}, Session: &client.Session{}, CommunityID: f.community}
	n.Configure(h, nil)
	defer n.Close()
	n.Register(f.voice, f.text, InteractionActor{User: f.actor})
	job := musicNoticeJob{Voice: f.voice, Destination: noticeRoute(n, f.voice), Items: []string{"a"}, Reason: "provider unavailable", Input: &in}
	n.Clear(f.voice)
	err := n.send(context.Background(), &job)
	if !errors.Is(err, ErrSendTerminal) || session.posts != 0 || session.resolves != 1 || helper.confirmCalls != 1 {
		t.Fatalf("revoked attempt did not terminate: error=%v uploads=%d resolves=%d confirms=%d", err, session.posts, session.resolves, helper.confirmCalls)
	}
}

func (s musicNoticeCoordinatorSender) ResolvePrepared(ctx context.Context, _ SendSession, in SendInput) (protocol.SendReceipt, error) {
	resolver, ok := any(s.coordinator).(sendPreparedResolver)
	if !ok {
		return protocol.SendReceipt{}, errors.New("resolve-only send unavailable")
	}
	return resolver.ResolvePrepared(ctx, s.session, in)
}
func (s *musicNoticeSender) ResolvePrepared(_ context.Context, _ SendSession, in SendInput) (protocol.SendReceipt, error) {
	if s.attempts != nil {
		s.attempts <- in
	}
	return protocol.SendReceipt{OperationID: in.OperationID, AttemptID: in.AttemptID, GroupID: in.GroupID, CommunityID: s.community, ExpectedGeneration: in.ExpectedGeneration, Outcome: protocol.ReceiptClosedNotAccepted}, nil
}

type musicNoticeCapacitySession struct {
	*sendSessionFake
	posted chan protocol.SendReceipt
}

func (s musicNoticeCapacitySession) PostBotMessage(_ context.Context, _ id.ID, group id.ID, upload client.BotMessageUpload) (protocol.SendReceipt, error) {
	receipt := sendReceipt(upload.OperationID, upload.AttemptID, group, upload.ExpectedGeneration, upload.Epoch, protocol.ReceiptAccepted)
	s.posts++
	s.receipts[upload.AttemptID] = receipt
	s.posted <- receipt
	return receipt, nil
}
func TestMusicSkipNoticesMissingPreparedCiphertextFreesWorkerCapacity(t *testing.T) {
	for _, status := range []uint64{4, 7} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			coordinator, helper, session, in := sendFixture()
			f := newMusicAnnounceFixture(id.New(), true)
			f.community = helper.binding.Community
			f.group = helper.group.ID
			blocked := make(map[id.ID]bool)
			failed := make(chan id.ID, 32)
			jobs := make([]*musicNoticeJob, 0, 32)
			for i := 0; i < 32; i++ {
				old := in
				old.OperationID = id.New()
				old.AttemptID = id.New()
				blocked[old.OperationID] = true
				jobs = append(jobs, &musicNoticeJob{Voice: id.New(), Destination: musicNoticeDestination{text: f.text, actor: InteractionActor{User: f.actor}, grant: id.New()}, Items: []string{id.New().String()}, Reason: "provider unavailable", Input: &old})
			}
			coordinator.native = unavailablePreparedNative{sendNativeFake: helper, failure: &helperpkg.StatusError{Status: status}, blocked: blocked, failed: failed}
			accepted := musicNoticeCapacitySession{sendSessionFake: session, posted: make(chan protocol.SendReceipt, 1)}
			sender := musicNoticeCoordinatorSender{coordinator: coordinator, session: accepted}
			n := &MusicSkipNotices{jobs: jobs}
			h := MusicAnnouncements{Music: &MusicInteractions{Authority: f.authority}, Channels: f.member, Destination: &rolePanelDestinationFake{group: f.group}, Sender: sender, Discovery: &rolePanelDiscoveryFake{generation: 12}, Session: &client.Session{}, CommunityID: f.community}
			n.Configure(h, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
			defer n.Close()
			for i := 0; i < 32; i++ {
				select {
				case <-failed:
				case <-time.After(time.Second):
					t.Fatal("old prepared record was not examined")
				}
			}
			n.Register(f.voice, f.text, InteractionActor{User: f.actor})
			n.Notify(context.Background(), f.voice, noticeSkips("fresh-item"))
			select {
			case <-accepted.posted:
			case <-time.After(time.Second):
				t.Fatal("never-prepared jobs permanently exhausted worker capacity")
			}
			n.Close()
			n.mu.Lock()
			pending := len(n.jobs)
			n.mu.Unlock()
			if pending != 0 || session.posts != 1 || session.resolves != 0 || helper.confirmCalls != 1 {
				t.Fatalf("old job was fabricated or retained: pending=%d uploads=%d resolves=%d confirms=%d", pending, session.posts, session.resolves, helper.confirmCalls)
			}
		})
	}
}

func TestMusicSkipNoticeResolveRefusalDropsQueueAndJournal(t *testing.T) {
	coordinator, helper, session, in := sendFixture()
	f := newMusicAnnounceFixture(id.New(), true)
	f.community = helper.binding.Community
	f.group = in.GroupID
	voice := id.New()
	in.OperationID, in.AttemptID = musicSkipNoticeIDs(f.community, voice, []string{"revoked"})
	job := &musicNoticeJob{Voice: voice, Destination: musicNoticeDestination{text: f.text, actor: InteractionActor{User: f.actor}, grant: id.New()}, Items: []string{"revoked"}, Input: &in}
	state := &musicNoticeState{binding: helper.binding}
	journalKey := []byte("dilla.bot.music.skip-notices.v1/" + f.community.String())
	seed := &MusicSkipNotices{state: state, journalKey: journalKey, jobs: []*musicNoticeJob{job}}
	if err := seed.persist(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	session.resolveErr = &client.APIError{Status: 404, Code: "E_NOT_FOUND", Outcome: client.OutcomeDefinitive}
	sender := musicNoticeCoordinatorSender{coordinator: coordinator, session: session}
	var logs bytes.Buffer
	n := &MusicSkipNotices{}
	n.SetState(state)
	h := MusicAnnouncements{Music: &MusicInteractions{Authority: f.authority}, Channels: f.member, Destination: &rolePanelDestinationFake{group: f.group}, Sender: sender, Discovery: &rolePanelDiscoveryFake{generation: 12}, Session: &client.Session{}, CommunityID: f.community}
	n.Configure(h, slog.New(slog.NewTextHandler(&logs, nil)))
	defer n.Close()
	deadline := time.Now().Add(time.Second)
	for {
		value, err := state.StateGet(t.Context(), 8, journalKey)
		if err != nil {
			t.Fatal(err)
		}
		var envelope []any
		if err := cborx.Unmarshal(value.Value, &envelope); err != nil {
			t.Fatal(err)
		}
		var journal musicNoticeJournal
		if err := json.Unmarshal(envelope[1].([]byte), &journal); err != nil {
			t.Fatal(err)
		}
		if len(journal.Pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("definitively refused prepared notice remains journaled")
		}
		time.Sleep(time.Millisecond)
	}
	n.Close()
	if len(n.jobs) != 0 || session.resolves != 1 || session.posts != 0 || helper.confirmCalls != 0 {
		t.Fatalf("refused notice retained or uploaded: queued=%d resolves=%d posts=%d confirms=%d", len(n.jobs), session.resolves, session.posts, helper.confirmCalls)
	}
	if !bytes.Contains(logs.Bytes(), []byte("send_terminal")) {
		t.Fatalf("terminal disposition missing from logs: %s", logs.String())
	}
}

type musicNoticeRetrySender struct {
	*musicNoticeSender
	entered chan struct{}
}

func (s musicNoticeRetrySender) ResolvePrepared(ctx context.Context, _ SendSession, _ SendInput) (protocol.SendReceipt, error) {
	s.entered <- struct{}{}
	<-ctx.Done()
	return protocol.SendReceipt{}, ctx.Err()
}

func TestMusicSkipNoticeRetryBudgetSurvivesEveryRestart(t *testing.T) {
	_, helper, _, in := sendFixture()
	f := newMusicAnnounceFixture(id.New(), true)
	f.community = helper.binding.Community
	voice := id.New()
	in.OperationID, in.AttemptID = musicSkipNoticeIDs(f.community, voice, []string{"uncertain"})
	job := &musicNoticeJob{Voice: voice, Destination: musicNoticeDestination{text: f.text, actor: InteractionActor{User: f.actor}, grant: id.New()}, Items: []string{"uncertain"}, Input: &in}
	state := &musicNoticeState{binding: helper.binding}
	journalKey := []byte("dilla.bot.music.skip-notices.v1/" + f.community.String())
	seed := &MusicSkipNotices{state: state, journalKey: journalKey, jobs: []*musicNoticeJob{job}}
	if err := seed.persist(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	sender := musicNoticeRetrySender{musicNoticeSender: &musicNoticeSender{}, entered: make(chan struct{}, 1)}
	h := MusicAnnouncements{Music: &MusicInteractions{Authority: f.authority}, Channels: f.member, Destination: &rolePanelDestinationFake{group: f.group}, Sender: sender, Discovery: &rolePanelDiscoveryFake{generation: 12}, Session: &client.Session{}, CommunityID: f.community}
	readJournal := func() musicNoticeJournal {
		t.Helper()
		value, err := state.StateGet(t.Context(), 8, journalKey)
		if err != nil {
			t.Fatal(err)
		}
		var envelope []any
		if err := cborx.Unmarshal(value.Value, &envelope); err != nil {
			t.Fatal(err)
		}
		var journal musicNoticeJournal
		if err := json.Unmarshal(envelope[1].([]byte), &journal); err != nil {
			t.Fatal(err)
		}
		return journal
	}
	for attempt := uint32(1); attempt <= 6; attempt++ {
		n := &MusicSkipNotices{}
		n.SetState(state)
		n.Configure(h, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
		select {
		case <-sender.entered:
		case <-time.After(time.Second):
			n.Close()
			t.Fatalf("attempt %d did not reach resolve", attempt)
		}
		journal := readJournal()
		n.Close()
		if len(journal.Pending) != 1 || journal.Pending[0].Attempts != attempt {
			t.Fatalf("attempt %d was not durably reserved before resolve: %+v", attempt, journal.Pending)
		}
	}
	var logs bytes.Buffer
	exhausted := &MusicSkipNotices{}
	exhausted.SetState(state)
	exhausted.Configure(h, slog.New(slog.NewTextHandler(&logs, nil)))
	defer exhausted.Close()
	deadline := time.Now().Add(time.Second)
	for len(readJournal().Pending) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("exhausted notice retained after restart")
		}
		time.Sleep(time.Millisecond)
	}
	exhausted.Close()
	select {
	case <-sender.entered:
		t.Fatal("restart reset the prepared notice retry budget")
	default:
	}
	if len(exhausted.jobs) != 0 || !bytes.Contains(logs.Bytes(), []byte("retry_exhausted")) {
		t.Fatalf("exhausted notice not evicted/logged: jobs=%d log=%s", len(exhausted.jobs), logs.String())
	}
}

func TestMusicSkipNoticeRegistrationCannotCarryIntoFreshCall(t *testing.T) {
	n, f, sender := noticeFixture(t)
	oldGroup := id.New()
	n.Register(f.voice, f.text, InteractionActor{User: f.actor}, oldGroup)
	oldRoute := noticeRoute(n, f.voice)
	f.authority.call.GroupID = id.New()
	job := &musicNoticeJob{Voice: f.voice, Destination: oldRoute, Items: []string{"old-call"}}
	if err := n.send(t.Context(), job); err == nil {
		t.Fatal("fresh call inherited previous call's registration")
	}
	n.mu.Lock()
	_, registered := n.destinations[f.voice]
	n.mu.Unlock()
	if registered || job.Input != nil {
		t.Fatal("stale call grant was retained or prepared")
	}
	select {
	case <-sender.inputs:
		t.Fatal("old call destination received fresh call notice")
	default:
	}
	n.Register(f.voice, f.text, InteractionActor{User: f.actor}, oldGroup)
	oldRoute = noticeRoute(n, f.voice)
	n.Register(f.voice, f.text, InteractionActor{User: f.actor}, f.authority.call.GroupID)
	current := noticeRoute(n, f.voice)
	if current.grant == oldRoute.grant || current.group != f.authority.call.GroupID {
		t.Fatal("fresh call reused old registration grant")
	}
}
