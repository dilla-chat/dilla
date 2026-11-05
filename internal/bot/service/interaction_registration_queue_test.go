package service

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/cborx"
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
	key := interactionRegistrationBackoffKey{installation: f.install, user: f.request.ParticipantUser, episode: f.episode, device: device}
	if !f.coordinator.backoff.has(key) {
		t.Fatal("no backoff entry after an exhausted take")
	}
	delete(f.s.keyPackageErrFor, device)
	clock.t = clock.t.Add(31 * time.Second)
	f.pass()
	if f.s.registerCalls != 2 {
		t.Fatalf("registered %d devices, want 2", f.s.registerCalls)
	}
	if jobs := f.queuedJobs(t); len(jobs) != 0 {
		t.Fatalf("finished job still queued: %+v", jobs)
	}
	if f.coordinator.backoff.has(key) {
		t.Fatal("backoff entry survived a successful take")
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

func TestQueueStaysVersionOneAfterBackoff(t *testing.T) {
	f, _, _ := newBackoffFixture(t)
	device := id.New()
	f.s.candidates = []id.ID{device}
	f.s.keyPackageErrFor[device] = exhaustedKeyPackageError()
	f.queue(t, f.request.ParticipantUser)
	f.pass()
	value, err := f.n.StateGet(context.Background(), interactionRegistrationStateArea, []byte(interactionRegistrationQueueKey))
	if err != nil {
		t.Fatal(err)
	}
	var decoded []any
	if err := cborx.Unmarshal(value.Value, &decoded); err != nil || len(decoded) != 2 || decoded[0] != uint64(1) {
		t.Fatalf("queue after a backoff = %v (err %v), want wire version 1", decoded, err)
	}
	if jobs := f.queuedJobs(t); len(jobs) != 1 {
		t.Fatalf("queue lost the stuck job: %+v", jobs)
	}
}

func TestUnclassifiedFailureKeepsABackoff(t *testing.T) {
	f, clock, logs := newBackoffFixture(t)
	device := id.New()
	f.s.candidates = []id.ID{device}
	f.s.keyPackageErrFor[device] = exhaustedKeyPackageError()
	f.queue(t, f.request.ParticipantUser)
	f.pass()
	f.s.keyPackageErrFor[device] = &client.APIError{Status: http.StatusInternalServerError, Code: "E_INTERNAL", Outcome: client.OutcomeDefinitive}
	clock.t = clock.t.Add(time.Minute)
	f.pass()
	f.s.keyPackageErrFor[device] = exhaustedKeyPackageError()
	clock.t = clock.t.Add(time.Minute)
	f.pass()
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings, want 1 (a transient failure must not reset the backoff):\n%s", n, logs.String())
	}
}

func TestRealClientRefusalBacksOff(t *testing.T) {
	f, clock, _ := newBackoffFixture(t)
	device := id.New()
	f.s.candidates = []id.ID{device}
	instance := id.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/cbor")
		var status int
		var reply any
		if strings.HasSuffix(r.URL.Path, "/instance") {
			status, reply = http.StatusOK, []any{[]any{uint64(3), uint64(2)}, []any{uint64(1)}, []any{uint64(1)}, instance, uint64(1), "chat.example", uint64(0), []any{uint64(0)}, uint64(1), id.New(), make([]byte, 32)}
		} else {
			status, reply = http.StatusNotFound, []any{"E_KEYPACKAGES_EXHAUSTED", "key package", nil}
		}
		w.WriteHeader(status)
		body, _ := cborx.Marshal(reply)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c, err := client.New(client.Config{BaseURL: srv.URL, InstanceID: instance, AllowLoopbackHTTP: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	session, _ := c.Session("device")
	_, takeErr := session.KeyPackage(context.Background(), device)
	f.s.keyPackageErrFor[device] = takeErr
	f.queue(t, f.request.ParticipantUser)
	f.pass()
	calls := f.s.keyPackageCalls
	clock.t = clock.t.Add(5 * time.Second)
	f.pass()
	if f.s.keyPackageCalls != calls {
		t.Fatal("device taken again inside its backoff window: the client error was not classified")
	}
}

func TestOneJobWithEightExhaustedDevicesDoesNotStarveLaterMembers(t *testing.T) {
	f, _, _ := newBackoffFixture(t)
	var stuck []id.ID
	for i := 0; i < 8; i++ {
		d := id.New()
		stuck = append(stuck, d)
		f.s.keyPackageErrFor[d] = exhaustedKeyPackageError()
	}
	f.s.candidatesByUser = []userDevices{{id.New(), stuck}, {id.New(), []id.ID{id.New()}}}
	f.queueAll(t)
	f.pass()
	f.pass()
	if f.s.registerCalls != 1 {
		t.Fatalf("healthy member registered %d times within 2 passes, want 1", f.s.registerCalls)
	}
}

func TestFourSingleDeviceStuckJobsDoNotStarveLaterMembers(t *testing.T) {
	f, _, _ := newBackoffFixture(t)
	for i := 0; i < 4; i++ {
		d := id.New()
		f.s.keyPackageErrFor[d] = exhaustedKeyPackageError()
		f.s.candidatesByUser = append(f.s.candidatesByUser, userDevices{id.New(), []id.ID{d}})
	}
	f.s.candidatesByUser = append(f.s.candidatesByUser, userDevices{id.New(), []id.ID{id.New()}})
	f.queueAll(t)
	f.pass()
	f.pass()
	if f.s.registerCalls != 1 {
		t.Fatalf("healthy member registered %d times within 2 passes, want 1", f.s.registerCalls)
	}
}

func (f *interactionRegistrationFixture) queueAll(t *testing.T) {
	t.Helper()
	for _, u := range f.s.candidatesByUser {
		f.queue(t, u.user)
	}
}

func TestAnInvalidCreateBacksOffPerDevice(t *testing.T) {
	f, clock, logs := newBackoffFixture(t)
	device := id.New()
	f.s.candidates = []id.ID{device}
	f.n.createErr = &helper.StatusError{Status: 1}
	f.queue(t, f.request.ParticipantUser)
	key := interactionRegistrationBackoffKey{installation: f.install, user: f.request.ParticipantUser, episode: f.episode, device: device}
	for i := 0; i < 20; i++ { // a pass every 30 s for 10 minutes
		f.pass()
		if !f.coordinator.backoff.has(key) {
			t.Fatalf("pass %d: no backoff entry after a refused create", i)
		}
		clock.t = clock.t.Add(30 * time.Second)
	}
	// Delays 30 s, 60 s, 120 s, 240 s: attempts at 0, 30, 90, 210 and 450 s.
	if f.n.createCalls != 5 || f.s.keyPackageCalls != 5 {
		t.Fatalf("creates=%d takes=%d over 10 minutes, want 5 each", f.n.createCalls, f.s.keyPackageCalls)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 5 {
		t.Fatalf("%d warnings, want one per backoff step (5):\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), device.String()) {
		t.Fatalf("warning lacks the device:\n%s", logs.String())
	}
	if jobs := f.queuedJobs(t); len(jobs) != 1 {
		t.Fatalf("queue lost the refused device's job: %+v", jobs)
	}
}
