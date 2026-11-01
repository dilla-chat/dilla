package spotify

import (
	"context"
	"errors"
)

// ErrProviderUnavailable means this installation has no configured integration
// or linked account. A configured provider's credential load failure must use
// ErrCredentialUnavailable instead.
var ErrProviderUnavailable error = providerUnavailable{}

type providerUnavailable struct{}

func (providerUnavailable) Error() string                  { return "spotify: provider unavailable" }
func (providerUnavailable) FailureClass() string           { return "provider_unavailable" }
func (providerUnavailable) PermanentPlaybackFailure() bool { return true }

// credentialFailure keeps transport/authentication causes inspectable while
// ensuring private remote response details never become the error string.
type credentialFailure struct{ cause error }

func (*credentialFailure) Error() string        { return ErrCredentialUnavailable.Error() }
func (e *credentialFailure) Unwrap() error      { return e.cause }
func (*credentialFailure) Is(target error) bool { return target == ErrCredentialUnavailable }
func (e *credentialFailure) PermanentPlaybackFailure() bool {
	return errors.Is(e.cause, ErrProviderUnavailable)
}
func (e *credentialFailure) FailureClass() string {
	if e.PermanentPlaybackFailure() {
		return "provider_unavailable"
	}
	return "credential_unavailable"
}

type helperFailure struct{ phase, code string }

func (e *helperFailure) Error() string {
	return ErrPlaybackFailed.Error() + ": " + e.phase + "/" + e.code
}
func (*helperFailure) Unwrap() error          { return ErrPlaybackFailed }
func (e *helperFailure) FailureClass() string { return e.code }
func (e *helperFailure) PermanentPlaybackFailure() bool {
	return e.phase == "playback" && e.code == "provider_unavailable"
}

// PermanentPlaybackFailure accepts only explicit missing-provider state or the
// bounded playback statuses whose cause belongs to the queued track.
func PermanentPlaybackFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrProviderUnavailable) {
		return true
	}
	var helper *helperFailure
	if errors.As(err, &helper) {
		return helper.PermanentPlaybackFailure()
	}
	// Preserve classification of the previous adapter's bounded enum errors.
	if errors.Is(err, ErrPlaybackFailed) {
		return err.Error() == ErrPlaybackFailed.Error()+": playback/provider_unavailable"
	}
	return false
}

func FailureClass(err error) string {
	if errors.Is(err, ErrProviderUnavailable) {
		return "provider_unavailable"
	}
	var helper *helperFailure
	if errors.As(err, &helper) {
		return helper.FailureClass()
	}
	if errors.Is(err, ErrCredentialUnavailable) {
		return "credential_unavailable"
	}
	if PermanentPlaybackFailure(err) {
		return "provider_unavailable"
	}
	return "unknown"
}
