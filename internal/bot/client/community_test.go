package client

import (
	"context"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
)

func TestSessionCommunityReadsOnlyTheRequestedCommunity(t *testing.T) {
	community := testID(42)
	var gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		gotPath = r.URL.Path
		body, err := cborx.Marshal([]any{community, testID(8), "own", []byte{0xa0}, uint64(7), uint64(0), uint64(0), int64(123)})
		if err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/cbor")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}, 0)
	s, err := c.Session("bot-token")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Community(context.Background(), community)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != community || got.Name != "own" || got.OwnerID != testID(8) || got.PolicyVersion != 7 {
		t.Fatalf("community = %#v", got)
	}
	if gotPath != "/v1/communities/"+community.String() {
		t.Fatalf("path = %q", gotPath)
	}
}
