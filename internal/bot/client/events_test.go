package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	botprotocol "github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestStructuralEventsPageDecodesStrictlyAndAcknowledges(t *testing.T) {
	community := eventFixtureID(1)
	event := botprotocol.Event{ID: eventFixtureID(2), Sequence: 2, CommunityID: community, Timestamp: 4, Kind: botprotocol.EventMemberJoin, Data: []any{eventFixtureID(5), eventFixtureID(6)}}
	pageBody := cbor(t, []any{[]any{[]any{uint64(1), event.ID, event.Sequence, event.CommunityID, event.Timestamp, uint64(event.Kind), event.Data}}, uint64(2), uint64(8), uint64(1)})
	instance := testID(99)
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.RequestURI())
		if r.URL.Path == "/v1/instance" {
			w.Header().Set("Content-Type", "application/cbor")
			reply(w, 200, discovery(instance))
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("missing scoped session")
		}
		switch r.Method {
		case http.MethodGet:
			if r.URL.Path != "/v1/communities/"+community.String()+"/bot/events" || r.URL.Query().Get("after") != "1" || r.URL.Query().Get("limit") != "1" {
				t.Errorf("unexpected event query %s", r.URL.RequestURI())
			}
			w.Header().Set("Content-Type", "application/cbor")
			_, _ = w.Write(pageBody)
		case http.MethodPut:
			if r.URL.Path != "/v1/communities/"+community.String()+"/bot/events/ack" {
				t.Errorf("unexpected ack path %q", r.URL.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method %q", r.Method)
		}
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client(), InstanceID: instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Session("secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	page, err := s.StructuralEvents(context.Background(), community, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].ID != event.ID || page.NextSequence != 2 || page.HighWater != 8 || page.Floor != 1 {
		t.Fatalf("page = %+v", page)
	}
	if err = s.AcknowledgeStructuralEvents(context.Background(), community, 2); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 3 {
		t.Fatalf("requests = %v", methods)
	}
}

func TestStructuralEventsPrunedResponseCarriesExplicitGapBounds(t *testing.T) {
	community := testID(1)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		reply(w, http.StatusGone, []any{"E_PRUNED", "event history expired", nil, uint64(11), uint64(15)})
	}, 0)
	s, _ := c.Session("device-token")
	_, err := s.StructuralEvents(context.Background(), community, 2, 10)
	var gap *EventHistoryGapError
	if !errors.As(err, &gap) || gap.Floor != 11 || gap.HighWater != 15 {
		t.Fatalf("gap = %#v, error=%v", gap, err)
	}
}

func TestStructuralEventsAcceptsFreshZeroFloor(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		reply(w, http.StatusOK, []any{[]any{}, uint64(0), uint64(0), uint64(0)})
	}, 0)
	s, _ := c.Session("device-token")
	page, err := s.StructuralEvents(context.Background(), testID(1), 0, 10)
	if err != nil || page.Floor != 0 || page.HighWater != 0 || page.NextSequence != 0 {
		t.Fatalf("page=%+v error=%v", page, err)
	}
}

func TestStructuralEventsRejectsSequenceGapsAndIncompleteTail(t *testing.T) {
	community := testID(1)
	makeEvent := func(seq uint64) []any {
		return []any{uint64(1), testID(byte(seq + 1)), seq, community, uint64(4), uint64(1), []any{testID(3), testID(4)}}
	}
	for _, tc := range []struct {
		name string
		rows []any
		next uint64
		high uint64
	}{
		{"gap-in-page", []any{makeEvent(3)}, 3, 3},
		{"missing-tail", []any{makeEvent(2)}, 2, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, http.StatusOK, discovery(testID(99)))
					return
				}
				reply(w, http.StatusOK, []any{tc.rows, tc.next, tc.high, uint64(1)})
			}, 0)
			s, _ := c.Session("device-token")
			if _, err := s.StructuralEvents(context.Background(), community, 1, 10); !errors.Is(err, ErrProtocol) {
				t.Fatalf("error=%v, want ErrProtocol", err)
			}
		})
	}
}

func TestStructuralEventsRejectsWrongCommunityAndNonIncreasingSequence(t *testing.T) {
	community := testID(1)
	other := testID(9)
	for _, tc := range []struct {
		name string
		rows []any
	}{
		{"wrong-community", []any{[]any{uint64(1), testID(2), uint64(2), other, uint64(4), uint64(1), []any{testID(3), testID(4)}}}},
		{"duplicate-sequence", []any{
			[]any{uint64(1), testID(2), uint64(2), community, uint64(4), uint64(1), []any{testID(3), testID(4)}},
			[]any{uint64(1), testID(5), uint64(2), community, uint64(4), uint64(1), []any{testID(6), testID(7)}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, http.StatusOK, discovery(testID(99)))
					return
				}
				reply(w, http.StatusOK, []any{tc.rows, uint64(3), uint64(3), uint64(1)})
			}, 0)
			s, _ := c.Session("device-token")
			if _, err := s.StructuralEvents(context.Background(), community, 1, 10); !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v, want ErrProtocol", err)
			}
		})
	}
}

func eventFixtureID(n byte) id.ID {
	var out id.ID
	out[15] = n
	return out
}
