package ds_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestManagedRegistrationRejectsNoncanonicalBodyBeforeStoreAccess(t *testing.T) {
	h := newDSHarness(t)
	installation := id.New()
	request := ds.BotRegisterRequest{
		Session:        auth.Session{Scope: auth.ScopeEnrolled, Bot: &auth.BotSessionBinding{InstallationID: installation, State: store.BotInstallationJoining}},
		InstallationID: installation, OperationID: id.New(), ExpectedGeneration: 1,
		RegistrationBody: []byte{0x80}, Registration: ds.RegisterRequest{GroupID: id.New()},
	}
	_, _, err := h.ds.RegisterWithBotReceipt(context.Background(), request)
	var refusal *ds.Error
	if !errors.As(err, &refusal) || refusal.Code != "E_INVALID_REQUEST" {
		t.Fatalf("noncanonical registration body = %v, want E_INVALID_REQUEST before lookup", err)
	}
}

func TestManagedRegistrationRejectsAdmissionIDOutsideKindThree(t *testing.T) {
	h := newDSHarness(t)
	installation := id.New()
	registration := ds.RegisterRequest{GroupID: id.New(), Binding: []byte("not a binding"), GroupInfo: []byte{1}, RatchetTree: []byte{2}}
	body, err := cborx.Marshal([]any{registration.GroupID, registration.Binding, registration.GroupInfo, registration.RatchetTree})
	if err != nil {
		t.Fatal(err)
	}
	request := ds.BotRegisterRequest{
		Session:        auth.Session{Scope: auth.ScopeEnrolled, Bot: &auth.BotSessionBinding{InstallationID: installation, State: store.BotInstallationJoining}},
		InstallationID: installation, OperationID: id.New(), ExpectedGeneration: 1, AdmissionRequestID: id.New(),
		RegistrationBody: body, Registration: registration,
	}
	_, _, err = h.ds.RegisterWithBotReceipt(context.Background(), request)
	var refusal *ds.Error
	if !errors.As(err, &refusal) || refusal.Code != "E_INVALID_REQUEST" || !bytes.Contains([]byte(refusal.Detail), []byte("admission_request_id is only valid")) {
		t.Fatalf("admission ID on a non-kind-3 registration = %v, want explicit E_INVALID_REQUEST", err)
	}
}

func TestManagedRegistrationKeepsV2KindThreeFailClosedBeforeStoreAccess(t *testing.T) {
	h := newDSHarness(t)
	installation, community := id.New(), id.New()
	registration := ds.RegisterRequest{GroupID: id.New(), GroupInfo: []byte{1}, RatchetTree: []byte{2}}
	var err error
	registration.Binding, err = cborx.Marshal([]any{uint64(2), id.New(), nil, id.New(), uint64(3), uint64(1), uint64(1), uint64(0), installation, community, id.New(), id.New()})
	if err != nil {
		t.Fatal(err)
	}
	body, err := cborx.Marshal([]any{registration.GroupID, registration.Binding, registration.GroupInfo, registration.RatchetTree})
	if err != nil {
		t.Fatal(err)
	}
	for _, admissionRequestID := range []id.ID{{}, id.New()} {
		request := ds.BotRegisterRequest{
			Session: auth.Session{Scope: auth.ScopeEnrolled, Bot: &auth.BotSessionBinding{
				InstallationID: installation, CommunityID: community, State: store.BotInstallationJoining}},
			InstallationID: installation, OperationID: id.New(), ExpectedGeneration: 1,
			AdmissionRequestID: admissionRequestID, RegistrationBody: body, Registration: registration,
		}
		_, _, err := h.ds.RegisterWithBotReceipt(context.Background(), request)
		var refusal *ds.Error
		if !errors.As(err, &refusal) || refusal.Code != "E_FORBIDDEN" || !bytes.Contains([]byte(refusal.Detail), []byte("unavailable")) {
			t.Fatalf("kind-3 admission %s = %v, want explicit fail-closed refusal", admissionRequestID, err)
		}
	}
}

func TestManagedRegistrationKeepsLegacyKindThreeFailClosedBeforeStoreAccess(t *testing.T) {
	h := newDSHarness(t)
	installation := id.New()
	binding, err := cborx.Marshal(ds.Binding{V: 1, InstanceID: id.New(), TargetID: id.New(), Kind: 3, PolicyVersion: 1, E2EEVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	registration := ds.RegisterRequest{GroupID: id.New(), Binding: binding, GroupInfo: []byte{1}, RatchetTree: []byte{2}}
	body, err := cborx.Marshal([]any{registration.GroupID, registration.Binding, registration.GroupInfo, registration.RatchetTree})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = h.ds.RegisterWithBotReceipt(context.Background(), ds.BotRegisterRequest{
		Session:        auth.Session{Scope: auth.ScopeEnrolled, Bot: &auth.BotSessionBinding{InstallationID: installation, State: store.BotInstallationJoining}},
		InstallationID: installation, OperationID: id.New(), ExpectedGeneration: 1, RegistrationBody: body, Registration: registration,
	})
	var refusal *ds.Error
	if !errors.As(err, &refusal) || refusal.Code != "E_FORBIDDEN" || !bytes.Contains([]byte(refusal.Detail), []byte("unavailable")) {
		t.Fatalf("legacy kind-3 registration = %v, want explicit fail-closed refusal", err)
	}
}

func TestManagedRegistrationRejectsZeroOperationID(t *testing.T) {
	h := newDSHarness(t)
	installation := id.New()
	request := ds.BotRegisterRequest{
		Session:        auth.Session{Scope: auth.ScopeEnrolled, Bot: &auth.BotSessionBinding{InstallationID: installation, State: store.BotInstallationJoining}},
		InstallationID: installation, ExpectedGeneration: 1, RegistrationBody: []byte{0x80},
	}
	_, _, err := h.ds.RegisterWithBotReceipt(context.Background(), request)
	var refusal *ds.Error
	if !errors.As(err, &refusal) || refusal.Code != "E_INVALID_REQUEST" {
		t.Fatalf("zero operation id = %v, want E_INVALID_REQUEST", err)
	}
}
