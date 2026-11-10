package helper

import (
	"errors"
	"io"
	"os"
	"strings"
)

// Class names why a helper start, open or call failed, as a fixed, non-secret word for logs. Without it an operator
// sees only that the helper failed, never whether it timed out, exited, refused the store or could not be launched.
func Class(err error) string {
	var status *StatusError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &status):
		return "status_" + strings.TrimPrefix(status.Error(), "helper status: ")
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "helper_exited"
	case errors.Is(err, ErrOutcomeUnknown):
		return "outcome_unknown"
	case errors.Is(err, ErrClosed):
		return "closed"
	case errors.Is(err, ErrNotOpened):
		return "not_opened"
	case strings.HasPrefix(err.Error(), "native open returned"):
		return "open_response"
	default:
		return "launch"
	}
}
