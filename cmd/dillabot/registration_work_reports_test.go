package main

import (
	"errors"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

func TestRegistrationWorkReportsOnlyStateChanges(t *testing.T) {
	clock := time.Unix(1000, 0)
	r := &changeReports{now: func() time.Time { return clock }}
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

func TestRegistrationWorkReportsRepeatsALongLivedError(t *testing.T) {
	clock := time.Unix(1000, 0)
	r := &changeReports{now: func() time.Time { return clock }}
	key, err := id.New(), errors.New("stuck")
	if !r.changed(key, err) {
		t.Fatal("first report suppressed")
	}
	clock = clock.Add(registrationWorkRepeatAfter - time.Second)
	if r.changed(key, err) {
		t.Fatal("repeated before the interval")
	}
	clock = clock.Add(2 * time.Second)
	if !r.changed(key, err) {
		t.Fatal("not repeated after the interval")
	}
}
