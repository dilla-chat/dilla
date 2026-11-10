package client

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Bot v2 Task 1: on wire 6 the server's device-list head carries the account-kind attestation as a
// fifth element, as it does for every human client since wire 4. The Bot reads the same head and
// hands its callers the four signed elements, byte-identical to the head it published.
func TestOwnDeviceListAcceptsTheWire6HeadWithItsAttestation(t *testing.T) {
	head := []any{uint64(3), []byte("list"), bytes.Repeat([]byte{0x51}, 64), bytes.Repeat([]byte{0x52}, 32)}
	for name, tc := range map[string]struct {
		body any
		ok   bool
	}{
		"four elements":         {body: head, ok: true},
		"with attestation":      {body: append(append([]any{}, head...), []byte("attestation")), ok: true},
		"attestation not bytes": {body: append(append([]any{}, head...), "attestation"), ok: false},
		"attestation empty":     {body: append(append([]any{}, head...), []byte{}), ok: false},
		"six elements":          {body: append(append([]any{}, head...), []byte("a"), []byte("b")), ok: false},
		// Bot v2 task 20b: the wire-6 head adds the account's umk_pub, ssk_pub and sig_umk_ssk.
		"with identity keys": {body: append(append([]any{}, head...), []byte("attestation"), bytes.Repeat([]byte{1}, 32),
			bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 64)), ok: true},
		"identity key short": {body: append(append([]any{}, head...), []byte("attestation"), bytes.Repeat([]byte{1}, 31),
			bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 64)), ok: false},
		"three elements": {body: head[:3], ok: false},
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, 200, discovery(testID(99)))
					return
				}
				if strings.HasSuffix(r.URL.Path, "/device-list") {
					reply(w, 200, tc.body)
					return
				}
				reply(w, 404, []any{"E_NOT_FOUND", "", nil})
			}, time.Second)
			s, _ := c.Session("device")
			raw, err := s.OwnDeviceList(context.Background(), testID(2))
			if !tc.ok {
				if err == nil {
					t.Fatalf("head %v was accepted", tc.body)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want, _ := encode(head)
			if !bytes.Equal(raw, want) {
				t.Fatalf("head = %x, want the four signed elements %x", raw, want)
			}
		})
	}
}
