//go:build linux

package service

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/native"
)

func TestSpotifyCredentialsWithCompiledHelper(t *testing.T) {
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" {
		t.Skip("set DILLA_BOT_HELPER_BIN for native credential storage integration")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	s, runtime, fake, exchange := spotifyFixture(t)
	ctx := context.Background()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	n, err := native.Start(ctx, helper.Options{Executable: binary, StateDir: filepath.Join(t.TempDir(), "state")}, fake.binding, key)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	s.native = n
	if err := s.ValidateAccount(ctx); err != nil {
		t.Fatalf("empty credential state must be readable: %v", err)
	}
	if err := s.Link(ctx); err != nil {
		t.Fatalf("persist linked credentials: %v", err)
	}
	if _, err := s.Load(ctx); err != nil {
		t.Fatalf("read linked credentials: %v", err)
	}
	if err := s.Link(ctx); err != nil {
		t.Fatalf("repeated claim: %v", err)
	}
	if runtime.calls != 2 || exchange.exchanges != 1 {
		t.Fatal("duplicate authorization was exchanged again")
	}
}
