package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
)

func TestClassNamesEveryHelperFailureWithoutItsText(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{&StatusError{Status: 4}, "status_conflict"},
		{fmt.Errorf("open: %w", &StatusError{Status: 6}), "status_busy"},
		{&StatusError{Status: 42}, "status_unknown"},
		{unknown(os.ErrDeadlineExceeded), "timeout"},
		{context.DeadlineExceeded, "timeout"},
		{fmt.Errorf("acquire: %w", context.DeadlineExceeded), "timeout"},
		{context.Canceled, "canceled"},
		{fmt.Errorf("checkpoint: %w", context.Canceled), "canceled"},
		{unknown(io.EOF), "helper_exited"},
		{unknown(io.ErrUnexpectedEOF), "helper_exited"},
		{unknown(errors.New("response envelope mismatch")), "outcome_unknown"},
		{ErrClosed, "closed"},
		{ErrNotOpened, "not_opened"},
		{&launchError{cause: errors.New("could not start native helper")}, "launch"},
		{fmt.Errorf("start: %w", &launchError{cause: errors.New("helper executable is unavailable")}), "launch"},
		{fmt.Errorf("open: %w", ErrOpenResponse), "open_response"},
		// Not launch failures: before M-1 every unmatched error was called "launch".
		{errors.New("helper client is already open"), "other"},
		{errors.New("invalid helper open parameters"), "other"},
		{errors.New("native open returned an unsupported store response"), "other"},
	} {
		if got := Class(c.err); got != c.want {
			t.Errorf("Class(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// Start's launch failures keep their text and match ErrLaunch.
func TestStartLaunchFailureIsErrLaunch(t *testing.T) {
	_, err := Start(Options{Executable: "relative/helper", StateDir: "relative/state"})
	if !errors.Is(err, ErrLaunch) || Class(err) != "launch" {
		t.Fatalf("Start with relative paths = %v (class %q), want ErrLaunch", err, Class(err))
	}
	if errors.Unwrap(err) == nil || err.Error() != errors.Unwrap(err).Error() {
		t.Fatalf("launch error text changed: %q", err.Error())
	}
}
