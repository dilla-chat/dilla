package helper

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
)

var (
	// ErrLaunch matches every error of Start that came from launching the helper process (fix round, M-1).
	ErrLaunch = errors.New("native helper could not be launched")
	// ErrOpenResponse matches an Open whose answer the caller could not accept: a store version or identity it does not
	// recognise. The native package wraps its open-response errors with it.
	ErrOpenResponse = errors.New("native helper open response is invalid")
)

// launchError keeps launch's own text and matches ErrLaunch.
type launchError struct{ cause error }

func (e *launchError) Error() string        { return e.cause.Error() }
func (e *launchError) Unwrap() error        { return e.cause }
func (e *launchError) Is(target error) bool { return target == ErrLaunch }

// Class names why a helper start, open or call failed, as a fixed, non-secret word for logs. Without it an operator
// sees only that the helper failed, never whether it timed out, exited, refused the store or could not be launched.
// An error that matches none of the known causes is "other", never a guess.
func Class(err error) string {
	var status *StatusError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &status):
		return "status_" + strings.TrimPrefix(status.Error(), "helper status: ")
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "helper_exited"
	case errors.Is(err, ErrLaunch):
		return "launch"
	case errors.Is(err, ErrOpenResponse):
		return "open_response"
	case errors.Is(err, ErrOutcomeUnknown):
		return "outcome_unknown"
	case errors.Is(err, ErrClosed):
		return "closed"
	case errors.Is(err, ErrNotOpened):
		return "not_opened"
	default:
		return "other"
	}
}
