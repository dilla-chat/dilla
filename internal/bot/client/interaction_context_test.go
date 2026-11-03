package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

func TestBotInteractionContextSnapshotReturnsExactSignedWire(t *testing.T) {
	installation, requestID, group := testID(10), testID(11), testID(12)
	participant, device := testID(13), testID(14)
	payload := []byte{0x82, 0x43, 0x00, 0x01, 0xff, 0x42, 0xfe, 0xfd}
	signature := make([]byte, 64)
	for i := range signature {
		signature[i] = byte(i + 1)
	}
	want := cbor(t, []any{payload, signature})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/bot-runtime/installations/"+installation.String()+"/interaction-admissions/context" ||
			r.Header.Get("Authorization") != "Bearer bot-session" || r.Header.Get("Content-Type") != "application/cbor" {
			t.Errorf("request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		fields, err := decode(body, 4)
		if err != nil {
			t.Errorf("request body: %v", err)
			return
		}
		got := make([]id.ID, 4)
		for i := range got {
			got[i], _ = asID(fields[i])
		}
		if got[0] != requestID || got[1] != group || got[2] != participant || got[3] != device {
			t.Errorf("request tuple = %v", got)
		}
		w.Header().Set("Content-Type", "application/cbor")
		_, _ = w.Write(want)
	}, 0)
	s, _ := c.Session("bot-session")
	got, err := s.BotInteractionContextSnapshot(context.Background(), installation, requestID, group, participant, device)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("returned wire changed: got %x want %x", got, want)
	}
}

func TestBotInteractionContextSnapshotRejectsWrongSessionOrInstallation(t *testing.T) {
	installation := testID(20)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		if r.Header.Get("Authorization") != "Bearer expected-session" || r.URL.Path != "/v1/bot-runtime/installations/"+installation.String()+"/interaction-admissions/context" {
			reply(w, http.StatusForbidden, []any{"E_FORBIDDEN", "installation mismatch", nil})
			return
		}
		reply(w, http.StatusOK, []any{[]byte{1}, make([]byte, 64)})
	}, 0)
	wrongSession, _ := c.Session("other-session")
	if _, err := wrongSession.BotInteractionContextSnapshot(context.Background(), installation, testID(21), testID(22), testID(23), testID(24)); err == nil {
		t.Fatal("wrong session unexpectedly succeeded")
	}
	rightSession, _ := c.Session("expected-session")
	if _, err := rightSession.BotInteractionContextSnapshot(context.Background(), testID(25), testID(21), testID(22), testID(23), testID(24)); err == nil {
		t.Fatal("wrong installation unexpectedly succeeded")
	}
}

func TestBotInteractionContextSnapshotRejectsMalformedAndOversizedResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "wrong tuple length", body: cbor(t, []any{[]byte{1}, make([]byte, 64), uint64(3)})},
		{name: "empty payload", body: cbor(t, []any{[]byte{}, make([]byte, 64)})},
		{name: "wrong signature size", body: cbor(t, []any{[]byte{1}, []byte{2}})},
		{name: "payload too large", body: cbor(t, []any{make([]byte, 160<<10+1), make([]byte, 64)})},
		{name: "response too large", body: cbor(t, []any{[]byte{1}, make([]byte, MaxInteractionContextResponse)})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, http.StatusOK, discovery(testID(99)))
					return
				}
				w.Header().Set("Content-Type", "application/cbor")
				_, _ = w.Write(tc.body)
			}, 0)
			s, _ := c.Session("bot-session")
			if _, err := s.BotInteractionContextSnapshot(context.Background(), testID(30), testID(31), testID(32), testID(33), testID(34)); !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v, want ErrProtocol", err)
			}
		})
	}
}

func TestBotInteractionContextSnapshotValidatesIdentifiers(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		t.Error("invalid request reached server")
	}, 0)
	s, _ := c.Session("bot-session")
	valid := []id.ID{testID(40), testID(41), testID(42), testID(43), testID(44)}
	for i := range valid {
		args := append([]id.ID(nil), valid...)
		args[i] = id.ID{}
		if _, err := s.BotInteractionContextSnapshot(context.Background(), args[0], args[1], args[2], args[3], args[4]); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("zero argument %d error = %v", i, err)
		}
	}
}
