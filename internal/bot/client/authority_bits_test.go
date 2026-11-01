package client

import (
	"context"
	"net/http"
	"testing"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/permbits"
)

// authorityServer answers every authority route with body.
func authorityServer(t *testing.T, body any) *Session {
	t.Helper()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		raw, err := cborx.Marshal(body)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/cbor")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	}, 0)
	s, err := c.Session("authority-token")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// An owner or administrator resolves to PermAll, which includes Control music
// (bit 25); a DJ holds bit 25 directly. Both must decode (fix round 1, S-H).
func TestAuthorityDecodersAcceptEveryDefinedBit(t *testing.T) {
	community, actor, channel := testID(37), testID(38), testID(39)
	for _, bits := range []uint64{permbits.All, permbits.ControlMusic | permbits.ViewChannel} {
		got, present, err := authorityServer(t, []any{bits, uint64(1), uint64(1), []any{}}).BotActorAuthority(context.Background(), community, actor)
		if err != nil || !present || got.EffectiveBits != bits {
			t.Fatalf("community authority %#x = (%#v, %v, %v)", bits, got, present, err)
		}
		chBits, present, err := authorityServer(t, []any{community, actor, channel, bits}).BotActorChannelAuthority(context.Background(), community, actor, channel)
		if err != nil || !present || chBits != bits {
			t.Fatalf("channel authority %#x = (%#x, %v, %v)", bits, chBits, present, err)
		}
	}
}

func TestAuthorityDecodersRejectTheFirstUndefinedBit(t *testing.T) {
	community, actor, channel := testID(37), testID(38), testID(39)
	undefined := permbits.All + 1 // bit 26
	if _, _, err := authorityServer(t, []any{undefined, uint64(0), uint64(0), []any{}}).BotActorAuthority(context.Background(), community, actor); err == nil {
		t.Fatal("community authority accepted bit 26")
	}
	if _, _, err := authorityServer(t, []any{community, actor, channel, undefined}).BotActorChannelAuthority(context.Background(), community, actor, channel); err == nil {
		t.Fatal("channel authority accepted bit 26")
	}
}
