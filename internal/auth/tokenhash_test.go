package auth_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
)

// TokenHash is the one spelling of "what the sessions table stores for a bearer token": `dillad
// admin` looks a session up by the token an operator was handed, and finds it only if this is what
// minting wrote.
func TestTokenHashIsWhatMintingStoresAndWhatResolveLooksUp(t *testing.T) {
	sum := sha256.Sum256([]byte("abc"))
	if !bytes.Equal(auth.TokenHash("abc"), sum[:]) {
		t.Fatal("TokenHash is not SHA-256 of the token's text")
	}
	s, repo, clk := newSessions(t)
	ctx := context.Background()
	_, device, priv := seedDevice(t, repo)
	nonce, _, _ := s.Challenge(ctx, device)
	tok, err := s.Establish(ctx, signed(t, s, device, nonce, auth.PurposeSession, priv))
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	row, err := repo.GetSessionByHash(ctx, auth.TokenHash(tok.Token), clk.Now().Unix())
	if err != nil {
		t.Fatalf("the minted session is not stored under TokenHash(token): %v", err)
	}
	if row.DeviceID != device {
		t.Fatalf("session row = %+v", row)
	}
	resolved, err := s.Resolve(ctx, tok.Token)
	if err != nil || !bytes.Equal(resolved.TokenHash, auth.TokenHash(tok.Token)) {
		t.Fatalf("Resolve = %+v, %v", resolved, err)
	}
}
