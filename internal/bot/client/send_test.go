package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	botprotocol "github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func sendReceiptForTest(operation, attempt, community, group id.ID, message []byte) botprotocol.SendReceipt {
	hash := sha256.Sum256(message)
	return botprotocol.SendReceipt{OperationID: operation, AttemptID: attempt, CommunityID: community, GroupID: group, ExpectedGeneration: 8, Epoch: 4, Class: 0, CiphertextHash: hash[:], Outcome: botprotocol.ReceiptClosedNotAccepted}
}

func TestPostBotMessageSendsExactCiphertextAndDecodesReceipt(t *testing.T) {
	community, group, operation, attempt := testID(31), testID(32), testID(33), testID(34)
	ciphertext := []byte{0, 0xff, 0x80, 1, 2}
	receipt, _ := botprotocol.EncodeReceipt(sendReceiptForTest(operation, attempt, community, group, ciphertext))
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/groups/"+group.String()+"/bot/messages" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer send-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		raw, err := io.ReadAll(r.Body)
		var tuple []any
		if err != nil || cborx.Unmarshal(raw, &tuple) != nil || len(tuple) != 7 {
			t.Fatalf("request tuple: %v", err)
		}
		gotOperation, opOK := asID(tuple[0])
		gotAttempt, attemptOK := asID(tuple[1])
		if !opOK || gotOperation != operation || !attemptOK || gotAttempt != attempt || tuple[2] != nil || tuple[3] != uint64(8) || tuple[4] != uint64(4) || !bytes.Equal(tuple[5].([]byte), ciphertext) || tuple[6] != uint64(0) {
			t.Errorf("request tuple=%#v", tuple)
		}
		w.Header().Set("Content-Type", "application/cbor")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(receipt)
	}, time.Second)
	s, _ := c.Session("send-token")
	got, err := s.PostBotMessage(context.Background(), community, group, BotMessageUpload{OperationID: operation, AttemptID: attempt, ExpectedGeneration: 8, Epoch: 4, Class: 0, PrivateMessage: ciphertext})
	if err != nil {
		t.Fatal(err)
	}
	if got.OperationID != operation || got.AttemptID != attempt || got.GroupID != group || !bytes.Equal(got.CiphertextHash, sendReceiptForTest(operation, attempt, community, group, ciphertext).CiphertextHash) {
		t.Fatalf("receipt=%+v", got)
	}
}

func TestResolveAndLookupSendReceiptUseTypedBodiesAndExactReceipt(t *testing.T) {
	community, group, operation, attempt := testID(41), testID(42), testID(43), testID(44)
	previous := testID(45)
	ciphertext := []byte("encrypted bytes")
	receiptValue := sendReceiptForTest(operation, attempt, community, group, ciphertext)
	receiptValue.PredecessorAttemptID = &previous
	receipt, _ := botprotocol.EncodeReceipt(receiptValue)
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		calls++
		if calls == 1 {
			want := "/v1/communities/" + community.String() + "/bot/receipts/" + operation.String() + "/resolve"
			if r.Method != http.MethodPost || r.URL.Path != want {
				t.Errorf("resolve request=%s %s", r.Method, r.URL.Path)
			}
			raw, _ := io.ReadAll(r.Body)
			var tuple []any
			if cborx.Unmarshal(raw, &tuple) != nil || len(tuple) != 7 {
				t.Errorf("resolve tuple=%x", raw)
			} else {
				gotAttempt, aok := asID(tuple[0])
				gotPrevious, pok := asID(tuple[1])
				gotGroup, gok := asID(tuple[2])
				if !aok || gotAttempt != attempt || !pok || gotPrevious != previous || !gok || gotGroup != group || tuple[3] != uint64(8) || tuple[4] != uint64(4) || tuple[5] != uint64(0) || !bytes.Equal(tuple[6].([]byte), sha256Bytes(ciphertext)) {
					t.Errorf("resolve body=%#v", tuple)
				}
			}
		} else if r.Method != http.MethodGet || r.URL.Path != "/v1/communities/"+community.String()+"/bot/receipts/"+operation.String() || r.URL.Query().Get("attempt") != attempt.String() {
			t.Errorf("lookup request=%s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/cbor")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(receipt)
	}, time.Second)
	s, _ := c.Session("send-token")
	resolve := BotMessageResolution{AttemptID: attempt, PredecessorAttemptID: &previous, GroupID: group, ExpectedGeneration: 8, Epoch: 4, Class: 0, CiphertextSHA256: sha256.Sum256(ciphertext)}
	if got, err := s.ResolveBotMessage(context.Background(), community, operation, resolve); err != nil || got.AttemptID != attempt {
		t.Fatalf("resolve=%+v err=%v", got, err)
	}
	if got, err := s.BotMessageReceipt(context.Background(), community, operation, attempt, group); err != nil || got.AttemptID != attempt {
		t.Fatalf("lookup=%+v err=%v", got, err)
	}
}

func TestBotSendMethodsRejectInvalidInputsAndReceiptScope(t *testing.T) {
	requests := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		requests++
		reply(w, http.StatusOK, []any{})
	}, time.Second)
	s, _ := c.Session("send-token")
	if _, err := s.PostBotMessage(context.Background(), testID(1), testID(2), BotMessageUpload{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid upload error=%v", err)
	}
	if _, err := s.BotMessageReceipt(context.Background(), id.Zero, testID(2), testID(3), testID(4)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid lookup error=%v", err)
	}
	if _, err := s.ResolveBotMessage(context.Background(), testID(1), testID(2), BotMessageResolution{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid resolve error=%v", err)
	}
	if requests != 0 {
		t.Fatalf("sent %d invalid requests", requests)
	}

	foreign, _ := botprotocol.EncodeReceipt(sendReceiptForTest(testID(3), testID(4), testID(77), testID(2), []byte("ciphertext")))
	c = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		w.Header().Set("Content-Type", "application/cbor")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(foreign)
	}, time.Second)
	s, _ = c.Session("send-token")
	if _, err := s.BotMessageReceipt(context.Background(), testID(1), testID(3), testID(4), testID(2)); !errors.Is(err, ErrProtocol) {
		t.Fatalf("foreign receipt accepted: %v", err)
	}
}

func TestSendMutationMismatchedReceiptIsOutcomeUnknown(t *testing.T) {
	community, group, operation, attempt := testID(51), testID(52), testID(53), testID(54)
	ciphertext := []byte("private ciphertext")
	for _, method := range []string{"post", "resolve"} {
		t.Run(method, func(t *testing.T) {
			bad := sendReceiptForTest(operation, attempt, community, group, ciphertext)
			bad.ExpectedGeneration++
			response, _ := botprotocol.EncodeReceipt(bad)
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, 200, discovery(testID(99)))
					return
				}
				w.Header().Set("Content-Type", "application/cbor")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(response)
			}, time.Second)
			s, _ := c.Session("send-token")
			var err error
			if method == "post" {
				_, err = s.PostBotMessage(context.Background(), community, group, BotMessageUpload{OperationID: operation, AttemptID: attempt, ExpectedGeneration: 8, Epoch: 4, Class: 0, PrivateMessage: ciphertext})
			} else {
				_, err = s.ResolveBotMessage(context.Background(), community, operation, BotMessageResolution{AttemptID: attempt, GroupID: group, ExpectedGeneration: 8, Epoch: 4, Class: 0, CiphertextSHA256: sha256.Sum256(ciphertext)})
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Outcome != OutcomeUnknown {
				t.Fatalf("mismatched successful receipt error=%v, want outcome unknown", err)
			}
		})
	}
}

func TestResolveNotFoundRemainsDefinitiveWhileReceiptLookupUsesNotFoundSentinel(t *testing.T) {
	community, group, operation, attempt := testID(61), testID(62), testID(63), testID(64)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		reply(w, http.StatusNotFound, []any{"E_NOT_FOUND", "send receipt not found", nil})
	}, time.Second)
	s, _ := c.Session("send-token")
	_, err := s.ResolveBotMessage(context.Background(), community, operation, BotMessageResolution{AttemptID: attempt, GroupID: group, ExpectedGeneration: 8, Epoch: 4, Class: 0, CiphertextSHA256: sha256.Sum256([]byte("ciphertext"))})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound || apiErr.Outcome != OutcomeDefinitive || errors.Is(err, ErrBotSendReceiptNotFound) {
		t.Fatalf("resolve 404=%v, want definitive API error", err)
	}
	_, err = s.BotMessageReceipt(context.Background(), community, operation, attempt, group)
	if !errors.Is(err, ErrBotSendReceiptNotFound) {
		t.Fatalf("receipt lookup 404=%v, want ErrBotSendReceiptNotFound", err)
	}
}

func TestBotMessageReceiptPinsExpectedGroup(t *testing.T) {
	community, operation, attempt := testID(71), testID(72), testID(73)
	receipt, _ := botprotocol.EncodeReceipt(sendReceiptForTest(operation, attempt, community, testID(74), []byte("ciphertext")))
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		w.Header().Set("Content-Type", "application/cbor")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(receipt)
	}, time.Second)
	s, _ := c.Session("send-token")
	if _, err := s.BotMessageReceipt(context.Background(), community, operation, attempt, testID(75)); !errors.Is(err, ErrProtocol) {
		t.Fatalf("receipt for another group accepted: %v", err)
	}
}

func sha256Bytes(b []byte) []byte { h := sha256.Sum256(b); return h[:] }
