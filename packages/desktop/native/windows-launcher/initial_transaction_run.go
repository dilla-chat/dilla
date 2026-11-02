package main

import "errors"

// Each operation is fixed-layout and local to one initial-retry transaction.
// Platform adapters retain all handles; this driver owns the authority/rollback
// order. No callback accepts a caller path or arbitrary operation description.
type initialRetryOperations interface {
	prepare() (initialRetryDisposition, error)
	quarantineProgram() error
	leaseArchive() error
	recheck() error
	quarantineUpdates() error
	copyAndVerify() error
	publish() (initialPublicationOutcome, error)
	observe() (initialSeedObservation, error)
	flush() error
	closeCreated() error
	rollbackCreated() error
	closeArchive() error
	restoreArchives() error
}

func runInitialRetryTransaction(operations initialRetryOperations) (state initialCommitState, err error) {
	disposition, err := operations.prepare()
	if err != nil {
		return initialPrecommit, err
	}
	if disposition == initialFinalizeOnly {
		return initialCommitted, nil
	}
	if disposition != initialRepair {
		return initialPrecommit, errors.New("initial retry authority is not established")
	}
	state = initialPrecommit
	defer func() {
		if err != nil && initialRollbackAllowed(state) {
			// Created-object deletion uses retained claims; archive leases are separately
			// released before restoring directory names. Never reverse these lifetimes.
			err = errors.Join(err, operations.rollbackCreated(), operations.closeArchive(), operations.restoreArchives())
		} else {
			err = errors.Join(err, operations.closeCreated(), operations.closeArchive())
		}
	}()
	for _, step := range []func() error{operations.quarantineProgram, operations.leaseArchive, operations.recheck, operations.quarantineUpdates, operations.copyAndVerify} {
		if err = step(); err != nil {
			return state, err
		}
	}
	state = initialPublishAttempted
	outcome, publishErr := operations.publish()
	observed, observeErr := operations.observe()
	if observeErr != nil && observed != initialSeedExact {
		observed = initialSeedUnknown
	}
	state = reconcileInitialPublication(state, outcome, observed)
	if state != initialCommitted {
		return state, errors.Join(publishErr, observeErr, errors.New("initial seed publication is not established"))
	}
	return state, errors.Join(publishErr, observeErr, operations.flush())
}
