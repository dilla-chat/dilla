//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

// queuePoller models the durable event queue for interactionEventRuntime: events in order, each
// acknowledged only after its handler succeeds; a failing handler stops the pass at that event.
type queuePoller struct {
	events []native.GroupEvent
}

func (q *queuePoller) PollOnce(ctx context.Context, handler service.GroupEventHandler) (service.GroupEventPollResult, error) {
	var result service.GroupEventPollResult
	for len(q.events) > 0 {
		head := q.events[0]
		if err := handler.HandleGroupEvent(ctx, head); err != nil {
			return result, err
		}
		q.events = q.events[1:]
		result.Handled++
		result.AcknowledgedThrough = head.DeliveryID
	}
	return result, nil
}

type recordingHandler struct{ handled []id.ID }

func (h *recordingHandler) HandleGroupEvent(_ context.Context, event native.GroupEvent) error {
	h.handled = append(h.handled, event.SenderUser)
	return nil
}

// The production runtime applies the hold: an event from an unrefreshed sender never reaches the
// inner handler.
func TestInteractionEventRuntimeNeverHandsAHeldSendersEventToTheHandler(t *testing.T) {
	b := id.New()
	handler := &recordingHandler{}
	runtime := newInteractionEventRuntime(&queuePoller{events: []native.GroupEvent{{DeliveryID: 1, SenderUser: b}}}, handler)
	if _, err := runtime.DrainHolding(t.Context(), map[id.ID]bool{b: true}); !errors.Is(err, service.ErrGroupEventHeld) {
		t.Fatalf("DrainHolding error %v, want the held sender", err)
	}
	if len(handler.handled) != 0 {
		t.Fatalf("the handler received a held sender's event: %v", handler.handled)
	}
}

// Jonas 2026-10-30: after a restart (a fresh runtime), the reconcile pass's isolating trust stage
// reports a stale member B; the pass's drain dispatches A's command and holds B's.
func TestRestartWithAStaleMemberDispatchesOthersAndHoldsThem(t *testing.T) {
	a, b := id.New(), id.New()
	var steps []string
	unrefreshed, err := refreshTrust(t.Context(), trustOutcome{steps: &steps, refresh: service.TrustRefresh{Unrefreshed: map[id.ID]bool{b: true}}}, nil, func(service.TrustRefresh) {})
	if err != nil {
		t.Fatal(err)
	}
	handler := &recordingHandler{}
	queue := &queuePoller{events: []native.GroupEvent{{DeliveryID: 1, SenderUser: a}, {DeliveryID: 2, SenderUser: b}}}
	fresh := newInteractionEventRuntime(queue, handler)
	if _, err := fresh.DrainHolding(t.Context(), unrefreshed); !errors.Is(err, service.ErrGroupEventHeld) {
		t.Fatalf("DrainHolding error %v, want B's event held", err)
	}
	if !reflect.DeepEqual(handler.handled, []id.ID{a}) || len(queue.events) != 1 || queue.events[0].SenderUser != b {
		t.Fatalf("handled %v, queued %v: want A's command dispatched and B's held", handler.handled, queue.events)
	}
}

type readinessMembership struct {
	steps        *[]string
	reconcileErr error
	sync         func(context.Context, *client.Session) error
}

func (m readinessMembership) ReconcileOnce(context.Context, *client.Session) (service.MembershipResult, error) {
	*m.steps = append(*m.steps, "membership")
	return service.MembershipResult{}, m.reconcileErr
}
func (m readinessMembership) ReconcileInteractionGroupsOnce(ctx context.Context, s *client.Session) (service.MembershipResult, error) {
	*m.steps = append(*m.steps, "interaction_sync")
	return service.MembershipResult{}, m.sync(ctx, s)
}

type readinessTrust struct{ steps *[]string }

func (t readinessTrust) TrustPendingLeaves(context.Context, *client.Session) (uint32, error) {
	*t.steps = append(*t.steps, "trust")
	return 0, nil
}

// This exercises the production runtime stage sequence and actual client HTTP
// encoding/error handling. The HTTP peer models the production Ready-only gate;
// bot_group_kind3_http_acceptance_test separately exercises that gate with the
// real repository, DS, authenticated MLS roster and session middleware.
func TestMembershipReadinessBeforePrivateRestore(t *testing.T) {
	for _, tc := range []struct {
		name                                                        string
		pending, denied, membershipDenied, syncDenied, alreadyReady bool
		staleUser, trustFails                                       bool
		want                                                        []string
		stage                                                       string
	}{
		{name: "reenabled", want: []string{"membership", "trust", "ready", "heal", "interaction_sync"}, stage: "interaction_source_sync"},
		// Jonas 2026-10-30: one member's stale device-list history isolates that member and the
		// pass goes on (no freeze after a restart); a general trust failure stays fatal.
		{name: "stale_user_isolated", staleUser: true, want: []string{"membership", "trust", "ready", "heal", "interaction_sync"}, stage: "interaction_source_sync"},
		{name: "general_trust_failure", trustFails: true, want: []string{"membership", "trust"}, stage: "membership_trust"},
		{name: "already_ready", alreadyReady: true, want: []string{"membership", "trust", "heal", "interaction_sync"}, stage: "interaction_source_sync"},
		{name: "source_forbidden", syncDenied: true, want: []string{"membership", "trust", "ready", "heal", "interaction_sync"}, stage: "interaction_source_sync"},
		{name: "pending_membership", pending: true, want: []string{"membership", "trust", "ready"}, stage: "readiness"},
		{name: "current_group_forbidden", denied: true, want: []string{"membership", "trust", "ready", "heal"}, stage: "interaction_restore"},
		{name: "membership_failure", membershipDenied: true, want: []string{"membership"}, stage: "membership_source_sync"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, _ := restoreFixture()
			installation := client.Installation{ID: n.binding.Installation, CommunityID: n.binding.Community, State: 2, Revision: 7}
			var steps []string
			state := uint64(2)
			if tc.alreadyReady {
				state = 3
				installation.State = 3
			}
			reply := func(w http.ResponseWriter, status int, body any) {
				w.Header().Set("Content-Type", "application/cbor")
				w.WriteHeader(status)
				b, err := cborx.Marshal(body)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err = w.Write(b); err != nil {
					t.Error(err)
				}
			}
			base := "/v1/bot-runtime/installations/" + installation.ID.String()
			groupBase := base + "/interaction-groups/" + n.groups[0].Group.String()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, 200, []any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}, n.binding.Instance, uint64(1), "restore-test", uint64(0), []any{uint64(0)}, uint64(1), n.binding.Instance, make([]byte, 32)})
					return
				}
				if r.Header.Get("Authorization") != "Bearer restore-session" {
					t.Error("missing bound session")
				}
				switch {
				case r.Method == http.MethodPost && r.URL.Path == base+"/ready":
					steps = append(steps, "ready")
					var body []any
					if err := cborx.Unmarshal(readinessRequestBody(t, r), &body); err != nil || len(body) != 2 || body[1] != uint64(7) {
						t.Errorf("wrong readiness body: %v %v", body, err)
					}
					if tc.pending {
						reply(w, 409, []any{"E_INVALID_REQUEST", "installation prerequisites are still pending", nil})
						return
					}
					state = 3
					reply(w, 200, []any{installation.ID, installation.CommunityID, state, uint64(8), n.groups[0].Binding.BotUser, n.groups[0].BotDevice, uint64(0), uint64(100)})
				case r.Method == http.MethodGet && r.URL.Path == groupBase+"/heal":
					steps = append(steps, "heal")
					if state != 3 || tc.denied {
						reply(w, 403, []any{"E_FORBIDDEN", "current Bot interaction group unavailable", nil})
						return
					}
					reply(w, 200, []any{uint64(3), uint64(10), uint64(1), uint64(10), uint64(0)})
				case r.Method == http.MethodGet && r.URL.Path == groupBase+"/info":
					if state != 3 || tc.denied || tc.syncDenied {
						reply(w, 403, []any{"E_FORBIDDEN", "current Bot interaction group unavailable", nil})
						return
					}
					reply(w, 200, []any{uint64(3), []byte{1}, make([]byte, 32), uint64(10)})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					reply(w, 404, []any{"E_NOT_FOUND", "", nil})
				}
			}))
			defer srv.Close()
			c, err := client.New(client.Config{BaseURL: srv.URL, InstanceID: n.binding.Instance, AllowLoopbackHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Discover(t.Context()); err != nil {
				t.Fatal(err)
			}
			session, err := c.Session("restore-session")
			if err != nil {
				t.Fatal(err)
			}
			membership := readinessMembership{steps: &steps, sync: func(ctx context.Context, s *client.Session) error {
				_, err := s.BotInteractionGroupInfo(ctx, installation.ID, n.groups[0].Group)
				return err
			}}
			if tc.membershipDenied {
				membership.reconcileErr = errors.New("membership denied")
			}
			stale := id.New()
			trustUnavailable := errors.New("server unavailable")
			var trust intakeTrust = readinessTrust{steps: &steps}
			switch {
			case tc.staleUser:
				trust = trustOutcome{steps: &steps, refresh: service.TrustRefresh{Unrefreshed: map[id.ID]bool{stale: true}}}
			case tc.trustFails:
				trust = trustOutcome{steps: &steps, err: trustUnavailable}
			}
			ready, healed, stage, unrefreshed, err := reconcileMembershipReadiness(t.Context(), installation, n, session, membership, trust, func(service.TrustRefresh) {})
			if tc.trustFails {
				if !errors.Is(err, trustUnavailable) || ready || unrefreshed != nil {
					t.Fatalf("general trust failure: ready=%v unrefreshed=%v err=%v, want fatal", ready, unrefreshed, err)
				}
			}
			if tc.staleUser {
				if err != nil || !ready || !unrefreshed[stale] || len(unrefreshed) != 1 {
					t.Fatalf("stale user: ready=%v unrefreshed=%v err=%v, want ready with the stale user isolated", ready, unrefreshed, err)
				}
			} else if len(unrefreshed) != 0 {
				t.Fatalf("unexpected unrefreshed %v", unrefreshed)
			}
			if !reflect.DeepEqual(steps, tc.want) {
				t.Fatalf("steps=%v want=%v; ready=%v stage=%s err=%v", steps, tc.want, ready, stage, err)
			}
			if stage != tc.stage || healed != 0 || n.exports != 0 {
				t.Fatalf("stage=%s healed=%d exports=%d", stage, healed, n.exports)
			}
			if tc.denied || tc.syncDenied {
				var apiErr *client.APIError
				if !errors.As(err, &apiErr) || apiErr.Status != 403 {
					t.Fatalf("current denial lost: %v", err)
				}
			} else if tc.membershipDenied {
				if !errors.Is(err, membership.reconcileErr) {
					t.Fatalf("membership denial lost: %v", err)
				}
			} else if tc.trustFails {
				return
			} else if err != nil || ready == tc.pending {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
		})
	}
}

func readinessRequestBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
	}
	return b
}
