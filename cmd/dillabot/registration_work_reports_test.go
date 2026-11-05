package main

import (
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

func TestRegistrationWorkReportsOnlyStateChanges(t *testing.T) {
	r := &changeReports{}
	key := id.New()
	a, b := errors.New("a"), errors.New("b")
	for i, tc := range []struct {
		err  error
		want bool
	}{{a, true}, {a, false}, {b, true}, {nil, false}, {b, true}} {
		if got := r.changed(key, tc.err); got != tc.want {
			t.Fatalf("step %d: changed = %v, want %v", i, got, tc.want)
		}
	}
}
