package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// sessionLive reports whether the bearer token still names a session.
func sessionLive(t *testing.T, h *cliHarness, tok string) bool {
	t.Helper()
	_, err := h.Repo(t).GetSessionByHash(t.Context(), auth.TokenHash(tok), h.Now())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session: %v", err)
	}
	return err == nil
}

// typed sets the confirmation the verb reads and whether stdin counts as a terminal for one test.
// The previous values come back when the test ends.
func typed(t *testing.T, input string, terminal bool) {
	t.Helper()
	prevIn, prevTerm := adminIn, adminInTerminal
	adminIn = strings.NewReader(input)
	adminInTerminal = func() bool { return terminal }
	t.Cleanup(func() { adminIn, adminInTerminal = prevIn, prevTerm })
}

// activeDevices counts the devices of user that are not revoked.
func activeDevices(t *testing.T, h *cliHarness, user id.ID) int {
	t.Helper()
	rows, err := h.Repo(t).ListDevicesByUser(t.Context(), user)
	if err != nil {
		t.Fatalf("ListDevicesByUser: %v", err)
	}
	n := 0
	for _, d := range rows {
		if d.RevokedAt == nil {
			n++
		}
	}
	return n
}

// `device revoke --all` signs every device of the person out (R1): each device is revoked, each
// session is gone, and another person is untouched.
func TestDeviceRevokeAllRevokesEveryDeviceOfTheUser(t *testing.T) {
	h := newCLIHarness(t)
	alice := h.SeedUser(t, "alice")
	bob := h.SeedUser(t, "bob")
	aliceTok := []string{h.SeedSession(t, alice), h.SeedSession(t, alice)}
	bobTok := h.SeedSession(t, bob)
	var aliceDevices []id.ID
	for _, tok := range aliceTok {
		s, err := h.Repo(t).GetSessionByHash(t.Context(), auth.TokenHash(tok), h.Now())
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		aliceDevices = append(aliceDevices, s.DeviceID)
	}

	code, out := h.Run(t, "admin", "device", "revoke", "--all", "--username=alice", "--yes", "--config="+h.ConfigPath)
	if code != 0 {
		t.Fatalf("revoke --all: %d %s", code, out)
	}
	if !strings.Contains(out, "revoked 2 devices") {
		t.Errorf("the output must say how many devices were revoked: %s", out)
	}
	for _, d := range aliceDevices {
		row, err := h.Repo(t).GetDevice(t.Context(), d)
		if err != nil || row.RevokedAt == nil {
			t.Errorf("device %s was not revoked: %+v %v", d, row, err)
		}
	}
	for _, tok := range aliceTok {
		if sessionLive(t, h, tok) {
			t.Error("one of alice's sessions survived")
		}
	}
	if !sessionLive(t, h, bobTok) {
		t.Error("bob's session was deleted")
	}
}

// The history-loss warning is one sentence, and it is printed on success.
func TestDeviceRevokeAllWarnsInOneSentenceAboutLostHistory(t *testing.T) {
	h := newCLIHarness(t)
	alice := h.SeedUser(t, "alice")
	h.SeedSession(t, alice)
	code, out := h.Run(t, "admin", "device", "revoke", "--all", "--username=alice", "--yes", "--config="+h.ConfigPath)
	if code != 0 {
		t.Fatalf("revoke --all: %d %s", code, out)
	}
	want := "Encrypted history is lost unless another device still holds it, and after this none of the person's devices is active."
	if !strings.Contains(out, want) {
		t.Errorf("the warning is missing or changed; want %q in: %s", want, out)
	}
}

// --id names the user in --all mode, the same selector the other user verbs take.
func TestDeviceRevokeAllAcceptsTheUserIDFlag(t *testing.T) {
	h := newCLIHarness(t)
	alice := h.SeedUser(t, "alice")
	tok := h.SeedSession(t, alice)
	if code, out := h.Run(t, "admin", "device", "revoke", "--all", "--id="+alice.String(), "--yes", "--config="+h.ConfigPath); code != 0 {
		t.Fatalf("revoke --all --id: %d %s", code, out)
	}
	if sessionLive(t, h, tok) {
		t.Error("the session survived a revoke by --id")
	}
}

// Before anything is revoked the verb prints the resolved user, its id and its device count.
func TestDeviceRevokeAllPrintsTheResolvedUserFirst(t *testing.T) {
	h := newCLIHarness(t)
	alice := h.SeedUser(t, "alice")
	h.SeedSession(t, alice)
	h.SeedSession(t, alice)
	typed(t, "", false)
	code, out := h.Run(t, "admin", "device", "revoke", "--all", "--username=alice", "--config="+h.ConfigPath)
	if exit.Code(code) != exit.Usage {
		t.Fatalf("without --yes on a non-terminal: exit %d, want 2: %s", code, out)
	}
	for _, want := range []string{"alice", alice.String(), "2 active devices"} {
		if !strings.Contains(out, want) {
			t.Errorf("the resolved user line lacks %q: %s", want, out)
		}
	}
	if activeDevices(t, h, alice) != 2 {
		t.Error("a refused revoke changed the devices")
	}
}

// On a terminal the verb asks for the username typed back; the right answer revokes.
func TestDeviceRevokeAllTypedConfirmationRevokes(t *testing.T) {
	h := newCLIHarness(t)
	alice := h.SeedUser(t, "alice")
	h.SeedSession(t, alice)
	typed(t, "alice\n", true)
	code, out := h.Run(t, "admin", "device", "revoke", "--all", "--username=alice", "--config="+h.ConfigPath)
	if code != 0 {
		t.Fatalf("typed confirmation: exit %d: %s", code, out)
	}
	if activeDevices(t, h, alice) != 0 {
		t.Error("the typed confirmation did not revoke the devices")
	}
}

// A wrong answer, or no answer, revokes nothing.
func TestDeviceRevokeAllWrongConfirmationRevokesNothing(t *testing.T) {
	for _, input := range []string{"bob\n", "\n", "ALICE\n"} {
		t.Run(strings.TrimSpace(input), func(t *testing.T) {
			h := newCLIHarness(t)
			alice := h.SeedUser(t, "alice")
			h.SeedSession(t, alice)
			typed(t, input, true)
			code, out := h.Run(t, "admin", "device", "revoke", "--all", "--username=alice", "--config="+h.ConfigPath)
			if exit.Code(code) != exit.Fail {
				t.Fatalf("exit %d, want 1: %s", code, out)
			}
			if activeDevices(t, h, alice) != 1 {
				t.Error("a wrong confirmation revoked the devices")
			}
		})
	}
}

// Without --yes and without a terminal the verb refuses, and says why.
func TestDeviceRevokeAllRefusesWithoutYesOffATerminal(t *testing.T) {
	h := newCLIHarness(t)
	alice := h.SeedUser(t, "alice")
	h.SeedSession(t, alice)
	typed(t, "alice\n", false)
	code, out := h.Run(t, "admin", "device", "revoke", "--all", "--username=alice", "--config="+h.ConfigPath)
	if exit.Code(code) != exit.Usage || !strings.Contains(out, "--yes") {
		t.Fatalf("exit %d, want 2 naming --yes: %s", code, out)
	}
	if activeDevices(t, h, alice) != 1 {
		t.Error("the refused revoke changed the devices")
	}
}

// A user whose devices are already revoked has nothing to revoke: the verb says so and succeeds.
func TestDeviceRevokeAllForAUserWithNoActiveDevices(t *testing.T) {
	h := newCLIHarness(t)
	alice := h.SeedUser(t, "alice")
	h.SeedSession(t, alice)
	if code, out := h.Run(t, "admin", "device", "revoke", "--all", "--username=alice", "--yes", "--config="+h.ConfigPath); code != 0 {
		t.Fatalf("first revoke: %d %s", code, out)
	}
	code, out := h.Run(t, "admin", "device", "revoke", "--all", "--username=alice", "--yes", "--config="+h.ConfigPath)
	if code != 0 || !strings.Contains(out, "no active devices") {
		t.Fatalf("second revoke: %d %s, want 0 and 'no active devices'", code, out)
	}
}

// A deleted account is refused with the data exit code.
func TestDeviceRevokeAllForADeletedUser(t *testing.T) {
	h := newCLIHarness(t)
	alice := h.SeedUser(t, "alice")
	h.SeedSession(t, alice)
	if code, out := h.Run(t, "admin", "user", "delete", "--id="+alice.String(), "--yes", "--config="+h.ConfigPath); code != 0 {
		t.Fatalf("delete: %d %s", code, out)
	}
	code, out := h.Run(t, "admin", "device", "revoke", "--all", "--username=alice", "--yes", "--config="+h.ConfigPath)
	if exit.Code(code) != exit.Data {
		t.Fatalf("revoke of a deleted user: exit %d, want 65: %s", code, out)
	}
}

// The positional form is gone: a stray argument is a usage error, and nothing is revoked.
func TestDeviceRevokeAllRejectsAPositionalUser(t *testing.T) {
	h := newCLIHarness(t)
	alice := h.SeedUser(t, "alice")
	h.SeedSession(t, alice)
	code, out := h.Run(t, "admin", "device", "revoke", "--all", "alice", "--yes", "--config="+h.ConfigPath)
	if exit.Code(code) != exit.Usage {
		t.Fatalf("a positional user: exit %d, want 2: %s", code, out)
	}
	if activeDevices(t, h, alice) != 1 {
		t.Error("a refused positional revoke changed the devices")
	}
}

// Exactly one selector: both, or neither, is a usage error.
func TestDeviceRevokeAllNeedsExactlyOneSelector(t *testing.T) {
	h := newCLIHarness(t)
	alice := h.SeedUser(t, "alice")
	cases := map[string][]string{
		"both":    {"--id=" + alice.String(), "--username=alice"},
		"neither": {},
	}
	for name, sel := range cases {
		args := append([]string{"admin", "device", "revoke", "--all", "--yes"}, sel...)
		code, out := h.Run(t, append(args, "--config="+h.ConfigPath)...)
		if exit.Code(code) != exit.Usage {
			t.Errorf("%s: exit %d, want 2: %s", name, code, out)
		}
	}
}

// --username belongs to --all: the single-device form keeps --id and refuses the rest.
func TestDeviceRevokeSingleDeviceRefusesUsername(t *testing.T) {
	h := newCLIHarness(t)
	alice := h.SeedUser(t, "alice")
	h.SeedSession(t, alice)
	code, out := h.Run(t, "admin", "device", "revoke", "--username=alice", "--config="+h.ConfigPath)
	if exit.Code(code) != exit.Usage {
		t.Fatalf("--username without --all: exit %d, want 2: %s", code, out)
	}
}

// An unknown handle names no user: exit 65, nothing changed.
func TestDeviceRevokeAllUnknownUser(t *testing.T) {
	h := newCLIHarness(t)
	code, out := h.Run(t, "admin", "device", "revoke", "--all", "--username=carol", "--yes", "--config="+h.ConfigPath)
	if exit.Code(code) != exit.Data {
		t.Fatalf("unknown user: exit %d, want 65: %s", code, out)
	}
}
