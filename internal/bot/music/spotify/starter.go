package spotify

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

var ErrCredentialUnavailable = errors.New("spotify: credential unavailable")

// TokenLoader returns the current, installation-bound OAuth access token. It
// must refresh and persist the token before returning when necessary.
type TokenLoader func(context.Context) (string, error)

// PCMStarter starts the pinned librespot helper with one short-lived access
// token. Credentials enter the child only through inherited descriptor 3.
type PCMStarter struct {
	helperPath string
	stateDir   string
	loadToken  TokenLoader
}

func NewPCMStarter(helperPath, stateDir string, loadToken TokenLoader) (*PCMStarter, error) {
	if !filepath.IsAbs(helperPath) || !filepath.IsAbs(stateDir) || loadToken == nil {
		return nil, ErrInvalidConfig
	}
	return &PCMStarter{helperPath: helperPath, stateDir: stateDir, loadToken: loadToken}, nil
}

func (s *PCMStarter) Start(ctx context.Context, track string) (io.ReadCloser, error) {
	if s == nil || ctx == nil || s.loadToken == nil || !validTrackSource(track) {
		return nil, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	token, err := s.loadToken(ctx)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, &credentialFailure{cause: err}
	}
	if token == "" {
		return nil, ErrProviderUnavailable
	}
	credential, err := NewAccessTokenFile(token)
	if err != nil {
		return nil, &credentialFailure{cause: err}
	}
	adapter, err := New(Config{HelperPath: s.helperPath, StateDir: s.stateDir, Credential: credential})
	if err != nil {
		_ = credential.Close()
		return nil, ErrPlaybackFailed
	}
	playback, err := adapter.Start(ctx, track)
	if err != nil {
		_ = credential.Close()
		return nil, err
	}
	return &credentialPlayback{Playback: playback, credential: credential}, nil
}

type credentialPlayback struct {
	*Playback
	credential *os.File
	once       sync.Once
	err        error
}

func (p *credentialPlayback) Close() error {
	if p == nil {
		return nil
	}
	p.once.Do(func() {
		p.err = p.Playback.Close()
		if closeErr := p.credential.Close(); closeErr != nil && p.err == nil {
			p.err = closeErr
		}
	})
	return p.err
}

var _ io.ReadCloser = (*credentialPlayback)(nil)

// Check verifies a linked credential before taking a playback slot or joining a call.
func (s *PCMStarter) Check(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.loadToken == nil {
		return ErrProviderUnavailable
	}
	token, err := s.loadToken(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return &credentialFailure{cause: err}
	}
	if token == "" {
		return ErrProviderUnavailable
	}
	return nil
}
