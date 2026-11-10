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
	if !errors.As(err, &refusal) || refusal.Code != "E_FORBIDDEN" || !bytes.Contains([]byte(refusal.Detail), []byte("kind-3 groups are not registered")) {
		t.Fatalf("kind-3 registration = %v, want explicit fail-closed refusal", err)
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
