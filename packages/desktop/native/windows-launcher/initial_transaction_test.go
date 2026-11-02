package main

import (
	"os"
	"testing"
)

func TestInitialSeedClassificationNeverRequarantinesExactAuthority(t *testing.T) {
	expected := []byte(`{"format":1,"kind":"initial","generation":0}`)
	zeroTail := append([]byte(nil), expected...)
	for i := 12; i < len(zeroTail); i++ {
		zeroTail[i] = 0
	}
	for _, tc := range []struct {
		name                   string
		actual                 []byte
		readErr                error
		complete, otherHistory bool
		want                   initialRetryDisposition
	}{
		{"complete committed", expected, nil, true, false, initialFinalizeOnly},
		{"exact seed damaged program", expected, nil, false, false, initialHold},
		{"exact seed other history", expected, nil, true, true, initialHold},
		{"proper prefix new attempt", expected[:12], nil, false, false, initialRepair},
		{"zero tail new attempt", zeroTail, nil, false, false, initialRepair},
		{"empty interrupted copy", []byte{}, nil, false, false, initialRepair},
		{"absent seed", nil, os.ErrNotExist, false, false, initialRepair},
		{"unreadable", nil, os.ErrPermission, false, false, initialHold},
		{"arbitrary malformed", []byte("not initial metadata"), nil, false, false, initialHold},
		{"prefix with history", expected[:12], nil, false, true, initialHold},
		{"overlong", append(append([]byte(nil), expected...), 0), nil, false, false, initialHold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyInitialSeed(expected, tc.actual, tc.readErr, tc.complete, tc.otherHistory); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInitialPublicationNeverRollsBackVisibleOrUncertainAuthority(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outcome  initialPublicationOutcome
		observed initialSeedObservation
		want     initialCommitState
	}{
		{"visible", initialPublicationCompleted, initialSeedExact, initialCommitted},
		{"published result lost", initialPublicationUnknown, initialSeedExact, initialCommitted},
		{"flush failed after visibility", initialPublicationUnknown, initialSeedExact, initialCommitted},
		{"read result lost", initialPublicationUnknown, initialSeedUnknown, initialCommitUncertain},
		{"unexpected final seed", initialPublicationUnknown, initialSeedUnexpected, initialCommitUncertain},
		{"success but disappeared", initialPublicationCompleted, initialSeedAbsent, initialCommitUncertain},
		{"definitive prepublication refusal", initialPublicationNotInvoked, initialSeedAbsent, initialPrecommit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := initialPrecommit
			state = initialPublishAttempted
			state = reconcileInitialPublication(state, tc.outcome, tc.observed)
			if state != tc.want {
				t.Fatalf("got %v, want %v", state, tc.want)
			}
			if initialRollbackAllowed(state) != (tc.want == initialPrecommit) {
				t.Fatal("unsafe rollback decision")
			}
			if state == initialCommitted {
				// A later close/flush failure cannot turn visible authority into rollback.
				if got := reconcileInitialPublication(state, initialPublicationUnknown, initialSeedUnknown); got != initialCommitted {
					t.Fatalf("forgot visible authority: %v", got)
				}
			}
		})
	}
	if initialRollbackAllowed(initialPublishAttempted) {
		t.Fatal("unreconciled publication permits rollback")
	}
	if initialRollbackAllowed(initialCommitUncertain) {
		t.Fatal("uncertain publication permits rollback")
	}
}

func TestInitialSeedClassificationRequiresPreparedExpectedSeed(t *testing.T) {
	for _, expected := range [][]byte{nil, {}} {
		for _, complete := range []bool{false, true} {
			for _, readErr := range []error{nil, os.ErrNotExist} {
				if got := classifyInitialSeed(expected, nil, readErr, complete, false); got != initialHold {
					t.Fatalf("unprepared expected seed classified %v", got)
				}
			}
		}
	}
}

func TestAmbiguousPublicationErrorAndAbsentObservationStillHold(t *testing.T) {
	if got := reconcileInitialPublication(initialPublishAttempted, initialPublicationUnknown, initialSeedAbsent); got != initialCommitUncertain {
		t.Fatalf("ambiguous publication permits rollback: %v", got)
	}
}
