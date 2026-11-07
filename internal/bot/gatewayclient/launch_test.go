package gatewayclient

import (
	"context"
	"encoding/base64"
	"testing"
	"time"
)

// The launch token goes on identifies until one reaches ready; identifies that might carry it take turns; a refused
// one leaves it for the next; a network Bot (nil) never sends one.
func TestLaunchTokenIsSentUntilAnIdentifyIsReady(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	want := base64.RawURLEncoding.EncodeToString(raw)
	tok, err := NewLaunchToken(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range raw {
		if b != 0 {
			t.Fatal("NewLaunchToken left the caller's copy of the token")
		}
	}
	ctx := context.Background()
	v, settle, err := tok.acquire(ctx)
	if err != nil || v != want {
		t.Fatalf("first identify carries %q, %v; want the token", v, err)
	}
	// A second identify waits its turn while the first one holds the token.
	second := make(chan string, 1)
	go func() {
		v, settle2, _ := tok.acquire(ctx)
		settle2(false)
		second <- v
	}()
	select {
	case v := <-second:
		t.Fatalf("a second identify got %q while the first held the token", v)
	case <-time.After(50 * time.Millisecond):
	}
	settle(false) // refused: the token is still good
	if v := <-second; v != want {
		t.Fatalf("after a refusal the next identify carries %q, want the token", v)
	}
	v, settle, _ = tok.acquire(ctx)
	settle(true)
	settle(true) // idempotent
	if v != want {
		t.Fatalf("got %q", v)
	}
	for range 3 {
		if v, s, _ := tok.acquire(ctx); v != "" {
			t.Fatalf("after ready an identify still carries %q", v)
		} else {
			s(false)
		}
	}
	var none *LaunchToken
	if v, s, err := none.acquire(ctx); v != "" || err != nil {
		t.Fatalf("a network Bot's identify carries %q, %v", v, err)
	} else {
		s(true)
	}
	if _, err := NewLaunchToken(make([]byte, 31)); err == nil {
		t.Fatal("a 31-byte token was accepted")
	}
}
