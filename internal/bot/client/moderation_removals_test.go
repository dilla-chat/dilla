package client

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func removalProofBytes(t *testing.T, request, group id.ID) []byte {
	t.Helper()
	b, err := cborx.Marshal([]any{uint64(1), request, testID(2), testID(3), testID(4), group, uint64(9), uint64(100)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestModerationRemovalsPageDecodesContiguousFeed(t *testing.T) {
	group := testID(10)
	proof := removalProofBytes(t, testID(1), group)
	body := cbor(t, []any{[]any{[]any{uint64(1), proof, make([]byte, 64), uint64(20)}}, uint64(1)})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/v1/groups/"+group.String()+"/moderation/removals" || r.URL.Query().Get("after") != "0" || r.URL.Query().Get("limit") != "10" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
		w.Header().Set("Content-Type", "application/cbor")
		_, _ = w.Write(body)
	}, 0)
	s, _ := c.Session("device-token")
	page, err := s.ModerationRemovals(context.Background(), group, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || page.Rows[0].Revision != 1 || page.Rows[0].AcceptedAt != 20 || page.NextRevision != 1 {
		t.Fatalf("page = %+v", page)
	}
	decoded, decodeErr := protocol.DecodeRemovalProof(page.Rows[0].Proof)
	if decodeErr != nil || decoded.GroupID != group || len(page.Rows[0].Signature) != 64 {
		t.Fatalf("row = %+v", page.Rows[0])
	}
}

func TestModerationRemovalsRejectsMalformedFeedRows(t *testing.T) {
	group := testID(10)
	valid := removalProofBytes(t, testID(1), group)
	other := removalProofBytes(t, testID(1), testID(11))
	for _, tc := range []struct {
		name string
		body any
	}{
		{"revision-gap", []any{[]any{[]any{uint64(2), valid, make([]byte, 64), uint64(20)}}, uint64(2)}},
		{"wrong-next", []any{[]any{[]any{uint64(1), valid, make([]byte, 64), uint64(20)}}, uint64(0)}},
		{"cross-group-proof", []any{[]any{[]any{uint64(1), other, make([]byte, 64), uint64(20)}}, uint64(1)}},
		{"malformed-proof", []any{[]any{[]any{uint64(1), []byte{0x80}, make([]byte, 64), uint64(20)}}, uint64(1)}},
		{"bad-signature-size", []any{[]any{[]any{uint64(1), valid, []byte{1}, uint64(20)}}, uint64(1)}},
		{"bad-row-shape", []any{[]any{[]any{uint64(1), valid, make([]byte, 64)}}, uint64(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, http.StatusOK, discovery(testID(99)))
					return
				}
				reply(w, http.StatusOK, tc.body)
			}, 0)
			s, _ := c.Session("device-token")
			if _, err := s.ModerationRemovals(context.Background(), group, 0, 10); !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v, want ErrProtocol", err)
			}
		})
	}
}

func TestModerationRemovalsRejectsOversizedBody(t *testing.T) {
	group := testID(10)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		w.Header().Set("Content-Type", "application/cbor")
		_, _ = w.Write(make([]byte, MaxModerationRemovalBody+1))
	}, 0)
	s, _ := c.Session("device-token")
	if _, err := s.ModerationRemovals(context.Background(), group, 0, 250); !errors.Is(err, ErrProtocol) {
		t.Fatalf("error = %v, want ErrProtocol", err)
	}
}

func TestModerationRemovalsValidatesArguments(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
		}
	}, 0)
	s, _ := c.Session("device-token")
	for _, tc := range []struct {
		group        id.ID
		after, limit uint64
	}{{id.ID{}, 0, 1}, {testID(1), 1 << 63, 1}, {testID(1), 0, 0}, {testID(1), 0, 251}} {
		if _, err := s.ModerationRemovals(context.Background(), tc.group, tc.after, tc.limit); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("args %+v error=%v, want ErrInvalidConfig", tc, err)
		}
	}
}

func TestAcceptModerationRemovalSubmitsOnlyBoundSignedProof(t *testing.T) {
	group := testID(10)
	proof := removalProofBytes(t, testID(1), group)
	body := cbor(t, []any{proof, make([]byte, 64)})
	requests := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/groups/"+group.String()+"/moderation/removals" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
		reply(w, http.StatusOK, []any{testID(20), uint64(3)})
	}, 0)
	s, _ := c.Session("device-token")
	receipt, err := s.AcceptModerationRemoval(context.Background(), group, body)
	if err != nil || receipt.ActionID != testID(20) || receipt.Revision != 3 {
		t.Fatalf("receipt = %+v, %v", receipt, err)
	}
	wrongBody := cbor(t, []any{removalProofBytes(t, testID(2), testID(11)), make([]byte, 64)})
	if _, err := s.AcceptModerationRemoval(context.Background(), group, wrongBody); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("cross-group proof error = %v, want ErrInvalidConfig", err)
	}
	if _, err := s.AcceptModerationRemoval(context.Background(), group, []byte{0x80}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("malformed body error = %v, want ErrInvalidConfig", err)
	}
	if requests != 1 {
		t.Fatalf("sent %d requests, want only the valid proof", requests)
	}
}
