package service

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/id"
)

func exhaustedKeyPackageError() error {
	return &client.APIError{Status: http.StatusNotFound, Code: "E_KEYPACKAGES_EXHAUSTED", Outcome: client.OutcomeDefinitive}
}

type backoffClock struct{ t time.Time }

func (c *backoffClock) now() time.Time { return c.t }

func newBackoffFixture(t *testing.T) (*interactionRegistrationFixture, *backoffClock, *bytes.Buffer) {
	t.Helper()
	f := newInteractionRegistrationFixture(t)
	clock := &backoffClock{t: time.Unix(1_000_000, 0)}
	logs := &bytes.Buffer{}
	f.coordinator.now = clock.now
	f.coordinator.WithLogger(slog.New(slog.NewTextHandler(logs, nil)))
	f.s.currentEpisode = f.episode
	f.s.keyPackage = []byte{7, 8, 9}
	f.s.keyPackageErrFor = map[id.ID]error{}
	return f, clock, logs
}

func (f *interactionRegistrationFixture) pass() {
	_ = f.coordinator.ProcessPending(context.Background(), 4, 8, f.s, f.s)
}

func (f *interactionRegistrationFixture) queue(t *testing.T, user id.ID) {
	t.Helper()
	if err := f.coordinator.QueueMemberJoin(context.Background(), user, f.episode); err != nil {
		t.Fatal(err)
	}
}

func (f *interactionRegistrationFixture) queuedJobs(t *testing.T) []interactionRegistrationJob {
	t.Helper()
	value, err := f.n.StateGet(context.Background(), interactionRegistrationStateArea, []byte(interactionRegistrationQueueKey))
	if err != nil {
		t.Fatal(err)
	}
	if value.Revision == 0 {
		return nil
	}
	jobs, err := decodeInteractionRegistrationJobs(value.Value)
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func TestExhaustedDeviceBacksOff(t *testing.T) {
	f, clock, _ := newBackoffFixture(t)
	device := id.New()
	f.s.candidates = []id.ID{device}
	f.s.keyPackageErrFor[device] = exhaustedKeyPackageError()
	f.queue(t, f.request.ParticipantUser)
	var attempts []time.Duration
	start := clock.t
	// A pass every 10 s for 5 hours; record when the take is actually tried.
	for elapsed := time.Duration(0); elapsed <= 5*time.Hour; elapsed += 10 * time.Second {
		clock.t = start.Add(elapsed)
		before := f.s.keyPackageCalls
		f.pass()
		if f.s.keyPackageCalls > before {
			attempts = append(attempts, elapsed)
		}
	}
	// Gaps of 30 s, 60 s, 120 s ... doubling to the 1 h cap.
	gaps := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second, 480 * time.Second, 960 * time.Second, 1920 * time.Second, time.Hour, time.Hour, time.Hour}
	want := []time.Duration{0}
	for _, gap := range gaps {
		want = append(want, want[len(want)-1]+gap)
	}
	if len(attempts) != len(want) {
		t.Fatalf("attempts at %v, want %v", attempts, want)
	}
	for i := range want {
		if attempts[i] != want[i] {
			t.Fatalf("attempts at %v, want %v", attempts, want)
		}
	}
}

func TestStuckDevicesDoNotStarveLaterMembers(t *testing.T) {
	f, _, _ := newBackoffFixture(t)
	for i := 0; i < 4; i++ {
		a, b := id.New(), id.New()
		f.s.keyPackageErrFor[a], f.s.keyPackageErrFor[b] = exhaustedKeyPackageError(), exhaustedKeyPackageError()
		f.s.candidatesByUser = append(f.s.candidatesByUser, userDevices{id.New(), []id.ID{a, b}})
	}
	f.s.candidatesByUser = append(f.s.candidatesByUser, userDevices{id.New(), []id.ID{id.New()}})
	for _, u := range f.s.candidatesByUser {
		f.queue(t, u.user)
	}
	f.pass()
	f.pass()
	if f.s.registerCalls != 1 {
		t.Fatalf("healthy member registered %d times within 2 passes, want 1", f.s.registerCalls)
	}
}

func TestPublishClearsBackoff(t *testing.T) {
	f, clock, _ := newBackoffFixture(t)
	device, other := id.New(), id.New()
	f.s.candidates = []id.ID{device, other}
	f.s.keyPackageErrFor[device] = exhaustedKeyPackageError()
	f.queue(t, f.request.ParticipantUser)
	f.pass()
	if jobs := f.queuedJobs(t); len(jobs) != 1 || len(jobs[0].Backoff) != 1 || jobs[0].Backoff[0].Device != device {
		t.Fatalf("backoff after an exhausted take: %+v", jobs)
	}
	delete(f.s.keyPackageErrFor, device)
	clock.t = clock.t.Add(31 * time.Second)
	f.pass()
	if f.s.registerCalls != 2 {
		t.Fatalf("registered %d devices, want 2", f.s.registerCalls)
	}
	if jobs := f.queuedJobs(t); len(jobs) != 0 {
		t.Fatalf("finished job still queued with its backoff: %+v", jobs)
	}
}

func TestNotFoundDeviceBacksOffToo(t *testing.T) {
	f, clock, _ := newBackoffFixture(t)
	device := id.New()
	f.s.candidates = []id.ID{device}
	f.s.keyPackageErrFor[device] = &client.APIError{Status: http.StatusNotFound, Code: "E_NOT_FOUND", Outcome: client.OutcomeDefinitive}
	f.queue(t, f.request.ParticipantUser)
	f.pass()
	calls := f.s.keyPackageCalls
	clock.t = clock.t.Add(10 * time.Second)
	f.pass()
	if f.s.keyPackageCalls != calls {
		t.Fatal("device retried inside its backoff window")
	}
}

func TestTransientTakeFailureIsNotBackedOff(t *testing.T) {
	f, _, _ := newBackoffFixture(t)
	device := id.New()
	f.s.candidates = []id.ID{device}
	f.s.keyPackageErrFor[device] = &client.APIError{Status: http.StatusInternalServerError, Code: "E_INTERNAL", Outcome: client.OutcomeDefinitive}
	f.queue(t, f.request.ParticipantUser)
	f.pass()
	f.pass()
	if f.s.keyPackageCalls != 2 {
		t.Fatalf("take attempted %d times over 2 passes, want 2", f.s.keyPackageCalls)
	}
}

func TestBackoffLogsOnce(t *testing.T) {
	f, clock, logs := newBackoffFixture(t)
	device := id.New()
	f.s.candidates = []id.ID{device}
	f.s.keyPackageErrFor[device] = exhaustedKeyPackageError()
	f.queue(t, f.request.ParticipantUser)
	for i := 0; i < 5; i++ {
		f.pass()
		clock.t = clock.t.Add(2 * time.Hour)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), device.String()) || !strings.Contains(logs.String(), "E_KEYPACKAGES_EXHAUSTED") {
		t.Fatalf("warning lacks the device or the reason:\n%s", logs.String())
	}
	// A new device running dry is a state change and warns anew.
	second := id.New()
	f.s.candidates = []id.ID{device, second}
	f.s.keyPackageErrFor[second] = exhaustedKeyPackageError()
	f.pass()
	if n := strings.Count(logs.String(), "level=WARN"); n != 2 {
		t.Fatalf("%d warnings after a second device ran dry, want 2:\n%s", n, logs.String())
	}
}
