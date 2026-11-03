// Package runtime manages opaque operator-registered bot runtime credentials.
package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

var ErrUnauthorized = errors.New("bot runtime: invalid credential")
var ErrCommitUncertain = errors.New("bot runtime: credential commit outcome uncertain")

const tokenPrefix = "dbr_"

// Manager uses the repository's transaction boundary for token activation. The
// secret callback completes its protected write before the transaction commits.
type Manager struct {
	repo   store.Repository
	now    func() time.Time
	random io.Reader
}

func New(repo store.Repository, now func() time.Time) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{repo: repo, now: now, random: rand.Reader}
}

// Create stores the hash and asks writeSecret to durably place the one-time
// plaintext token while the database transaction remains open.
func (m *Manager) Create(ctx context.Context, writeSecret func(id.ID, string) error) (id.ID, error) {
	if writeSecret == nil {
		return id.ID{}, errors.New("bot runtime: secret writer is required")
	}
	runtimeID := id.New()
	token, hash, err := m.mint()
	if err != nil {
		return id.ID{}, err
	}
	at := m.now().Unix()
	callbackSucceeded := false
	err = m.repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: runtimeID, TokenHash: hash, Created: at, Rotated: at}); err != nil {
			return err
		}
		if err := writeSecret(runtimeID, token); err != nil {
			return err
		}
		callbackSucceeded = true
		return nil
	})
	if err != nil {
		if callbackSucceeded {
			return runtimeID, ErrCommitUncertain
		}
		return id.ID{}, err
	}
	return runtimeID, nil
}

// Rotate replaces the hash on the same runtime identity. A stale concurrent
// rotation or a revoked runtime fails its compare-and-swap and rolls back.
func (m *Manager) Rotate(ctx context.Context, runtimeID id.ID, writeSecret func(id.ID, string) error) error {
	if writeSecret == nil {
		return errors.New("bot runtime: secret writer is required")
	}
	token, hash, err := m.mint()
	if err != nil {
		return err
	}
	at := m.now().Unix()
	callbackSucceeded := false
	err = m.repo.Tx(ctx, func(tx store.Repository) error {
		current, err := tx.GetBotRuntime(ctx, runtimeID)
		if err != nil {
			return err
		}
		if current.RevokedAt != nil {
			return store.ErrConflict
		}
		if err := writeSecret(runtimeID, token); err != nil {
			return err
		}
		if err := tx.RotateBotRuntime(ctx, runtimeID, current.TokenHash, hash, at); err != nil {
			return err
		}
		callbackSucceeded = true
		return nil
	})
	if err != nil && callbackSucceeded {
		return ErrCommitUncertain
	}
	return err
}

func (m *Manager) Revoke(ctx context.Context, runtimeID id.ID) error {
	return m.repo.RevokeBotRuntime(ctx, runtimeID, m.now().Unix())
}

// Authenticate accepts only a canonical dbr_ token and returns the active
// runtime identity. No token value is included in returned errors.
func (m *Manager) Authenticate(ctx context.Context, token string) (id.ID, error) {
	_, ok := decodeToken(token)
	if !ok {
		return id.ID{}, ErrUnauthorized
	}
	hash := sha256.Sum256([]byte(token))
	runtime, err := m.repo.GetBotRuntimeByHash(ctx, hash[:])
	if errors.Is(err, store.ErrNotFound) {
		return id.ID{}, ErrUnauthorized
	}
	if err != nil {
		return id.ID{}, err
	}
	if runtime.RevokedAt != nil || subtle.ConstantTimeCompare(runtime.TokenHash, hash[:]) != 1 {
		return id.ID{}, ErrUnauthorized
	}
	return runtime.ID, nil
}

func (m *Manager) mint() (string, []byte, error) {
	var entropy [32]byte
	if _, err := io.ReadFull(m.random, entropy[:]); err != nil {
		return "", nil, errors.New("bot runtime: credential generation failed")
	}
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(entropy[:])
	hash := sha256.Sum256([]byte(token))
	return token, hash[:], nil
}

func decodeToken(token string) ([]byte, bool) {
	if !strings.HasPrefix(token, tokenPrefix) || strings.Contains(token, "=") {
		return nil, false
	}
	encoded := strings.TrimPrefix(token, tokenPrefix)
	if len(encoded) != base64.RawURLEncoding.EncodedLen(32) {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return nil, false
	}
	return raw, true
}
