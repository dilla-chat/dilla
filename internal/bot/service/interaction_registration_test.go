package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

func TestInteractionRegistrationPersistsAndReturnsPendingWelcomeAfterReceipt(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	got, err := f.coordinator.Register(context.Background(), f.request)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got.Receipt.OperationID.IsZero() || len(got.PendingWelcome) == 0 || got.Usable {
		t.Fatalf("result must carry a pending Welcome without claiming usability: %+v", got)
	}
	if f.n.createCalls != 1 || f.s.registerCalls != 1 || f.n.registered != 1 {
		t.Fatalf("create/register/native-resolve calls = %d/%d/%d", f.n.createCalls, f.s.registerCalls, f.n.registered)
	}
	if len(f.s.requestIDs) != 3 || f.s.requestIDs[0].IsZero() || f.s.requestIDs[0] != f.s.requestIDs[1] || f.s.requestIDs[0] != f.s.requestIDs[2] {
		t.Fatalf("admission, context, and registration request IDs differ: %v", f.s.requestIDs)
	}
}

func TestInteractionRegistrationRecoversLostResponseAfterRestartWithoutRecreate(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.s.storeThenLoseResponse = true
	if _, err := f.coordinator.Register(context.Background(), f.request); !errors.Is(err, ErrInteractionRegistrationOutcomeUnknown) {
		t.Fatalf("first Register error = %v, want unknown outcome", err)
	}
	f.coordinator, _ = NewInteractionRegistrationCoordinator(f.instance, f.community, f.install, f.n, f.d, f.s)
	got, err := f.coordinator.Register(context.Background(), f.request)
	if err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
	if f.n.createCalls != 1 || f.s.registerCalls != 1 || f.s.receiptCalls != 3 || len(got.PendingWelcome) == 0 || got.Usable {
		t.Fatalf("recovery duplicated work or lost pending Welcome: creates=%d registers=%d receipts=%d result=%+v", f.n.createCalls, f.s.registerCalls, f.s.receiptCalls, got)
	}
}

func TestInteractionRegistrationStopsForManualRecoveryAfterLateCommitRace(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.s.dropRegistrationResponse = true
	f.s.lateCommitAfterAbsence = true
	if _, err := f.coordinator.Register(context.Background(), f.request); !errors.Is(err, ErrInteractionRegistrationOutcomeUnknown) {
		t.Fatalf("first Register error = %v, want unknown outcome", err)
	}
	oldOperation, oldAdmission := f.s.lastOperation, f.s.requestIDs[0]
	f.s.currentTime = 131 // original admission expired at server time 130
	if _, err := f.coordinator.Register(context.Background(), f.request); !errors.Is(err, ErrInteractionRegistrationManualRecovery) {
		t.Fatalf("expired lost-response retry = %v, want explicit manual recovery", err)
	}
	if f.s.receipt == nil || f.s.receipt.OperationID != oldOperation {
		t.Fatal("adversarial fake did not commit original POST after absence observations")
	}
	if f.s.registerCalls != 1 || f.n.createCalls != 1 || len(f.n.discarded) != 0 {
		t.Fatalf("late commit race caused duplicate POST or native discard: posts=%d creates=%d discarded=%v", f.s.registerCalls, f.n.createCalls, f.n.discarded)
	}
	value, err := f.n.StateGet(context.Background(), interactionRegistrationStateArea, interactionRegistrationKey(f.request.Group))
	if err != nil {
		t.Fatal(err)
	}
	intent, err := decodeInteractionRegistrationIntent(value.Value)
	if err != nil || !intent.ManualRecovery || !intent.Submitted || intent.Operation != oldOperation || intent.AdmissionRequest != oldAdmission || len(intent.Body) == 0 {
		t.Fatalf("manual recovery did not preserve exact staged operation/body: intent=%+v err=%v", intent, err)
	}
	if _, err := f.coordinator.Register(context.Background(), f.request); !errors.Is(err, ErrInteractionRegistrationManualRecovery) {
		t.Fatalf("subsequent retry = %v, want stable manual-recovery state", err)
	}
	if f.s.registerCalls != 1 || len(f.n.discarded) != 0 {
		t.Fatalf("terminal retry repeated mutation: posts=%d discarded=%v", f.s.registerCalls, f.n.discarded)
	}
}

func TestInteractionRegistrationRejectsConflictingReuse(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	_, err := f.coordinator.Register(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	conflict := f.request
	conflict.ParticipantDevice = id.New()
	if _, err := f.coordinator.Register(context.Background(), conflict); !errors.Is(err, ErrInteractionRegistrationRecovery) {
		t.Fatalf("conflicting reuse error = %v, want recovery refusal", err)
	}
	if f.n.createCalls != 1 || f.s.registerCalls != 1 {
		t.Fatalf("conflict caused side effects: create=%d register=%d", f.n.createCalls, f.s.registerCalls)
	}
}

func TestInteractionRegistrationRejectsStaleContextWithoutPosting(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.n.createErr = errors.New("snapshot expired")
	if _, err := f.coordinator.Register(context.Background(), f.request); !errors.Is(err, ErrInteractionRegistrationRecovery) {
		t.Fatalf("stale context error = %v, want fail-closed recovery", err)
	}
	if f.n.createCalls != 1 || f.s.registerCalls != 0 || f.n.registered != 0 {
		t.Fatalf("stale context side effects: create=%d register=%d native-resolve=%d", f.n.createCalls, f.s.registerCalls, f.n.registered)
	}
	value, err := f.n.StateGet(context.Background(), interactionRegistrationStateArea, interactionRegistrationKey(f.request.Group))
	if err != nil {
		t.Fatal(err)
	}
	intent, err := decodeInteractionRegistrationIntent(value.Value)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Operation != f.n.createOperations[0] || intent.AdmissionRequest != f.s.requestIDs[0] || len(intent.Snapshot) == 0 {
		t.Fatalf("uncertain create error rotated pinned durable intent: %+v", intent)
	}
}

func TestInteractionRegistrationRenewsExpiredCreateContext(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.n.createErr = &helper.StatusError{Status: 3}
	if _, err := f.coordinator.Register(context.Background(), f.request); !errors.Is(err, ErrInteractionRegistrationRecovery) {
		t.Fatalf("expired create context error = %v, want recoverable pending state", err)
	}
	if len(f.s.requestIDs) != 2 {
		t.Fatalf("first admission/context request IDs = %v", f.s.requestIDs)
	}
	oldRequest, operation := f.s.requestIDs[0], f.n.createOperations[0]
	value, err := f.n.StateGet(context.Background(), interactionRegistrationStateArea, interactionRegistrationKey(f.request.Group))
	if err != nil {
		t.Fatal(err)
	}
	intent, err := decodeInteractionRegistrationIntent(value.Value)
	if err != nil {
		t.Fatal(err)
	}
	if intent.AdmissionRequest == oldRequest || intent.AdmissionRequest.IsZero() || intent.Operation == operation || intent.Operation.IsZero() || len(intent.Admission) != 0 || len(intent.Snapshot) != 0 || intent.Group != f.request.Group || intent.ParticipantUser != f.request.ParticipantUser || intent.ParticipantDevice != f.request.ParticipantDevice || !bytes.Equal(intent.KeyPackage, f.request.ParticipantKeyPackage) {
		t.Fatalf("expired admission was not safely renewed: request=%v intent=%+v", oldRequest, intent)
	}
	newOperation := intent.Operation
	if intent.MinimumRevision != 0 {
		t.Fatalf("expired snapshot revision was retained: %d", intent.MinimumRevision)
	}
	f.n.createErr = nil
	f.s.minimumRevision = 5 // Server membership advanced before the renewed admission.
	if _, err := f.coordinator.Register(context.Background(), f.request); err != nil {
		t.Fatalf("Register with renewed context (creates=%d requests=%v): %v", f.n.createCalls, f.s.requestIDs, err)
	}
	if f.n.createCalls != 2 || f.s.registerCalls != 1 || f.s.lastOperation != newOperation || len(f.s.requestIDs) != 5 {
		t.Fatalf("renewed registration duplicated or lost durable intent: creates=%d posts=%d operation=%v request IDs=%v", f.n.createCalls, f.s.registerCalls, f.s.lastOperation, f.s.requestIDs)
	}
	if f.n.discarded != nil {
		t.Fatalf("definitive expired retry discarded groups: %v", f.n.discarded)
	}
	newRequest := f.s.requestIDs[2]
	if newRequest == oldRequest || f.s.requestIDs[3] != newRequest || f.s.requestIDs[4] != newRequest {
		t.Fatalf("renewed admission/context/register IDs differ: %v", f.s.requestIDs)
	}
	value, err = f.n.StateGet(context.Background(), interactionRegistrationStateArea, interactionRegistrationKey(f.request.Group))
	if err != nil {
		t.Fatal(err)
	}
	intent, err = decodeInteractionRegistrationIntent(value.Value)
	if err != nil || intent.MinimumRevision != 5 {
		t.Fatalf("refreshed eligibility revision was not adopted: minimum=%d err=%v", intent.MinimumRevision, err)
	}
}

func TestInteractionRegistrationCompletedRetryDoesNotRecreate(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	if _, err := f.coordinator.Register(context.Background(), f.request); err != nil {
		t.Fatal(err)
	}
	if _, err := f.coordinator.Register(context.Background(), f.request); err != nil {
		t.Fatal(err)
	}
	if f.n.createCalls != 1 {
		t.Fatalf("native create calls=%d, want 1", f.n.createCalls)
	}
}

func TestInteractionRegistrationRecoveryDiagnosticsAreReadOnly(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	intent := pendingInteractionRecoveryIntent(f, []byte{1, 2, 3})
	putInteractionRecoveryIntent(t, f, intent)
	f.s.receipt = &client.BotGroupRegistrationReceipt{OperationID: intent.Operation, GroupID: intent.Group, RegistrationSHA256: interactionRegistrationHash(intent.Body), AcceptedGeneration: intent.Generation, InitialEpoch: 1, NextSequence: 1}

	got, err := f.coordinator.InspectInteractionRegistrationRecovery(context.Background())
	if err != nil {
		t.Fatalf("InspectInteractionRegistrationRecovery: %v", err)
	}
	if len(got) != 1 || got[0].Operation != intent.Operation || got[0].Group != intent.Group || got[0].ReceiptStatus != "present" || got[0].GroupStatus != "absent" {
		t.Fatalf("diagnostic = %+v", got)
	}
	if f.n.registered != 0 || f.s.registerCalls != 0 {
		t.Fatalf("diagnostic caused mutation: registered=%d posts=%d", f.n.registered, f.s.registerCalls)
	}
}

func TestInteractionRegistrationManualRecoveryRequiresExactAuthoritativeReceipt(t *testing.T) {
	t.Run("matching receipt resolves without repost", func(t *testing.T) {
		f := newInteractionRegistrationFixture(t)
		intent := pendingInteractionRecoveryIntent(f, []byte{1, 2, 3})
		putInteractionRecoveryIntent(t, f, intent)
		r := client.BotGroupRegistrationReceipt{OperationID: intent.Operation, GroupID: intent.Group, RegistrationSHA256: interactionRegistrationHash(intent.Body), AcceptedGeneration: intent.Generation, InitialEpoch: 1, NextSequence: 1}
		f.s.receipt = &r

		got, err := f.coordinator.RecoverInteractionRegistration(context.Background(), intent.Group)
		if err != nil || got.Receipt != r || got.Usable || f.n.registered != 1 || f.s.registerCalls != 0 {
			t.Fatalf("recovery result=%+v err=%v registered=%d posts=%d", got, err, f.n.registered, f.s.registerCalls)
		}
		v, err := f.n.StateGet(context.Background(), interactionRegistrationStateArea, interactionRegistrationKey(intent.Group))
		if err != nil {
			t.Fatal(err)
		}
		saved, err := decodeInteractionRegistrationIntent(v.Value)
		if err != nil || saved.Phase != 2 || saved.ManualRecovery {
			t.Fatalf("saved recovery state=%+v err=%v", saved, err)
		}
	})

	for _, mutate := range []struct {
		name   string
		change func(*client.BotGroupRegistrationReceipt)
	}{
		{"operation", func(r *client.BotGroupRegistrationReceipt) { r.OperationID = id.New() }},
		{"group", func(r *client.BotGroupRegistrationReceipt) { r.GroupID = id.New() }},
		{"body hash", func(r *client.BotGroupRegistrationReceipt) { r.RegistrationSHA256[0] ^= 1 }},
		{"generation", func(r *client.BotGroupRegistrationReceipt) { r.AcceptedGeneration++ }},
	} {
		t.Run("reject mismatched "+mutate.name, func(t *testing.T) {
			f := newInteractionRegistrationFixture(t)
			intent := pendingInteractionRecoveryIntent(f, []byte{4, 5, 6})
			putInteractionRecoveryIntent(t, f, intent)
			r := client.BotGroupRegistrationReceipt{OperationID: intent.Operation, GroupID: intent.Group, RegistrationSHA256: interactionRegistrationHash(intent.Body), AcceptedGeneration: intent.Generation, InitialEpoch: 1, NextSequence: 1}
			mutate.change(&r)
			f.s.receipt = &r
			f.s.ignoreReceiptOperation = true
			if _, err := f.coordinator.RecoverInteractionRegistration(context.Background(), intent.Group); !errors.Is(err, ErrInteractionRegistrationRecovery) {
				t.Fatalf("recovery error=%v, want exact receipt refusal", err)
			}
			if f.n.registered != 0 || f.s.registerCalls != 0 {
				t.Fatalf("mismatched receipt caused side effects: native=%d posts=%d", f.n.registered, f.s.registerCalls)
			}
		})
	}
}

func TestInteractionRegistrationReadmitsManualIntentWithSameGroupAndOperation(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.s.dropRegistrationResponse = true
	if _, err := f.coordinator.Register(context.Background(), f.request); !errors.Is(err, ErrInteractionRegistrationOutcomeUnknown) {
		t.Fatalf("lost registration response = %v, want unknown outcome", err)
	}
	oldOperation, oldRequest := f.s.lastOperation, f.s.requestIDs[0]
	f.s.currentTime = 131
	if _, err := f.coordinator.Register(context.Background(), f.request); !errors.Is(err, ErrInteractionRegistrationManualRecovery) {
		t.Fatalf("expired admission = %v, want manual recovery", err)
	}
	f.s.dropRegistrationResponse = false
	got, err := f.coordinator.ReadmitInteractionRegistration(context.Background(), f.request.Group)
	if err != nil {
		t.Fatalf("ReadmitInteractionRegistration: %v", err)
	}
	if got.Receipt.OperationID != oldOperation || got.Receipt.GroupID != f.request.Group || got.Usable ||
		f.n.createCalls != 1 || f.n.registered != 1 || f.s.registerCalls != 2 || f.s.lastOperation != oldOperation {
		t.Fatalf("readmission changed the staged MLS operation: result=%+v creates=%d registered=%d posts=%d last_op=%s",
			got, f.n.createCalls, f.n.registered, f.s.registerCalls, f.s.lastOperation)
	}
	if len(f.s.requestIDs) != 6 || f.s.requestIDs[3] == oldRequest || f.s.requestIDs[3] != f.s.requestIDs[4] || f.s.requestIDs[4] != f.s.requestIDs[5] {
		t.Fatalf("readmission did not use one fresh authorization: requests=%v", f.s.requestIDs)
	}
	value, err := f.n.StateGet(context.Background(), interactionRegistrationStateArea, interactionRegistrationKey(f.request.Group))
	if err != nil {
		t.Fatal(err)
	}
	intent, err := decodeInteractionRegistrationIntent(value.Value)
	if err != nil || intent.Phase != 2 || intent.ManualRecovery || intent.Operation != oldOperation {
		t.Fatalf("durable intent was not resolved: intent=%+v err=%v", intent, err)
	}
}

func TestInteractionRegistrationReadmissionRefusesChangedOrMissingAuthority(t *testing.T) {
	for _, variant := range []string{"generation", "policy", "identity", "native group", "native state", "admission"} {
		t.Run(variant, func(t *testing.T) {
			f := newInteractionRegistrationFixture(t)
			f.s.dropRegistrationResponse = true
			if _, err := f.coordinator.Register(context.Background(), f.request); !errors.Is(err, ErrInteractionRegistrationOutcomeUnknown) {
				t.Fatalf("initial outcome = %v", err)
			}
			f.s.currentTime = 131
			if _, err := f.coordinator.Register(context.Background(), f.request); !errors.Is(err, ErrInteractionRegistrationManualRecovery) {
				t.Fatalf("manual recovery = %v", err)
			}
			switch variant {
			case "generation":
				f.d.value.Generation++
			case "policy":
				f.d.value.PolicyVersion++
			case "identity":
				f.n.identity.Device = new(id.ID)
			case "native group":
				f.n.createCalls = 0
			case "native state":
				f.n.inspectState = 2
			case "admission":
				f.s.admissionErr = errors.New("not eligible")
			}
			if _, err := f.coordinator.ReadmitInteractionRegistration(context.Background(), f.request.Group); err == nil {
				t.Fatal("readmission accepted changed or missing authority")
			}
			if f.s.registerCalls != 1 || f.n.registered != 0 {
				t.Fatalf("refused readmission mutated registration: posts=%d native=%d", f.s.registerCalls, f.n.registered)
			}
		})
	}
}

func TestInteractionRegistrationReadmissionAcceptsLateOriginalReceipt(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.s.dropRegistrationResponse = true
	if _, err := f.coordinator.Register(context.Background(), f.request); !errors.Is(err, ErrInteractionRegistrationOutcomeUnknown) {
		t.Fatalf("initial outcome = %v", err)
	}
	f.s.currentTime = 131
	if _, err := f.coordinator.Register(context.Background(), f.request); !errors.Is(err, ErrInteractionRegistrationManualRecovery) {
		t.Fatalf("manual recovery = %v", err)
	}
	f.s.dropRegistrationResponse = false
	f.s.originalCommitsDuringReadmission = true
	result, err := f.coordinator.ReadmitInteractionRegistration(context.Background(), f.request.Group)
	if err != nil || result.Receipt.OperationID != f.s.lastOperation || f.s.registerCalls != 2 || f.n.registered != 1 {
		t.Fatalf("late original receipt was not resolved: result=%+v err=%v posts=%d native=%d", result, err, f.s.registerCalls, f.n.registered)
	}
}

func TestInteractionRegistrationManualRecoveryRemainsRetryableAfterNativeFailure(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	intent := pendingInteractionRecoveryIntent(f, []byte{7, 8, 9})
	putInteractionRecoveryIntent(t, f, intent)
	r := client.BotGroupRegistrationReceipt{OperationID: intent.Operation, GroupID: intent.Group, RegistrationSHA256: interactionRegistrationHash(intent.Body), AcceptedGeneration: intent.Generation, InitialEpoch: 1, NextSequence: 1}
	f.s.receipt = &r
	f.n.groupRegisteredErr = errors.New("native resolution unavailable")
	if _, err := f.coordinator.RecoverInteractionRegistration(context.Background(), intent.Group); err == nil {
		t.Fatal("recovery succeeded despite native resolution failure")
	}
	assertManualRecoveryPhase(t, f, intent.Group, 1)
	rows, err := f.coordinator.InspectInteractionRegistrationRecovery(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Group != intent.Group {
		t.Fatalf("failed recovery disappeared from diagnostics: rows=%+v err=%v", rows, err)
	}

	f.n.groupRegisteredErr = nil
	f.coordinator, err = NewInteractionRegistrationCoordinator(f.instance, f.community, f.install, f.n, f.d, f.s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.coordinator.RecoverInteractionRegistration(context.Background(), intent.Group)
	if err != nil || got.Receipt != r || f.n.registered != 2 || f.s.registerCalls != 0 {
		t.Fatalf("restart recovery result=%+v err=%v native_calls=%d posts=%d", got, err, f.n.registered, f.s.registerCalls)
	}
	assertCompletedInteractionRecovery(t, f, intent.Group)
}

func TestInteractionRegistrationManualRecoveryRemainsRetryableAfterPhaseTwoSaveFailure(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	intent := pendingInteractionRecoveryIntent(f, []byte{9, 8, 7})
	putInteractionRecoveryIntent(t, f, intent)
	r := client.BotGroupRegistrationReceipt{OperationID: intent.Operation, GroupID: intent.Group, RegistrationSHA256: interactionRegistrationHash(intent.Body), AcceptedGeneration: intent.Generation, InitialEpoch: 1, NextSequence: 1}
	f.s.receipt = &r
	f.n.failTransactionCall = 2 // phase 1 persists; phase 2 save fails after native resolution.
	if _, err := f.coordinator.RecoverInteractionRegistration(context.Background(), intent.Group); err == nil {
		t.Fatal("recovery succeeded despite phase 2 persistence failure")
	}
	assertManualRecoveryPhase(t, f, intent.Group, 1)
	rows, err := f.coordinator.InspectInteractionRegistrationRecovery(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Group != intent.Group {
		t.Fatalf("phase 2 save failure disappeared from diagnostics: rows=%+v err=%v", rows, err)
	}

	f.coordinator, err = NewInteractionRegistrationCoordinator(f.instance, f.community, f.install, f.n, f.d, f.s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.coordinator.RecoverInteractionRegistration(context.Background(), intent.Group)
	if err != nil || got.Receipt != r || f.n.registered != 2 || f.s.registerCalls != 0 {
		t.Fatalf("restart recovery result=%+v err=%v native_calls=%d posts=%d", got, err, f.n.registered, f.s.registerCalls)
	}
	assertCompletedInteractionRecovery(t, f, intent.Group)
}

func assertManualRecoveryPhase(t *testing.T, f *interactionRegistrationFixture, group id.ID, phase uint64) {
	t.Helper()
	v, err := f.n.StateGet(context.Background(), interactionRegistrationStateArea, interactionRegistrationKey(group))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := decodeInteractionRegistrationIntent(v.Value)
	if err != nil || saved.Phase != phase || !saved.ManualRecovery || saved.Receipt.OperationID.IsZero() {
		t.Fatalf("retryable recovery state=%+v err=%v", saved, err)
	}
}

func assertCompletedInteractionRecovery(t *testing.T, f *interactionRegistrationFixture, group id.ID) {
	t.Helper()
	v, err := f.n.StateGet(context.Background(), interactionRegistrationStateArea, interactionRegistrationKey(group))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := decodeInteractionRegistrationIntent(v.Value)
	if err != nil || saved.Phase != 2 || saved.ManualRecovery {
		t.Fatalf("completed recovery state=%+v err=%v", saved, err)
	}
}

func pendingInteractionRecoveryIntent(f *interactionRegistrationFixture, body []byte) interactionRegistrationIntent {
	return interactionRegistrationIntent{Operation: id.New(), AdmissionRequest: id.New(), Group: f.request.Group, ParticipantUser: f.request.ParticipantUser, ParticipantDevice: f.request.ParticipantDevice, BotUser: f.botUser, BotDevice: f.botDevice, MinimumRevision: 4, Policy: 3, Generation: 8, Body: body, Submitted: true, ManualRecovery: true}
}

func putInteractionRecoveryIntent(t *testing.T, f *interactionRegistrationFixture, intent interactionRegistrationIntent) {
	t.Helper()
	value, err := encodeInteractionRegistrationIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	f.n.values = map[string]native.StateValue{registrationStateMapKey(interactionRegistrationStateArea, interactionRegistrationKey(intent.Group)): {Revision: 1, Value: value}}
}

func TestInteractionRegistrationObtainsExactPackageAfterServerContext(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.request.MinimumEligibilityRevision = 0
	f.request.ParticipantKeyPackage = nil
	f.s.keyPackage = []byte{9, 8, 7}

	got, err := f.coordinator.Register(context.Background(), f.request)
	if err != nil {
		t.Fatalf("Register without caller-supplied package: %v", err)
	}
	if got.Usable || f.s.keyPackageCalls != 1 || f.n.createCalls != 1 || !bytes.Equal(f.n.createdKeyPackage, f.s.keyPackage) {
		t.Fatalf("registration did not use the exact fetched package: result=%+v keypackage_calls=%d create_calls=%d", got, f.s.keyPackageCalls, f.n.createCalls)
	}
	if f.n.createdExpected.MinimumEligibilityRevision != 4 {
		t.Fatalf("native create minimum eligibility revision = %d, want server snapshot revision 4", f.n.createdExpected.MinimumEligibilityRevision)
	}
}

func TestInteractionRegistrationFailsClosedWhenPackageReadFails(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.request.MinimumEligibilityRevision = 0
	f.request.ParticipantKeyPackage = nil
	f.s.keyPackageErr = errors.New("package unavailable")

	if _, err := f.coordinator.Register(context.Background(), f.request); err == nil {
		t.Fatal("Register succeeded without an exact participant KeyPackage")
	}
	if f.s.keyPackageCalls != 1 || f.n.createCalls != 0 || f.s.registerCalls != 0 {
		t.Fatalf("missing package did not fail closed: keypackage_calls=%d create_calls=%d register_calls=%d", f.s.keyPackageCalls, f.n.createCalls, f.s.registerCalls)
	}
}

func TestInteractionRegistrationSnapshotProviderUsesPersistedBinding(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	if _, err := f.coordinator.Register(context.Background(), f.request); err != nil {
		t.Fatalf("Register: %v", err)
	}
	event := native.GroupEvent{Group: f.request.Group, Community: f.community, SenderUser: f.request.ParticipantUser, SenderDevice: f.request.ParticipantDevice}
	expected, wire, err := f.coordinator.InteractionSnapshot(context.Background(), event)
	if err != nil {
		t.Fatalf("InteractionSnapshot: %v", err)
	}
	if expected.Group != event.Group || expected.Instance != f.instance || expected.Installation != f.install || expected.Community != f.community ||
		expected.BotUser != f.botUser || expected.BotDevice != f.botDevice || expected.ParticipantUser != event.SenderUser || expected.ParticipantDevice != event.SenderDevice ||
		expected.MinimumEligibilityRevision != 4 || expected.PolicyVersion != 3 || expected.ServerNow != 100 || len(wire) == 0 {
		t.Fatalf("snapshot binding did not match the durable registration: expected=%+v wire=%d", expected, len(wire))
	}
}

func TestInteractionRegistrationSnapshotAllowsNoMediaDespiteDiscoveryMediaSupport(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.d.value.MediaVersions = []uint64{1}
	if _, err := f.coordinator.Register(context.Background(), f.request); err != nil {
		t.Fatalf("Register: %v", err)
	}
	event := native.GroupEvent{Group: f.request.Group, Community: f.community, SenderUser: f.request.ParticipantUser, SenderDevice: f.request.ParticipantDevice}
	if _, wire, err := f.coordinator.InteractionSnapshot(context.Background(), event); err != nil || len(wire) == 0 {
		t.Fatalf("valid kind-3 mediaVersion=0 context rejected when discovery supports mediaVersion=1: wire=%d err=%v", len(wire), err)
	}
}

func TestInteractionRegistrationSnapshotProviderRequiresAdmissionAndExactTuple(t *testing.T) {
	t.Run("admission failure prevents context lookup", func(t *testing.T) {
		f := newInteractionRegistrationFixture(t)
		if _, err := f.coordinator.Register(context.Background(), f.request); err != nil {
			t.Fatalf("Register: %v", err)
		}
		f.s.admissionErr = errors.New("admission unavailable")
		event := native.GroupEvent{Group: f.request.Group, Community: f.community, SenderUser: f.request.ParticipantUser, SenderDevice: f.request.ParticipantDevice}
		if _, _, err := f.coordinator.InteractionSnapshot(context.Background(), event); err == nil {
			t.Fatal("InteractionSnapshot succeeded without a fresh admission")
		}
		if len(f.s.requestIDs) != 4 {
			t.Fatalf("admission/context calls=%d; failed admission must stop before context lookup", len(f.s.requestIDs))
		}
	})

	t.Run("wrong tuple is refused", func(t *testing.T) {
		f := newInteractionRegistrationFixture(t)
		if _, err := f.coordinator.Register(context.Background(), f.request); err != nil {
			t.Fatalf("Register: %v", err)
		}
		f.s.expected[3] = id.New() // signed snapshot names a different Bot user
		event := native.GroupEvent{Group: f.request.Group, Community: f.community, SenderUser: f.request.ParticipantUser, SenderDevice: f.request.ParticipantDevice}
		if _, _, err := f.coordinator.InteractionSnapshot(context.Background(), event); !errors.Is(err, ErrInteractionRegistrationRecovery) {
			t.Fatalf("InteractionSnapshot error=%v, want exact-tuple refusal", err)
		}
		if len(f.s.requestIDs) != 5 || f.s.requestIDs[3] != f.s.requestIDs[4] {
			t.Fatalf("snapshot did not use the fresh admission request consistently: %v", f.s.requestIDs)
		}
	})
}

func TestInteractionRegistrationSnapshotProviderRenewsExpiredAdmission(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	if _, err := f.coordinator.Register(context.Background(), f.request); err != nil {
		t.Fatalf("Register: %v", err)
	}
	originalRequest := f.s.requestIDs[0]
	f.s.currentTime = 131 // the registration admission expired at server time 130
	event := native.GroupEvent{Group: f.request.Group, Community: f.community, SenderUser: f.request.ParticipantUser, SenderDevice: f.request.ParticipantDevice}
	_, wire, err := f.coordinator.InteractionSnapshot(context.Background(), event)
	if err != nil {
		t.Fatalf("InteractionSnapshot after admission expiry: %v", err)
	}
	snapshot, err := interactionSnapshotDetailsFromWire(wire)
	if err != nil {
		t.Fatalf("parse fresh context: %v", err)
	}
	requestID := snapshot.Request
	if requestID.IsZero() || requestID == originalRequest || len(f.s.requestIDs) != 5 || f.s.requestIDs[3] != requestID || f.s.requestIDs[4] != requestID {
		t.Fatalf("provider reused expired admission request: original=%s context=%s requests=%v", originalRequest, requestID, f.s.requestIDs)
	}
	value, err := f.n.StateGet(context.Background(), interactionRegistrationStateArea, interactionRegistrationKey(f.request.Group))
	if err != nil {
		t.Fatalf("load durable registration intent: %v", err)
	}
	intent, err := decodeInteractionRegistrationIntent(value.Value)
	if err != nil || intent.AdmissionRequest != originalRequest {
		t.Fatalf("fresh snapshot mutated durable registration admission request: intent=%+v err=%v", intent, err)
	}
}

func TestInteractionRegistrationQueueProcessesDevicesOnlyForCurrentEpisode(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.s.currentEpisode = f.episode
	f.s.candidates = []id.ID{id.New(), id.New()}
	f.s.keyPackage = []byte{7, 8, 9}

	if err := f.coordinator.QueueMemberJoin(context.Background(), f.request.ParticipantUser, f.episode); err != nil {
		t.Fatalf("QueueMemberJoin: %v", err)
	}
	if f.s.registerCalls != 0 {
		t.Fatal("member join performed registration before the durable worker pass")
	}
	f.coordinator, _ = NewInteractionRegistrationCoordinator(f.instance, f.community, f.install, f.n, f.d, f.s)
	if err := f.coordinator.ProcessPending(context.Background(), 1, 8, f.s, f.s); err != nil {
		t.Fatalf("ProcessPending: %v (keypackages=%d creates=%d registrations=%d) ", err, f.s.keyPackageCalls, f.n.createCalls, f.s.registerCalls)
	}
	if f.s.registerCalls != 2 || f.n.createCalls != 2 {
		t.Fatalf("worker registered %d devices / created %d groups, want 2", f.s.registerCalls, f.n.createCalls)
	}
	if f.s.registeredGroups[0] == f.s.registeredGroups[1] {
		t.Fatal("device candidates reused one group identifier")
	}
}

func TestInteractionRegistrationQueueKeepsEpisodeUntilDevicesAppear(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.s.currentEpisode = f.episode
	if err := f.coordinator.QueueMemberJoin(context.Background(), f.request.ParticipantUser, f.episode); err != nil {
		t.Fatal(err)
	}
	if err := f.coordinator.ProcessPending(context.Background(), 1, 8, f.s, f.s); err != nil {
		t.Fatalf("empty candidate pass: %v", err)
	}
	value, err := f.n.StateGet(context.Background(), interactionRegistrationStateArea, []byte(interactionRegistrationQueueKey))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := decodeInteractionRegistrationJobs(value.Value)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("empty device list dropped pending episode: jobs=%+v err=%v", jobs, err)
	}
	f.s.candidates = []id.ID{id.New()}
	f.s.keyPackage = []byte{7, 8, 9}
	if err := f.coordinator.ProcessPending(context.Background(), 1, 8, f.s, f.s); err != nil {
		t.Fatalf("device arrival pass: %v", err)
	}
	if f.s.registerCalls != 1 {
		t.Fatalf("device arrival registered %d times, want 1", f.s.registerCalls)
	}
}

func TestInteractionRegistrationQueueStopsRetryingManualRecovery(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.s.currentEpisode = f.episode
	f.s.candidates = []id.ID{id.New()}
	f.s.keyPackage = []byte{7, 8, 9}
	f.s.dropRegistrationResponse = true
	f.s.lateCommitAfterAbsence = true
	if err := f.coordinator.QueueMemberJoin(context.Background(), f.request.ParticipantUser, f.episode); err != nil {
		t.Fatal(err)
	}
	if err := f.coordinator.ProcessPending(context.Background(), 1, 1, f.s, f.s); err == nil {
		t.Fatal("first lost response was not retained for retry")
	}
	f.s.currentTime = 131
	if err := f.coordinator.ProcessPending(context.Background(), 1, 1, f.s, f.s); !errors.Is(err, ErrInteractionRegistrationManualRecovery) {
		t.Fatalf("late commit retry error = %v, want manual recovery", err)
	}
	value, err := f.n.StateGet(context.Background(), interactionRegistrationStateArea, []byte(interactionRegistrationQueueKey))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := decodeInteractionRegistrationJobs(value.Value)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("manual recovery job remained automatic: jobs=%+v err=%v", jobs, err)
	}
	if f.s.receipt == nil || f.s.registerCalls != 1 || len(f.n.discarded) != 0 {
		t.Fatal("late server commit caused replay or local native discard")
	}
	if err := f.coordinator.ProcessPending(context.Background(), 1, 1, f.s, f.s); err != nil {
		t.Fatalf("empty terminal queue pass: %v", err)
	}
	if f.s.registerCalls != 1 {
		t.Fatalf("terminal job retried registration %d times", f.s.registerCalls)
	}
}

func TestInteractionRegistrationQueueDropsAStaleEpisode(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.s.currentEpisode = id.New()
	f.s.candidates = []id.ID{id.New()}
	if err := f.coordinator.QueueMemberJoin(context.Background(), f.request.ParticipantUser, f.episode); err != nil {
		t.Fatalf("QueueMemberJoin: %v", err)
	}
	if err := f.coordinator.ProcessPending(context.Background(), 1, 8, f.s, f.s); err != nil {
		t.Fatalf("ProcessPending: %v", err)
	}
	if f.s.registerCalls != 0 {
		t.Fatalf("stale episode attempted %d registrations", f.s.registerCalls)
	}
	f.s.currentEpisode = f.episode
	if err := f.coordinator.ProcessPending(context.Background(), 1, 8, f.s, f.s); err != nil {
		t.Fatalf("ProcessPending after stale job: %v", err)
	}
	if f.s.registerCalls != 0 {
		t.Fatal("stale episode job remained queued after it was discarded")
	}
}

func TestInteractionRegistrationQueueRetainsTransientPackageFailure(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.s.currentEpisode = f.episode
	f.s.candidates = []id.ID{id.New()}
	f.s.keyPackageErr = errors.New("package temporarily unavailable")
	if err := f.coordinator.QueueMemberJoin(context.Background(), f.request.ParticipantUser, f.episode); err != nil {
		t.Fatalf("QueueMemberJoin: %v", err)
	}
	if err := f.coordinator.ProcessPending(context.Background(), 1, 8, f.s, f.s); err == nil {
		t.Fatal("transient package failure was not reported to the worker")
	} else {
		t.Logf("first worker failure: %v (keypackages=%d)", err, f.s.keyPackageCalls)
	}
	if f.s.registerCalls != 0 {
		t.Fatal("registration ran without a package")
	}
	f.s.keyPackageErr = nil
	f.s.keyPackage = []byte{7, 8, 9}
	if err := f.coordinator.ProcessPending(context.Background(), 1, 8, f.s, f.s); err != nil {
		t.Fatalf("retry ProcessPending: %v (keypackages=%d creates=%d registrations=%d)", err, f.s.keyPackageCalls, f.n.createCalls, f.s.registerCalls)
	}
	if f.s.registerCalls != 1 {
		t.Fatalf("retry registered %d times, want 1", f.s.registerCalls)
	}
}

func TestInteractionRegistrationQueueRetriesIneligibleCurrentCandidate(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.s.currentEpisode = f.episode
	device := id.New()
	f.s.candidates = []id.ID{device}
	f.s.keyPackage = []byte{7, 8, 9}
	f.s.admissionErr = &client.APIError{Status: http.StatusForbidden}
	if err := f.coordinator.QueueMemberJoin(context.Background(), f.request.ParticipantUser, f.episode); err != nil {
		t.Fatal(err)
	}
	if err := f.coordinator.ProcessPending(context.Background(), 1, 1, f.s, f.s); err == nil {
		t.Fatal("ineligible candidate failure was not reported")
	}
	value, err := f.n.StateGet(context.Background(), interactionRegistrationStateArea, []byte(interactionRegistrationQueueKey))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := decodeInteractionRegistrationJobs(value.Value)
	if err != nil || len(jobs) != 1 || len(jobs[0].Processed) != 0 {
		t.Fatalf("ineligible current candidate was discarded: jobs=%+v err=%v", jobs, err)
	}
	f.s.admissionErr = nil // eligibility recovers while the same episode is current.
	if err := f.coordinator.ProcessPending(context.Background(), 1, 1, f.s, f.s); err != nil {
		t.Fatalf("recovered candidate pass: %v", err)
	}
	if f.s.registerCalls != 1 || f.n.createCalls != 1 {
		t.Fatalf("recovery duplicated or missed group creation: posts=%d creates=%d", f.s.registerCalls, f.n.createCalls)
	}
}

func TestInteractionRegistrationQueueRetriesOnlyFailedDevice(t *testing.T) {
	f := newInteractionRegistrationFixture(t)
	f.s.currentEpisode = f.episode
	f.s.candidates = []id.ID{id.New(), id.New()}
	f.s.keyPackage = []byte{7, 8, 9}
	f.s.keyPackageFailures = 1
	if err := f.coordinator.QueueMemberJoin(context.Background(), f.request.ParticipantUser, f.episode); err != nil {
		t.Fatalf("QueueMemberJoin: %v", err)
	}
	if err := f.coordinator.ProcessPending(context.Background(), 1, 8, f.s, f.s); err == nil {
		t.Fatal("partial device failure was not reported")
	}
	if f.s.registerCalls != 1 || f.n.createCalls != 1 {
		t.Fatalf("first pass registered/created %d/%d, want only one completed device", f.s.registerCalls, f.n.createCalls)
	}
	if err := f.coordinator.ProcessPending(context.Background(), 1, 8, f.s, f.s); err != nil {
		t.Fatalf("retry ProcessPending: %v", err)
	}
	if f.s.registerCalls != 2 || f.n.createCalls != 2 {
		t.Fatalf("retry duplicated completed device or missed failed device: registered=%d created=%d", f.s.registerCalls, f.n.createCalls)
	}
}

type interactionRegistrationFixture struct {
	coordinator                                      *InteractionRegistrationCoordinator
	n                                                *interactionRegistrationNativeFake
	s                                                *interactionRegistrationSessionFake
	d                                                *interactionRegistrationDiscoveryFake
	instance, community, install, botUser, botDevice id.ID
	episode                                          id.ID
	request                                          InteractionRegistrationRequest
}

func newInteractionRegistrationFixture(t *testing.T) *interactionRegistrationFixture {
	t.Helper()
	f := &interactionRegistrationFixture{instance: id.New(), community: id.New(), install: id.New(), botUser: id.New(), botDevice: id.New(), episode: id.New()}
	f.n = &interactionRegistrationNativeFake{binding: native.Binding{Instance: f.instance, Community: f.community, Installation: f.install}, identity: native.Identity{Phase: 2, Instance: &f.instance, User: &f.botUser, Device: &f.botDevice}}
	f.d = &interactionRegistrationDiscoveryFake{value: client.Discovery{InstanceID: f.instance, Generation: 8, PolicyVersion: 3, ExternalSenderPub: append([]byte{1}, make([]byte, 31)...), E2EEVersions: []uint64{1}, MediaVersions: []uint64{0}}}
	f.s = &interactionRegistrationSessionFake{admission: []byte{0x41}}
	f.request = InteractionRegistrationRequest{Group: id.New(), ParticipantUser: id.New(), ParticipantDevice: id.New(), MinimumEligibilityRevision: 4, ParticipantKeyPackage: []byte{1, 2, 3}}
	f.s.expected = []any{f.instance, f.install, f.community, f.botUser, f.botDevice}
	var err error
	f.coordinator, err = NewInteractionRegistrationCoordinator(f.instance, f.community, f.install, f.n, f.d, f.s)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func makeSignedSnapshotFixtureFor(request id.ID, expected []any, now uint64) ([]byte, error) {
	payload, err := cborx.Marshal([]any{uint64(1), request, id.New(), []byte{1}, make([]byte, 64), now, uint64(4), uint64(3), uint64(1), uint64(0), id.New(), id.New(), make([]byte, 32), expected, []any{}, []any{}})
	if err != nil {
		return nil, err
	}
	return cborx.Marshal([]any{payload, make([]byte, 64)})
}

type interactionRegistrationNativeFake struct {
	binding                               native.Binding
	identity                              native.Identity
	values                                map[string]native.StateValue
	createCalls, inspectCalls, registered uint64
	discarded                             []id.ID
	createErr                             error
	groupRegisteredErr                    error
	failTransactionCall                   uint64
	transactionCalls                      uint64
	createdExpected                       native.InteractionGroupExpected
	createdKeyPackage                     []byte
	createOperations                      []id.ID
	inspectState                          uint64
}

func (n *interactionRegistrationNativeFake) Binding() native.Binding { return n.binding }
func (n *interactionRegistrationNativeFake) Bootstrap(context.Context, [32]byte) (native.BootstrapResult, error) {
	return native.BootstrapResult{}, nil
}
func (n *interactionRegistrationNativeFake) Identity(context.Context) (native.Identity, error) {
	return n.identity, nil
}
func (n *interactionRegistrationNativeFake) CreateInteractionGroup(_ context.Context, operation id.ID, e native.InteractionGroupExpected, _ []byte, keyPackage []byte) (native.InteractionGroupCreateResult, error) {
	n.createCalls++
	n.createOperations = append(n.createOperations, operation)
	n.createdExpected = e
	n.createdKeyPackage = append([]byte(nil), keyPackage...)
	if n.createErr != nil {
		return native.InteractionGroupCreateResult{}, n.createErr
	}
	return native.InteractionGroupCreateResult{Group: e.Group, Binding: native.InteractionGroupBinding{Instance: e.Instance, BotUser: e.BotUser, Installation: e.Installation, Community: e.Community, ParticipantUser: e.ParticipantUser, ParticipantDevice: e.ParticipantDevice, PolicyVersion: e.PolicyVersion, E2EEVersion: 1, MediaVersion: 0}, BindingBody: []byte{1}, GroupInfo: []byte{2}, RatchetTree: []byte{3}, Welcome: []byte{4}, Epoch: 1, MemberCount: 2}, nil
}
func (n *interactionRegistrationNativeFake) InspectInteractionGroup(_ context.Context, e native.InteractionGroupExpected, _ []byte) (native.InteractionGroupInspectResult, error) {
	n.inspectCalls++
	if n.createCalls == 0 || e.Group != n.createdExpected.Group {
		return native.InteractionGroupInspectResult{}, errors.New("staged interaction group missing")
	}
	return native.InteractionGroupInspectResult{Binding: native.InteractionGroupBinding{Instance: e.Instance, BotUser: e.BotUser,
		Installation: e.Installation, Community: e.Community, ParticipantUser: e.ParticipantUser,
		ParticipantDevice: e.ParticipantDevice, PolicyVersion: e.PolicyVersion, E2EEVersion: 1},
		BotDevice: e.BotDevice, Epoch: 1, State: n.inspectState, NextSequence: 1}, nil
}
func (n *interactionRegistrationNativeFake) DiscardGroup(_ context.Context, operation, _ id.ID) error {
	n.discarded = append(n.discarded, operation)
	return nil
}

func (n *interactionRegistrationNativeFake) GroupRegistered(context.Context, id.ID, id.ID, uint64) error {
	n.registered++
	return n.groupRegisteredErr
}
func (n *interactionRegistrationNativeFake) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	if n.values == nil {
		n.values = map[string]native.StateValue{}
	}
	return n.values[registrationStateMapKey(area, key)], nil
}
func (n *interactionRegistrationNativeFake) StateScan(_ context.Context, area uint64, after []byte, limit uint64) (native.StatePage, error) {
	var keys [][]byte
	for encoded := range n.values {
		if len(encoded) == 0 || encoded[0] != byte(area) {
			continue
		}
		keys = append(keys, []byte(strings.TrimPrefix(encoded, string([]byte{byte(area)}))))
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
	page := native.StatePage{}
	for _, key := range keys {
		if after != nil && bytes.Compare(key, after) <= 0 {
			continue
		}
		if uint64(len(page.Entries)) >= limit {
			page.Next = append([]byte(nil), page.Entries[len(page.Entries)-1].Key...)
			break
		}
		page.Entries = append(page.Entries, native.StateEntry{Key: append([]byte(nil), key...), Revision: n.values[registrationStateMapKey(area, key)].Revision, Value: append([]byte(nil), n.values[registrationStateMapKey(area, key)].Value...), Tombstone: n.values[registrationStateMapKey(area, key)].Tombstone})
	}
	return page, nil
}
func (n *interactionRegistrationNativeFake) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	n.transactionCalls++
	if n.failTransactionCall != 0 && n.transactionCalls == n.failTransactionCall {
		return nil, errors.New("injected state transaction failure")
	}
	if n.values == nil {
		n.values = map[string]native.StateValue{}
	}
	out := make([]uint64, len(changes))
	for i, c := range changes {
		k := registrationStateMapKey(c.Area, c.Key)
		v := n.values[k]
		if v.Revision != c.ExpectedRevision {
			return nil, errors.New("state conflict")
		}
		v.Revision++
		v.Value = append([]byte(nil), c.Value...)
		n.values[k] = v
		out[i] = v.Revision
	}
	return out, nil
}

type interactionRegistrationDiscoveryFake struct{ value client.Discovery }

func (d *interactionRegistrationDiscoveryFake) Discover(context.Context) (client.Discovery, error) {
	return d.value, nil
}

type interactionRegistrationSessionFake struct {
	admission                        []byte
	registerCalls, receiptCalls      uint64
	storeThenLoseResponse            bool
	dropRegistrationResponse         bool
	lateCommitAfterAbsence           bool
	originalCommitsDuringReadmission bool
	firstSubmission                  *client.BotGroupRegistrationReceipt
	lastOperation                    id.ID
	lateReceipt                      *client.BotGroupRegistrationReceipt
	hideReceiptCalls                 uint64
	receipt                          *client.BotGroupRegistrationReceipt
	ignoreReceiptOperation           bool
	requestIDs                       []id.ID
	expected                         []any
	currentEpisode                   id.ID
	candidates                       []id.ID
	registeredGroups                 []id.ID
	admissionErr                     error
	registerErr                      error
	currentTime                      uint64
	minimumRevision                  uint64
	admissions                       map[id.ID]uint64
	keyPackage                       []byte
	keyPackageErr                    error
	keyPackageCalls                  uint64
	keyPackageFailures               uint64
}

func (s *interactionRegistrationSessionFake) KeyPackage(context.Context, id.ID) ([]byte, error) {
	s.keyPackageCalls++
	if s.keyPackageFailures > 0 {
		s.keyPackageFailures--
		return nil, errors.New("package temporarily unavailable")
	}
	return append([]byte(nil), s.keyPackage...), s.keyPackageErr
}

func (s *interactionRegistrationSessionFake) IssueBotInteractionAdmission(_ context.Context, _ id.ID, request id.ID, _ id.ID, _ id.ID, _ id.ID) ([]byte, error) {
	s.requestIDs = append(s.requestIDs, request)
	if s.admissionErr != nil {
		return nil, s.admissionErr
	}
	if s.currentTime == 0 {
		s.currentTime = 100
	}
	if s.admissions == nil {
		s.admissions = map[id.ID]uint64{}
	}
	s.admissions[request] = s.currentTime + 30
	return append([]byte(nil), s.admission...), nil
}

func (s *interactionRegistrationSessionFake) ResolveCurrentEpisode(context.Context, id.ID) (id.ID, bool, error) {
	return s.currentEpisode, !s.currentEpisode.IsZero(), nil
}

func (s *interactionRegistrationSessionFake) DeviceCandidates(context.Context, id.ID) ([]id.ID, error) {
	return append([]id.ID(nil), s.candidates...), nil
}
func (s *interactionRegistrationSessionFake) BotInteractionContextSnapshot(_ context.Context, _ id.ID, request, group, user, device id.ID) ([]byte, error) {
	s.requestIDs = append(s.requestIDs, request)
	if s.currentTime == 0 {
		s.currentTime = 100
	}
	if s.admissions[request] <= s.currentTime {
		return nil, errors.New("admission expired or missing")
	}
	expected := append(append([]any(nil), s.expected...), user, device, group)
	wire, err := makeSignedSnapshotFixtureFor(request, expected, s.currentTime)
	if err != nil || s.minimumRevision == 0 {
		return wire, err
	}
	var envelope []any
	if err := cborx.Unmarshal(wire, &envelope); err != nil {
		return nil, err
	}
	payload, ok := envelope[0].([]byte)
	if !ok {
		return nil, errors.New("bad signed snapshot fixture")
	}
	var fields []any
	if err := cborx.Unmarshal(payload, &fields); err != nil || len(fields) < 7 {
		return nil, errors.New("bad snapshot payload fixture")
	}
	fields[6] = s.minimumRevision
	payload, err = cborx.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return cborx.Marshal([]any{payload, make([]byte, 64)})
}
func (s *interactionRegistrationSessionFake) RegisterBotGroupWithAdmissionRequest(_ context.Context, _ id.ID, op id.ID, generation uint64, body []byte, admissionRequest id.ID) (client.BotGroupRegistrationReceipt, error) {
	s.requestIDs = append(s.requestIDs, admissionRequest)
	s.registerCalls++
	if s.registerErr != nil {
		return client.BotGroupRegistrationReceipt{}, s.registerErr
	}
	var a []any
	if err := cborx.Unmarshal(body, &a); err != nil || len(a) != 5 {
		return client.BotGroupRegistrationReceipt{}, errors.New("bad interaction body")
	}
	group, ok := registrationID(a[0])
	if !ok {
		return client.BotGroupRegistrationReceipt{}, errors.New("bad group")
	}
	s.registeredGroups = append(s.registeredGroups, group)
	s.lastOperation = op
	r := client.BotGroupRegistrationReceipt{OperationID: op, GroupID: group, RegistrationSHA256: interactionRegistrationHash(body), AcceptedGeneration: generation, InitialEpoch: 1, NextSequence: 1}
	if s.registerCalls == 1 {
		s.firstSubmission = &r
	}
	if s.registerCalls == 2 && s.originalCommitsDuringReadmission {
		s.receipt = s.firstSubmission
		return client.BotGroupRegistrationReceipt{}, errors.New("original submission committed before readmission")
	}
	if s.dropRegistrationResponse {
		if s.lateCommitAfterAbsence {
			s.lateReceipt = &r
		}
		return client.BotGroupRegistrationReceipt{}, errors.New("response lost before commit")
	}
	s.receipt = &r
	if s.storeThenLoseResponse {
		s.hideReceiptCalls = 1
		return client.BotGroupRegistrationReceipt{}, errors.New("response lost")
	}
	return r, nil
}
func (s *interactionRegistrationSessionFake) BotGroupRegistrationReceipt(_ context.Context, _ id.ID, operation id.ID) (client.BotGroupRegistrationReceipt, error) {
	s.receiptCalls++
	if s.hideReceiptCalls > 0 {
		s.hideReceiptCalls--
		return client.BotGroupRegistrationReceipt{}, client.ErrRegistrationReceiptNotFound
	}
	if s.receipt == nil || (s.receipt.OperationID != operation && !s.ignoreReceiptOperation) {
		if s.lateReceipt != nil && s.receiptCalls >= 3 {
			// The in-flight POST commits immediately after the next retry's
			// endpoint decided
			// to return E_NOT_FOUND.
			s.receipt = s.lateReceipt
			s.lateReceipt = nil
		}
		return client.BotGroupRegistrationReceipt{}, client.ErrRegistrationReceiptNotFound
	}
	return *s.receipt, nil
}
func (s *interactionRegistrationSessionFake) BotInteractionGroupAbsent(_ context.Context, _ id.ID, request id.ID) (bool, error) {
	if s.currentTime == 0 {
		s.currentTime = 100
	}
	notAfter, ok := s.admissions[request]
	return ok && notAfter > s.currentTime, nil
}
func (s *interactionRegistrationSessionFake) GroupInfo(context.Context, id.ID) (client.GroupInfo, error) {
	return client.GroupInfo{}, &client.APIError{Status: http.StatusNotFound, Code: "E_NOT_FOUND"}
}

var _ = native.StateValue{}

func TestInteractionRegistrationRateLimitsRemainRetryable(t *testing.T) {
	for _, stage := range []string{"admission", "keypackage", "registration"} {
		t.Run(stage, func(t *testing.T) {
			f := newInteractionRegistrationFixture(t)
			refusal := &client.APIError{Status: 429, Code: "E_RATE_LIMITED", Outcome: client.OutcomeDefinitive}
			switch stage {
			case "admission":
				f.s.admissionErr = refusal
			case "keypackage":
				f.request.ParticipantKeyPackage = nil
				f.s.keyPackage = []byte{7, 8, 9}
				f.s.keyPackageErr = refusal
			case "registration":
				f.s.registerErr = refusal
			}
			if _, err := f.coordinator.Register(t.Context(), f.request); client.RateLimitError(err) == nil {
				t.Fatalf("lost rate-limit classification: %v", err)
			}
			f.s.admissionErr = nil
			f.s.keyPackageErr = nil
			f.s.registerErr = nil
			if got, err := f.coordinator.Register(t.Context(), f.request); err != nil || got.Receipt.GroupID != f.request.Group || len(got.PendingWelcome) == 0 {
				t.Fatalf("next tick: result=%+v error=%v", got, err)
			}
		})
	}
}
