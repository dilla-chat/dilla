package client

import (
	"context"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
)

func TestSessionBotActorAuthorityParsesExactAuthenticatedTuple(t *testing.T) {
	community, actor := testID(37), testID(38)
	var gotPath, gotAuth string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/cbor")
		body, marshalErr := cborx.Marshal([]any{uint64(1<<16 | 1<<2), uint64(1), uint64(1), []any{testID(40)}})
		if marshalErr != nil {
			t.Error(marshalErr)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}, 0)
	s, err := c.Session("authority-token")
	if err != nil {
		t.Fatal(err)
	}
	got, present, err := s.BotActorAuthority(context.Background(), community, actor)
	if err != nil || !present || got.EffectiveBits != (1<<16|1<<2) || !got.Owner || !got.Administrator || len(got.RoleIDs) != 1 || got.RoleIDs[0] != testID(40) {
		t.Fatalf("authority = (%#v, %v, %v)", got, present, err)
	}
	if gotPath != "/v1/communities/"+community.String()+"/bot/actors/"+actor.String()+"/authority" || gotAuth != "Bearer authority-token" {
		t.Fatalf("request = %q, %q", gotPath, gotAuth)
	}
	_, _, err = s.BotActorAuthority(context.Background(), community, actor)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSessionBotActorAuthorityRejectsMalformedTuplesAndHides404(t *testing.T) {
	community, actor := testID(37), testID(38)
	for _, tc := range []struct {
		name    string
		body    any
		status  int
		present bool
	}{
		{name: "extra field", body: []any{uint64(4), uint64(0), uint64(0), []any{}, uint64(1)}, status: http.StatusOK, present: false},
		{name: "unknown bit", body: []any{uint64(1 << 30), uint64(0), uint64(0), []any{}}, status: http.StatusOK, present: false},
		{name: "invalid flag", body: []any{uint64(4), uint64(2), uint64(0), []any{}}, status: http.StatusOK, present: false},
		{name: "invalid role", body: []any{uint64(4), uint64(0), uint64(0), []any{"bad"}}, status: http.StatusOK, present: false},
		{name: "absent", status: http.StatusNotFound, present: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, http.StatusOK, discovery(testID(99)))
					return
				}
				if tc.status == http.StatusNotFound {
					reply(w, tc.status, []any{"E_NOT_FOUND", "no such object", nil})
					return
				}
				body, err := cborx.Marshal(tc.body)
				if err != nil {
					t.Fatal(err)
				}
				w.Header().Set("Content-Type", "application/cbor")
				w.WriteHeader(tc.status)
				_, _ = w.Write(body)
			}, 0)
			s, err := c.Session("authority-token")
			if err != nil {
				t.Fatal(err)
			}
			got, present, err := s.BotActorAuthority(context.Background(), community, actor)
			if tc.status == http.StatusOK && err == nil {
				t.Fatalf("malformed tuple accepted: %#v", got)
			}
			if tc.status == http.StatusNotFound && (err != nil || present || got.EffectiveBits != 0 || got.Owner || got.Administrator || len(got.RoleIDs) != 0) {
				t.Fatalf("404 = (%#v, %v, %v)", got, present, err)
			}
		})
	}
}
