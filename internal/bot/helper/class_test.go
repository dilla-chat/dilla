package helper

import (
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
		{unknown(io.EOF), "helper_exited"},
		{unknown(io.ErrUnexpectedEOF), "helper_exited"},
		{unknown(errors.New("response envelope mismatch")), "outcome_unknown"},
		{ErrClosed, "closed"},
		{ErrNotOpened, "not_opened"},
		{errors.New("native open returned an unsupported store response"), "open_response"},
		{errors.New("could not start native helper"), "launch"},
	} {
		if got := Class(c.err); got != c.want {
			t.Errorf("Class(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
