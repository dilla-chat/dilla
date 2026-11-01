package client

import (
	"context"
	"net/http"
	"testing"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

// Task 10c fix round 1 (M2): a server that predates Polls and Reminders answers v=2 with eight
// fields and has no switch for them, so the runtime treats both as on; a widened row is taken as sent.
func TestRuntimeListTreatsAnOlderServerAsPollsAndRemindersOn(t *testing.T) {
	installation, community := id.New(), id.New()
	base := []any{installation[:], community[:], uint64(3), uint64(4), nil, nil, uint64(1), uint64(99)}
	for _, tc := range []struct {
		name string
		row  []any
		want uint64
	}{
		{"older server", base, 1 | LegacyServerCapabilities},
		{"widened, both off", append(append([]any(nil), base...), uint64(0), uint64(0), nil, uint64(100), uint64(30)), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, http.StatusOK, discovery(testID(99)))
					return
				}
				body, err := cborx.Marshal([]any{tc.row})
				if err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/cbor")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(body)
			}, 0)
			rt, err := c.Runtime("runtime-token")
			if err != nil {
				t.Fatal(err)
			}
			got, err := rt.ListInstallationsPage(context.Background(), nil)
			if err != nil || len(got) != 1 || got[0].Capabilities != tc.want {
				t.Fatalf("list = %+v, %v; want capabilities %#x", got, err, tc.want)
			}
		})
	}
}
