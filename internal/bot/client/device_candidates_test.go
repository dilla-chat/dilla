package client

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

func deviceListFixture(t *testing.T, user byte, version uint64, prev, sig []byte, entries []any) []byte {
	t.Helper()
	return cbor(t, []any{uint64(1), testID(user), version, prev, entries, sig})
}

func candidateEntry(device byte, revoked any) any {
	id := testID(device)
	return []any{id[:], make([]byte, 32), uint64(0), uint64(1), revoked}
}

func TestDeviceCandidatesSortsDeduplicatesAndDropsRevoked(t *testing.T) {
	user, prev, sig := testID(1), make([]byte, 32), make([]byte, 64)
	blob := deviceListFixture(t, 1, 2, prev, sig, []any{
		candidateEntry(4, nil), candidateEntry(2, nil), candidateEntry(4, nil), candidateEntry(3, uint64(9)),
	})
	outer := cbor(t, []any{uint64(2), blob, sig, prev})
	got, err := parseDeviceCandidates(outer, user)
	if err != nil {
		t.Fatal(err)
	}
	want := []id.ID{testID(2), testID(4)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

func TestSessionDeviceCandidatesFetchesCurrentList(t *testing.T) {
	user, prev, sig := testID(1), make([]byte, 32), make([]byte, 64)
	blob := deviceListFixture(t, 1, 2, prev, sig, []any{candidateEntry(5, nil)})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/v1/users/"+user.String()+"/device-list" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			reply(w, http.StatusNotFound, []any{"E_NOT_FOUND", "", nil})
			return
		}
		reply(w, http.StatusOK, []any{uint64(2), blob, sig, prev})
	}, time.Second)
	s, err := c.Session("test-token")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.DeviceCandidates(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	if want := []id.ID{testID(5)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

func TestDeviceCandidatesRejectsOuterInnerMismatch(t *testing.T) {
	user, prev, sig := testID(1), make([]byte, 32), make([]byte, 64)
	blob := deviceListFixture(t, 1, 2, prev, sig, nil)
	for name, outer := range map[string][]byte{
		"version":       cbor(t, []any{uint64(3), blob, sig, prev}),
		"signature":     cbor(t, []any{uint64(2), blob, make([]byte, 64), prev}),
		"previous hash": cbor(t, []any{uint64(2), blob, sig, append([]byte{1}, make([]byte, 31)...)}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDeviceCandidates(outer, user); !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v, want ErrProtocol", err)
			}
		})
	}
}

func TestDeviceCandidatesRejectsWrongUserVersionAndOversizedCount(t *testing.T) {
	user, prev, sig := testID(1), make([]byte, 32), make([]byte, 64)
	for name, blob := range map[string][]byte{
		"wrong user":          deviceListFixture(t, 2, 1, prev, sig, nil),
		"unsupported version": deviceListFixture(t, 1, 1, prev, sig, make([]any, MaxDeviceCandidates+1)),
	} {
		t.Run(name, func(t *testing.T) {
			outer := cbor(t, []any{uint64(1), blob, sig, prev})
			if _, err := parseDeviceCandidates(outer, user); !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v, want ErrProtocol", err)
			}
		})
	}
	bad := cbor(t, []any{uint64(1), deviceListFixture(t, 1, 0, prev, sig, nil), sig, prev})
	if _, err := parseDeviceCandidates(bad, user); !errors.Is(err, ErrProtocol) {
		t.Fatalf("zero version error = %v, want ErrProtocol", err)
	}
}

func TestDeviceCandidatesRejectsNonCanonicalInnerBlob(t *testing.T) {
	user, prev, sig := testID(1), make([]byte, 32), make([]byte, 64)
	blob := cbor(t, []any{uint64(1), user, uint64(1), prev, []any{}, sig})
	// Re-encoding a valid blob is canonical; a trailing byte is rejected by strict decode.
	blob = append(blob, 0)
	outer := cbor(t, []any{uint64(1), blob, sig, prev})
	if _, err := parseDeviceCandidates(outer, user); !errors.Is(err, ErrProtocol) {
		t.Fatalf("error = %v, want ErrProtocol", err)
	}
}
