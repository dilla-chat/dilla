package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func registrationHash(body []byte) []byte {
	h := sha256.New()
	_, _ = h.Write([]byte("dilla bot group registration v1\x00"))
	_, _ = h.Write(body)
	return h.Sum(nil)
}

func registrationBody(t *testing.T, group id.ID) []byte {
	t.Helper()
	return cbor(t, []any{group, []byte("binding"), []byte("group-info"), []byte("tree")})
}

func kind3RegistrationBody(t *testing.T, group id.ID) []byte {
	t.Helper()
	return cbor(t, []any{group, []byte("binding"), []byte("group-info"), []byte("tree"), []byte("initial-welcome")})
}

func registrationReceipt(operation, group id.ID, body []byte, generation, epoch, next uint64) []any {
	return []any{operation, group, registrationHash(body), generation, epoch, next}
}

func TestRegisterBotGroupPostsExactPendingBodyAndAcceptsNewOrReplay(t *testing.T) {
	installation, operation, group := testID(21), testID(22), testID(23)
	body := registrationBody(t, group)
	for _, status := range []int{http.StatusCreated, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, 200, discovery(testID(99)))
					return
				}
				wantPath := "/v1/bot-runtime/installations/" + installation.String() + "/groups"
				if r.Method != http.MethodPost || r.URL.Path != wantPath {
					t.Errorf("request = %s %s, want POST %s", r.Method, r.URL.Path, wantPath)
				}
				if r.Header.Get("Authorization") != "Bearer device-token" {
					t.Errorf("authorization = %q", r.Header.Get("Authorization"))
				}
				var tuple []any
				// Handler reads the original registration body from the request's bstr.
				requestBytes, err := io.ReadAll(r.Body)
				if err != nil || cborx.Unmarshal(requestBytes, &tuple) != nil || len(tuple) != 3 {
					t.Fatalf("request tuple %x: %v", requestBytes, err)
				}
				gotOperation, opOK := asID(tuple[0])
				if !opOK || !equalID(gotOperation, operation) || tuple[1] != uint64(17) {
					t.Errorf("operation/generation = %#v/%#v", tuple[0], tuple[1])
				}
				gotBody, ok := tuple[2].([]byte)
				if !ok || !bytes.Equal(gotBody, body) {
					t.Errorf("registration body = %x, want exact %x", gotBody, body)
				}
				w.Header().Set("Content-Type", "application/cbor")
				w.WriteHeader(status)
				_, _ = w.Write(cbor(t, registrationReceipt(operation, group, body, 17, 9, 1)))
			}, time.Second)
			s, _ := c.Session("device-token")
			receipt, err := s.RegisterBotGroup(context.Background(), installation, operation, 17, body)
			if err != nil {
				t.Fatalf("RegisterBotGroup: %v", err)
			}
			if receipt.OperationID != operation || receipt.GroupID != group || receipt.AcceptedGeneration != 17 || receipt.InitialEpoch != 9 || receipt.NextSequence != 1 || receipt.RegistrationSHA256 != sha256.Sum256(registrationHashPreimage(body)) {
				t.Fatalf("receipt = %+v", receipt)
			}
		})
	}
}

func TestRegisterBotGroupWithAdmissionRequestPostsExactKind3Body(t *testing.T) {
	installation, operation, admissionRequest, group := testID(21), testID(22), testID(24), testID(23)
	body := kind3RegistrationBody(t, group)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		var tuple []any
		requestBytes, err := io.ReadAll(r.Body)
		if err != nil || cborx.Unmarshal(requestBytes, &tuple) != nil || len(tuple) != 4 {
			t.Fatalf("request tuple %x: %v", requestBytes, err)
		}
		gotAdmission, ok := asID(tuple[3])
		if !ok || gotAdmission != admissionRequest {
			t.Errorf("admission request id = %#v, want %s", tuple[3], admissionRequest)
		}
		gotBody, ok := tuple[2].([]byte)
		if !ok || !bytes.Equal(gotBody, body) {
			t.Errorf("registration body = %x, want exact %x", gotBody, body)
		}
		reply(w, http.StatusCreated, registrationReceipt(operation, group, body, 17, 9, 1))
	}, time.Second)
	s, _ := c.Session("device-token")
	if _, err := s.RegisterBotGroupWithAdmissionRequest(context.Background(), installation, operation, 17, body, admissionRequest); err != nil {
		t.Fatalf("kind-3 admission registration: %v", err)
	}
}

func TestRegisterBotGroupWithAdmissionRequestConflictingReceiptIsUnknown(t *testing.T) {
	installation, operation, group := testID(21), testID(22), testID(23)
	body := kind3RegistrationBody(t, group)
	conflictingBody := kind3RegistrationBody(t, testID(25))
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		reply(w, http.StatusOK, registrationReceipt(operation, group, conflictingBody, 17, 9, 1))
	}, time.Second)
	s, _ := c.Session("device-token")
	_, err := s.RegisterBotGroupWithAdmissionRequest(context.Background(), installation, operation, 17, body, testID(24))
	if !outcomeIs(err, OutcomeUnknown) {
		t.Fatalf("conflicting receipt error = %v, want unknown outcome", err)
	}
}

func TestRegisterBotGroupRequiresBodyShapeToMatchAdmissionKind(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		calls++
	}, time.Second)
	s, _ := c.Session("device-token")
	if _, err := s.RegisterBotGroupWithAdmissionRequest(context.Background(), testID(21), testID(22), 17, registrationBody(t, testID(23)), testID(24)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("four-field kind-3 body error = %v, want ErrInvalidConfig", err)
	}
	emptyWelcome := cbor(t, []any{testID(23), []byte("binding"), []byte("info"), []byte("tree"), []byte{}})
	if _, err := s.RegisterBotGroupWithAdmissionRequest(context.Background(), testID(21), testID(22), 17, emptyWelcome, testID(24)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty Welcome error = %v, want ErrInvalidConfig", err)
	}
	if calls != 0 {
		t.Fatalf("sent %d requests for invalid kind-3 bodies", calls)
	}
}

func TestRegisterBotGroupWithAdmissionRequestRejectsZeroIDBeforeRequest(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		calls++
	}, time.Second)
	s, _ := c.Session("device-token")
	if _, err := s.RegisterBotGroupWithAdmissionRequest(context.Background(), testID(21), testID(22), 17, registrationBody(t, testID(23)), id.Zero); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("error = %v, want ErrInvalidConfig", err)
	}
	if calls != 0 {
		t.Fatalf("sent %d requests for zero admission request id", calls)
	}
}

func registrationHashPreimage(body []byte) []byte {
	return append([]byte("dilla bot group registration v1\x00"), body...)
}

func TestRegisterBotGroupRejectsInvalidPendingIntentBeforeRequest(t *testing.T) {
	valid := registrationBody(t, testID(23))
	trailing := append(append([]byte(nil), valid...), 0)
	group := testID(23)
	noncanonical := append([]byte{0x84, 0x58, 0x10}, append(group[:], []byte{0x47, 'b', 'i', 'n', 'd', 'i', 'n', 'g', 0x4a, 'g', 'r', 'o', 'u', 'p', '-', 'i', 'n', 'f', 'o', 0x44, 't', 'r', 'e', 'e'}...)...)
	tooLarge := make([]byte, 65506)
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		calls++
		reply(w, 201, registrationReceipt(testID(22), testID(23), valid, 17, 9, 1))
	}, time.Second)
	s, _ := c.Session("device-token")
	bad := []struct {
		name                    string
		installation, operation id.ID
		generation              uint64
		body                    []byte
	}{{"zero-installation", id.Zero, testID(22), 17, valid}, {"zero-operation", testID(21), id.Zero, 17, valid}, {"generation-overflow", testID(21), testID(22), math.MaxInt64 + 1, valid}, {"trailing", testID(21), testID(22), 17, trailing}, {"noncanonical", testID(21), testID(22), 17, noncanonical}, {"too-large", testID(21), testID(22), 17, tooLarge}}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.RegisterBotGroup(context.Background(), tc.installation, tc.operation, tc.generation, tc.body); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("sent %d requests for invalid intents", calls)
	}
}

func TestRegisterBotGroupAcceptsMaximumCanonicalBody(t *testing.T) {
	installation, operation, group := testID(21), testID(22), testID(23)
	body := cbor(t, []any{group, make([]byte, 65482), []byte{}, []byte{}})
	if len(body) != maxBotGroupRegistrationBody {
		t.Fatalf("test body length = %d, want %d", len(body), maxBotGroupRegistrationBody)
	}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		reply(w, http.StatusCreated, registrationReceipt(operation, group, body, 17, 9, 1))
	}, time.Second)
	s, _ := c.Session("device-token")
	if _, err := s.RegisterBotGroup(context.Background(), installation, operation, 17, body); err != nil {
		t.Fatalf("maximum registration body: %v", err)
	}
}

func TestRegisterBotGroupRejectsMismatchedOrOutOfBoundsReceiptAsUnknown(t *testing.T) {
	installation, operation, group := testID(21), testID(22), testID(23)
	body := registrationBody(t, group)
	valid := registrationReceipt(operation, group, body, 17, 9, 1)
	tests := []struct {
		name     string
		response any
	}{
		{"operation-scope-mismatch", registrationReceipt(testID(24), group, body, 17, 9, 1)},
		{"group-mismatch", registrationReceipt(operation, testID(24), body, 17, 9, 1)},
		{"hash-mismatch", []any{operation, group, make([]byte, 32), uint64(17), uint64(9), uint64(1)}},
		{"generation-mismatch", registrationReceipt(operation, group, body, 18, 9, 1)},
		{"generation-overflow", []any{operation, group, registrationHash(body), uint64(math.MaxInt64) + 1, uint64(9), uint64(1)}},
		{"epoch-overflow", []any{operation, group, registrationHash(body), uint64(17), uint64(math.MaxInt64) + 1, uint64(1)}},
		{"next-sequence-zero", []any{operation, group, registrationHash(body), uint64(17), uint64(9), uint64(0)}},
		{"next-sequence-overflow", []any{operation, group, registrationHash(body), uint64(17), uint64(9), uint64(math.MaxInt64) + 1}},
		{"extra-field", append(valid, uint64(1))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, 200, discovery(testID(99)))
					return
				}
				reply(w, 201, tc.response)
			}, time.Second)
			s, _ := c.Session("device-token")
			if _, err := s.RegisterBotGroup(context.Background(), installation, operation, 17, body); !outcomeIs(err, OutcomeUnknown) {
				t.Fatalf("error = %v, want unknown outcome", err)
			}
		})
	}
}

func TestBotGroupRegistrationReceiptUsesScopedPathAndTypedAbsence(t *testing.T) {
	installation, operation := testID(21), testID(22)
	seen := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		seen = true
		wantPath := "/v1/bot-runtime/installations/" + installation.String() + "/groups/" + operation.String()
		if r.Method != http.MethodGet || r.URL.Path != wantPath || r.Header.Get("Authorization") != "Bearer device-token" {
			t.Errorf("request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		reply(w, 404, []any{"E_NOT_FOUND", "", nil})
	}, time.Second)
	s, _ := c.Session("device-token")
	_, err := s.BotGroupRegistrationReceipt(context.Background(), installation, operation)
	if !errors.Is(err, ErrRegistrationReceiptNotFound) {
		t.Fatalf("receipt lookup error = %v, want typed absence", err)
	}
	if !seen {
		t.Fatal("receipt lookup did not make a request")
	}

}

func TestBotGroupRegistrationReceiptStrictlyDecodesValidReceipt(t *testing.T) {
	installation, operation, group := testID(21), testID(22), testID(23)
	body := registrationBody(t, group)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		reply(w, http.StatusOK, registrationReceipt(operation, group, body, 17, 9, 1))
	}, time.Second)
	s, _ := c.Session("device-token")
	receipt, err := s.BotGroupRegistrationReceipt(context.Background(), installation, operation)
	if err != nil {
		t.Fatalf("receipt lookup: %v", err)
	}
	if receipt.OperationID != operation || receipt.GroupID != group || receipt.RegistrationSHA256 != sha256.Sum256(registrationHashPreimage(body)) || receipt.AcceptedGeneration != 17 || receipt.InitialEpoch != 9 || receipt.NextSequence != 1 {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestBotGroupRegistrationLostResponseRemainsUnknown(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("test server does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}, time.Second)
	s, _ := c.Session("device-token")
	_, err := s.RegisterBotGroup(context.Background(), testID(21), testID(22), 17, registrationBody(t, testID(23)))
	if !outcomeIs(err, OutcomeUnknown) {
		t.Fatalf("lost response error = %v, want unknown outcome", err)
	}
}

func outcomeIs(err error, want Outcome) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Outcome == want
}

func TestBotGroupRegistrationStatusRulesAreScoped(t *testing.T) {
	post := "bot-runtime/installations/" + testID(21).String() + "/groups"
	get := post + "/" + testID(22).String()
	keypackages := "bot-runtime/installations/" + testID(21).String() + "/keypackages"
	if want, ok := expectedStatus(http.MethodPost, post); !ok || want != http.StatusCreated {
		t.Fatalf("registration status = %d,%v; want 201 route", want, ok)
	}
	if want, ok := expectedStatus(http.MethodGet, get); !ok || want != http.StatusOK {
		t.Fatalf("receipt status = %d,%v; want 200 route", want, ok)
	}
	if !statusAllowed(http.MethodPost, post, http.StatusOK, http.StatusCreated) {
		t.Fatal("exact registration replay status 200 was rejected")
	}
	if statusAllowed(http.MethodPost, keypackages, http.StatusOK, http.StatusCreated) {
		t.Fatal("unexpected KeyPackage replay status 200 was accepted")
	}
	if statusAllowed(http.MethodGet, get, http.StatusCreated, http.StatusOK) {
		t.Fatal("unexpected receipt status 201 was accepted")
	}
}
