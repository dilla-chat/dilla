package main

import (
	"bytes"
	"os"
)

type initialRetryDisposition uint8

const (
	initialHold initialRetryDisposition = iota
	initialRepair
	initialFinalizeOnly
)

// The caller has already authenticated the fixed marker, package subset and
// expected seed. Prefix repair applies only to this explicit installer attempt.
// Exact seed bytes never fall through to repair when the program is damaged.
func classifyInitialSeed(expected, actual []byte, readErr error, complete, otherHistory bool) initialRetryDisposition {
	if len(expected) == 0 || otherHistory {
		return initialHold
	}
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return initialRepair
		}
		return initialHold
	}
	if bytes.Equal(expected, actual) {
		if complete {
			return initialFinalizeOnly
		}
		return initialHold
	}
	if len(actual) > len(expected) {
		return initialHold
	}
	zeroTail := false
	for i, value := range actual {
		if !zeroTail && value == expected[i] {
			continue
		}
		if value != 0 {
			return initialHold
		}
		zeroTail = true
	}
	return initialRepair
}

type initialCommitState uint8

const (
	initialPrecommit initialCommitState = iota
	initialPublishAttempted
	initialCommitted
	initialCommitUncertain
)

type initialSeedObservation uint8

const (
	initialSeedUnknown initialSeedObservation = iota
	initialSeedAbsent
	initialSeedExact
	initialSeedUnexpected
)

func initialRollbackAllowed(state initialCommitState) bool { return state == initialPrecommit }

// Publication is reconciled against the held canonical root, never inferred from
// the syscall's return alone. Once exact authority was observed, later errors
// cannot turn it back into an uncommitted transaction.
type initialPublicationOutcome uint8

const (
	// Zero/default, a syscall error or a lost result never proves no publication.
	initialPublicationUnknown initialPublicationOutcome = iota
	// Set only when the adapter definitively DID NOT INVOKE the publication call.
	// An observed absence after a syscall/flush/close error cannot select this.
	initialPublicationNotInvoked
	initialPublicationCompleted
)

func reconcileInitialPublication(state initialCommitState, outcome initialPublicationOutcome, observed initialSeedObservation) initialCommitState {
	if state == initialCommitted {
		return initialCommitted
	}
	if observed == initialSeedExact {
		return initialCommitted
	}
	if state == initialPublishAttempted && outcome == initialPublicationNotInvoked && observed == initialSeedAbsent {
		return initialPrecommit
	}
	return initialCommitUncertain
}
