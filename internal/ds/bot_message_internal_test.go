package ds

import (
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestBotSendFingerprintIsStableAndBindsAttemptScope(t *testing.T) {
	installation, user, device := id.New(), id.New(), id.New()
	operation, attempt, community, group := id.New(), id.New(), id.New(), id.New()
	hash := make([]byte, 32)
	base, err := botSendFingerprint(installation, user, device, operation, attempt, nil, community, group, 4, 7, 0, hash)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := botSendFingerprint(installation, user, device, operation, attempt, nil, community, group, 4, 7, 0, hash)
	if err != nil || repeated != base {
		t.Fatalf("repeat fingerprint = %x, %v; want %x", repeated, err, base)
	}
	changed, err := botSendFingerprint(installation, user, device, operation, id.New(), nil, community, group, 4, 7, 0, hash)
	if err != nil {
		t.Fatal(err)
	}
	if changed == base {
		t.Fatal("a different attempt ID produced the same immutable fingerprint")
	}
	if _, err := botSendFingerprint(installation, user, device, operation, attempt, nil, community, group, 1<<63, 7, 0, hash); err == nil {
		t.Fatal("generation above signed int64 was fingerprinted")
	}
}

func TestManagedBotSendRejectsInvalidScopeAndOrphanAcceptancePreconditions(t *testing.T) {
	installation, user, device, operation, attempt, community, group := id.New(), id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	request := BotMessageRequest{
		Session:        auth.Session{UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled, Bot: &auth.BotSessionBinding{InstallationID: installation, State: store.BotInstallationReady}},
		InstallationID: installation, OperationID: operation, AttemptID: attempt, CommunityID: community,
		GroupID: group, ExpectedGeneration: 1, Epoch: 1, Ciphertext: []byte{1},
	}
	if err := validateBotMessageRequest(request); err != nil {
		t.Fatalf("valid send intent rejected: %v", err)
	}
	bad := request
	bad.Session.Bot = &auth.BotSessionBinding{InstallationID: id.New(), State: store.BotInstallationReady}
	if err := validateBotMessageRequest(bad); err == nil {
		t.Fatal("a session bound to another installation was accepted")
	}
	bad = request
	bad.Class = 2
	if err := validateBotMessageRequest(bad); err == nil {
		t.Fatal("an unsupported upload class was accepted")
	}
	bad = request
	bad.PredecessorAttempt = &id.ID{}
	if err := validateBotMessageRequest(bad); err == nil {
		t.Fatal("a zero predecessor identifier was accepted")
	}
	bad = request
	bad.ExpectedGeneration = 1 << 63
	if err := validateBotMessageRequest(bad); err == nil {
		t.Fatal("generation above signed int64 was accepted")
	}
}

func TestManagedBotResolveRequiresExactCiphertextHash(t *testing.T) {
	request := BotMessageResolveRequest{
		Session:     auth.Session{UserID: id.New(), DeviceID: id.New(), Scope: auth.ScopeEnrolled, Bot: &auth.BotSessionBinding{InstallationID: id.New(), State: store.BotInstallationReady}},
		OperationID: id.New(), AttemptID: id.New(), CommunityID: id.New(), GroupID: id.New(), Epoch: 1,
		CiphertextSHA256: make([]byte, 31),
	}
	request.InstallationID = request.Session.Bot.InstallationID
	if err := validateBotResolveRequest(request); err == nil {
		t.Fatal("a resolve request without a 32-byte ciphertext hash was accepted")
	}
}
