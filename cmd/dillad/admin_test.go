package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestEveryNounVerbPairDispatches(t *testing.T) {
	h := newCLIHarness(t)
	uid := h.SeedUser(t, "alice")
	cid := h.SeedCommunity(t, uid)
	did := h.SeedDevice(t, uid)

	cases := []struct {
		args []string
		want exit.Code
		in   string // a substring that must appear in stdout
	}{
		{[]string{"admin", "user", "list"}, exit.OK, "alice"},
		{[]string{"admin", "user", "show", "--id=" + uid.String()}, exit.OK, uid.String()},
		{[]string{"admin", "user", "disable", "--id=" + uid.String()}, exit.OK, "disabled"},
		{[]string{"admin", "user", "enable", "--id=" + uid.String()}, exit.OK, "enabled"},
		{[]string{"admin", "invite", "create", "--max-uses=3", "--ttl=24h"}, exit.OK, "https://"},
		{[]string{"admin", "invite", "list"}, exit.OK, "max_uses"},
		{[]string{"admin", "community", "list"}, exit.OK, cid.String()},
		{[]string{"admin", "community", "show", "--id=" + cid.String()}, exit.OK, "policy_version"},
		{[]string{"admin", "device", "revoke", "--id=" + did.String()}, exit.OK, "revoked"},
		{[]string{"admin", "audit"}, exit.OK, "action"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, stdout := h.Run(t, append(tc.args, "--config="+h.ConfigPath)...)
			if exit.Code(code) != tc.want {
				t.Fatalf("exit = %d, want %d: %s", code, tc.want, stdout)
			}
			if !strings.Contains(stdout, tc.in) {
				t.Fatalf("stdout does not contain %q: %s", tc.in, stdout)
			}
		})
	}
}

func TestAnUnknownNounOrVerbExits2WithUsage(t *testing.T) {
	h := newCLIHarness(t)
	for _, args := range [][]string{
		{"admin"},
		{"admin", "nosuchnoun", "list"},
		{"admin", "user", "nosuchverb"},
	} {
		code, _, stderr := h.Run3(t, append(args, "--config="+h.ConfigPath)...)
		if code != int(exit.Usage) {
			t.Fatalf("%v: exit = %d, want 2", args, code)
		}
		if !strings.Contains(stderr, "usage: dillad admin") {
			t.Fatalf("%v: stderr = %q", args, stderr)
		}
	}
	// --help is not an error, and it is the only thing that writes to stdout here.
	code, stdout, stderr := h.Run3(t, "admin", "--help")
	if code != 0 {
		t.Fatalf("--help exit = %d", code)
	}
	if stdout == "" || stderr != "" {
		t.Fatalf("--help wrote stdout=%q stderr=%q; it must write only stdout", stdout, stderr)
	}
}

// A noun, and a verb, answer --help too, from stdout with exit 0: flag.ContinueOnError has already
// printed the message and the usage block for a bad flag, so the handler does not print them again.
func TestHelpAndBadFlagsAreAnsweredOnceAtEveryLevel(t *testing.T) {
	h := newCLIHarness(t)
	for _, args := range [][]string{
		{"admin", "user", "--help"},
		{"admin", "user", "show", "--help"},
		{"admin", "audit", "--help"},
		{"admin", "blob", "purge", "-h"},
	} {
		code, stdout, stderr := h.Run3(t, args...)
		if code != 0 || stdout == "" || stderr != "" {
			t.Fatalf("%v: exit %d, stdout=%q stderr=%q; want 0, text on stdout only", args, code, stdout, stderr)
		}
	}
	code, stdout, stderr := h.Run3(t, "admin", "user", "list", "--nope", "--config="+h.ConfigPath)
	if code != int(exit.Usage) || stdout != "" {
		t.Fatalf("a bad flag: exit %d, stdout=%q", code, stdout)
	}
	if n := strings.Count(stderr, "flag provided but not defined"); n != 1 {
		t.Fatalf("the bad-flag message was printed %d times:\n%s", n, stderr)
	}
}

func TestMissingAndMalformedArgumentsExit2AndUnknownRecordsExit65(t *testing.T) {
	h := newCLIHarness(t)
	for _, tc := range []struct {
		args []string
		want exit.Code
	}{
		{[]string{"admin", "user", "show"}, exit.Usage},
		{[]string{"admin", "user", "show", "--id=not-hex"}, exit.Usage},
		{[]string{"admin", "user", "show", "--id=" + id.New().String(), "--username=x"}, exit.Usage},
		{[]string{"admin", "user", "show", "extra"}, exit.Usage},
		{[]string{"admin", "user", "delete", "--username=nobody"}, exit.Usage}, // no --yes
		{[]string{"admin", "invite", "create", "--max-uses=0"}, exit.Usage},
		{[]string{"admin", "invite", "create", "--max-uses=1001"}, exit.Usage},
		{[]string{"admin", "invite", "create", "--ttl=0s"}, exit.Usage},
		{[]string{"admin", "invite", "create", "--ttl=721h"}, exit.Usage},
		{[]string{"admin", "blob", "purge", "--id=abcd", "--reason=x"}, exit.Usage},
		{[]string{"admin", "blob", "purge", "--id=" + strings.Repeat("ab", 32)}, exit.Usage}, // no --reason
		{[]string{"admin", "audit", "--limit=-1"}, exit.Usage},
		{[]string{"admin", "user", "show", "--id=" + id.New().String()}, exit.Data},
		{[]string{"admin", "user", "disable", "--username=nobody"}, exit.Data},
		{[]string{"admin", "device", "revoke", "--id=" + id.New().String()}, exit.Data},
		{[]string{"admin", "community", "show", "--id=" + id.New().String()}, exit.Data},
		{[]string{"admin", "invite", "revoke", "--id=" + id.New().String()}, exit.Data},
		{[]string{"admin", "invite", "create", "--community=" + id.New().String()}, exit.Data},
	} {
		code, out := h.Run(t, append(tc.args, "--config="+h.ConfigPath)...)
		if exit.Code(code) != tc.want {
			t.Fatalf("%v: exit = %d, want %d: %s", tc.args, code, tc.want, out)
		}
	}
	if rows, _ := h.Repo(t).ListAudit(t.Context(), 0, 100); len(rows) != 0 {
		t.Fatalf("refused verbs wrote audit rows: %+v", rows)
	}
}

func TestInviteCreatePrintsTheLinkOnceAndStoresOnlyTheHash(t *testing.T) {
	h := newCLIHarness(t)
	code, stdout := h.Run(t, "admin", "invite", "create", "--max-uses=1", "--ttl=24h", "--config="+h.ConfigPath)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	links := strings.Count(stdout, "/i/")
	if links != 1 {
		t.Fatalf("the link was printed %d times, want once", links)
	}
	if !strings.Contains(stdout, "https://chat.example/i/") {
		t.Fatalf("the link does not name the instance's domain: %q", stdout)
	}
	// A DISTINCT name: `code` is the int exit status returned by h.Run. SplitN(…, 2)[1] is
	// everything after the first "/i/"; the first line of that is the code, whatever the CLI
	// prints afterwards.
	inviteCode := strings.TrimSpace(strings.SplitN(strings.SplitN(stdout, "/i/", 2)[1], "\n", 2)[0])
	if inviteCode == "" {
		t.Fatalf("no invite code in stdout: %q", stdout)
	}
	rows := h.Invites(t)
	if len(rows) != 1 {
		t.Fatalf("invites = %d", len(rows))
	}
	if bytes.Contains(h.RawDB(t), []byte(inviteCode)) {
		t.Fatal("the plaintext invite code reached the database")
	}
	if !bytes.Equal(rows[0].CodeHash, auth.HashInviteCode(inviteCode)) {
		t.Fatal("the stored hash is not SHA-256 of the printed code")
	}
	if rows[0].MaxUses != 1 || rows[0].ExpiresAt-rows[0].Created != 24*3600 || rows[0].CreatedBy != nil {
		t.Fatalf("the invite row = %+v", rows[0])
	}
	// It is not in the log either, and the reference is.
	logs := h.LogOutput(t)
	if strings.Contains(logs, inviteCode) {
		t.Fatal("the plaintext invite code reached the log")
	}
	if ref := refOf(rows[0].CodeHash); !strings.Contains(logs, ref) {
		t.Fatalf("the log does not carry the invite's 8-hex reference %s: %q", ref, logs)
	}
}

func refOf(codeHash []byte) string {
	return hex.EncodeToString(codeHash[:4])
}

// `--grants-admin` and `--community` are the two things an operator reaches for beyond the
// defaults; an invite for another instance's community is not one the CLI will mint.
func TestInviteCreateHonoursCommunityAndGrantsAdmin(t *testing.T) {
	h := newCLIHarness(t)
	owner := h.SeedUser(t, "alice")
	cid := h.SeedCommunity(t, owner)
	if code, out := h.Run(t, "admin", "invite", "create", "--community="+cid.String(), "--grants-admin",
		"--max-uses=5", "--ttl=1h", "--config="+h.ConfigPath); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	rows := h.Invites(t)
	if len(rows) != 1 || rows[0].CommunityID == nil || *rows[0].CommunityID != cid || rows[0].GrantsAdmin != 1 || rows[0].MaxUses != 5 {
		t.Fatalf("the invite row = %+v", rows)
	}
}

func TestInviteRevokeEndsTheInviteAndListNamesItsStatus(t *testing.T) {
	h := newCLIHarness(t)
	if code, out := h.Run(t, "admin", "invite", "create", "--config="+h.ConfigPath); code != 0 {
		t.Fatalf("create: %d %s", code, out)
	}
	inv := h.Invites(t)[0]
	code, out := h.Run(t, "admin", "invite", "list", "--config="+h.ConfigPath)
	if code != 0 || !strings.Contains(out, inv.ID.String()) || !strings.Contains(out, "active") {
		t.Fatalf("list before revoke: %d %s", code, out)
	}
	if code, out := h.Run(t, "admin", "invite", "revoke", "--id="+inv.ID.String(), "--config="+h.ConfigPath); code != 0 || !strings.Contains(out, "revoked") {
		t.Fatalf("revoke: %d %s", code, out)
	}
	if _, err := h.Repo(t).RedeemInvite(t.Context(), inv.CodeHash, h.Now()); !errors.Is(err, store.ErrExhausted) {
		t.Fatalf("a revoked invite was redeemed: %v", err)
	}
	_, out = h.Run(t, "admin", "invite", "list", "--config="+h.ConfigPath)
	if !strings.Contains(out, "revoked") {
		t.Fatalf("list after revoke: %s", out)
	}
	// Revoking it again changes nothing and records nothing: the first revocation's time stands.
	first := h.Invites(t)[0].RevokedAt
	h.clk.Advance(time.Hour)
	if code, _ := h.Run(t, "admin", "invite", "revoke", "--id="+inv.ID.String(), "--config="+h.ConfigPath); code != 0 {
		t.Fatal("a second revoke failed")
	}
	if got := h.Invites(t)[0].RevokedAt; first == nil || got == nil || *got != *first {
		t.Fatalf("a second revoke moved revoked_at: %v -> %v", first, got)
	}
	n := 0
	rows, _ := h.Repo(t).ListAudit(t.Context(), 0, 100)
	for _, r := range rows {
		if r.Action == "invite.revoke" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d invite.revoke audit rows, want 1", n)
	}
}

// dillad admin is a SEPARATE PROCESS from dillad serve: it holds no handle on the
// running server's gateway registry and cannot close a socket, let alone "in the
// same transaction". What it can do is write the rows, and what serve does is
// notice. The mechanism, stated so the test can assert it rather than assert a
// harness artefact:
//
//   - the CLI sets users.disabled_at and deletes every session of that user, in
//     one transaction;
//   - the gateway re-checks the session's liveness on every heartbeat tick (the
//     heartbeat interval, default 30 s) by reading the session row, and closes the
//     connection when it is gone. The code is 4004 session_revoked, the one
//     protocol/02 already fixes for a revoked session, not the plan's 4401, which
//     protocol/02 does not define.
//
// So a live socket survives for at most one heartbeat, which the test asserts by
// driving the heartbeat rather than by waiting.
func TestUserDisableDeletesSessionsAndTheGatewayDropsTheSocket(t *testing.T) {
	h := newCLIHarness(t)
	uid := h.SeedUser(t, "alice")
	h.SeedFactor(t, uid)
	tok := h.SeedSession(t, uid)
	conn := h.OpenGatewayConn(t, uid) // a real gateway session against a real serve
	if code, _ := h.Run(t, "admin", "user", "disable", "--id="+uid.String(), "--config="+h.ConfigPath); code != 0 {
		t.Fatal("disable failed")
	}
	if _, err := h.Repo(t).GetSessionByHash(t.Context(), auth.TokenHash(tok), h.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the session survived: %v", err)
	}
	u, err := h.Repo(t).GetUser(t.Context(), uid)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.DisabledAt == nil {
		t.Fatal("disabled_at was not set")
	}
	// One heartbeat later the socket is gone, with the revocation close code.
	h.clk.Advance(h.HeartbeatInterval() + time.Second)
	code, err := conn.WaitClosed(t, 10*time.Second)
	if err != nil {
		t.Fatalf("the gateway connection was still open a heartbeat after the disable: %v", err)
	}
	if code != 4004 {
		t.Fatalf("close code = %d, want 4004", code)
	}
}

func TestEveryMutatingVerbWritesAnAuditRow(t *testing.T) {
	h := newCLIHarness(t)
	uid := h.SeedUser(t, "alice")
	gone := h.SeedUser(t, "bob")
	did := h.SeedDevice(t, uid)
	for _, args := range [][]string{
		{"admin", "user", "disable", "--id=" + uid.String()},
		{"admin", "user", "enable", "--id=" + uid.String()},
		{"admin", "user", "delete", "--id=" + gone.String(), "--yes"},
		{"admin", "device", "revoke", "--id=" + did.String()},
		{"admin", "invite", "create", "--max-uses=1", "--ttl=1h"},
		{"admin", "blob", "purge", "--id=" + strings.Repeat("cd", 32), "--reason=test"},
	} {
		if code, out := h.Run(t, append(args, "--config="+h.ConfigPath)...); code != 0 {
			t.Fatalf("%v failed: %s", args, out)
		}
	}
	h.Run(t, "admin", "invite", "revoke", "--id="+h.Invites(t)[0].ID.String(), "--config="+h.ConfigPath)
	rows, err := h.Repo(t).ListAudit(t.Context(), 0, 100)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	want := []string{"user.disable", "user.enable", "user.delete", "device.revoke", "invite.create", "invite.revoke", "blob.purge"}
	var got []string
	for _, r := range rows {
		got = append(got, r.Action)
		if r.Actor != nil {
			t.Fatalf("a CLI audit row names a user actor (%x); the CLI acts as the operator and writes actor NULL", *r.Actor)
		}
		if r.Detail == "" && r.Action == "invite.create" {
			t.Fatal("invite.create wrote no reference")
		}
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Fatalf("no audit row for %s: %v", w, got)
		}
	}
	// A read-only verb writes none.
	before := len(rows)
	for _, args := range [][]string{
		{"admin", "user", "list"}, {"admin", "user", "show", "--id=" + uid.String()},
		{"admin", "invite", "list"}, {"admin", "community", "list"}, {"admin", "audit"},
	} {
		if code, _ := h.Run(t, append(args, "--config="+h.ConfigPath)...); code != 0 {
			t.Fatalf("%v failed", args)
		}
	}
	after, _ := h.Repo(t).ListAudit(t.Context(), 0, 100)
	if len(after) != before {
		t.Fatalf("a read-only verb wrote %d audit rows", len(after)-before)
	}
}

// Deleting an account is the tombstone the API's DELETE /v1/accounts/me writes, plus what the
// operator can add: the handle stays reserved (R36), every session goes, every device is revoked,
// and the account cannot be brought back with `user enable`, because its keys are zeroed.
func TestUserDeleteTombstonesRevokesAndKeepsTheHandleReserved(t *testing.T) {
	h := newCLIHarness(t)
	uid := h.SeedUser(t, "mallory")
	tok := h.SeedSession(t, uid)
	did := h.SeedDevice(t, uid)

	code, out := h.Run(t, "admin", "user", "delete", "--username=mallory", "--config="+h.ConfigPath)
	if exit.Code(code) != exit.Usage || !strings.Contains(out, "--yes") {
		t.Fatalf("delete without --yes: exit %d: %s", code, out)
	}
	if u, _ := h.Repo(t).GetUser(t.Context(), uid); u.DeletedAt != nil {
		t.Fatal("a refused delete tombstoned the account")
	}

	if code, out := h.Run(t, "admin", "user", "delete", "--username=mallory", "--yes", "--config="+h.ConfigPath); code != 0 {
		t.Fatalf("delete: %d %s", code, out)
	}
	u, err := h.Repo(t).GetUser(t.Context(), uid)
	if err != nil || u.DeletedAt == nil || u.DisabledAt == nil {
		t.Fatalf("the account is not tombstoned and disabled: %+v %v", u, err)
	}
	if _, err := h.Repo(t).GetUserByUsername(t.Context(), "mallory"); err != nil {
		t.Fatalf("the handle was released (R36): %v", err)
	}
	if _, err := h.Repo(t).GetSessionByHash(t.Context(), auth.TokenHash(tok), h.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the session survived the delete: %v", err)
	}
	for _, d := range []id.ID{did} {
		row, err := h.Repo(t).GetDevice(t.Context(), d)
		if err != nil || row.RevokedAt == nil {
			t.Fatalf("device %s was not revoked: %+v %v", d, row, err)
		}
	}
	if code, out := h.Run(t, "admin", "user", "enable", "--id="+uid.String(), "--config="+h.ConfigPath); exit.Code(code) != exit.Data {
		t.Fatalf("enable of a deleted account: exit %d, want 65: %s", code, out)
	}
	if code, out := h.Run(t, "admin", "user", "show", "--id="+uid.String(), "--config="+h.ConfigPath); code != 0 || !strings.Contains(out, "deleted") {
		t.Fatalf("show of a deleted account: %d %s", code, out)
	}
}

func TestDeviceRevokeRevokesTheDeviceAndItsSessionsOnce(t *testing.T) {
	h := newCLIHarness(t)
	uid := h.SeedUser(t, "alice")
	tok := h.SeedSession(t, uid)
	row := func() store.SessionRow {
		s, err := h.Repo(t).GetSessionByHash(t.Context(), auth.TokenHash(tok), h.Now())
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		return s
	}
	did := row().DeviceID

	if code, out := h.Run(t, "admin", "device", "revoke", "--id="+did.String(), "--config="+h.ConfigPath); code != 0 || !strings.Contains(out, "revoked") {
		t.Fatalf("revoke: %d %s", code, out)
	}
	d, err := h.Repo(t).GetDevice(t.Context(), did)
	if err != nil || d.RevokedAt == nil {
		t.Fatalf("device not revoked: %+v %v", d, err)
	}
	if _, err := h.Repo(t).GetSessionByHash(t.Context(), auth.TokenHash(tok), h.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the device's session survived: %v", err)
	}
	first := *d.RevokedAt
	h.clk.Advance(time.Hour)
	if code, out := h.Run(t, "admin", "device", "revoke", "--id="+did.String(), "--config="+h.ConfigPath); code != 0 || !strings.Contains(out, "already") {
		t.Fatalf("second revoke: %d %s", code, out)
	}
	if d, _ := h.Repo(t).GetDevice(t.Context(), did); *d.RevokedAt != first {
		t.Fatalf("a second revoke moved revoked_at from %d to %d", first, *d.RevokedAt)
	}
}

// Enabling clears disabled_at and brings no session back.
func TestUserEnableClearsTheFlagAndUserShowReportsIt(t *testing.T) {
	h := newCLIHarness(t)
	uid := h.SeedUser(t, "alice")
	h.SeedSession(t, uid)
	h.Run(t, "admin", "user", "disable", "--username=alice", "--config="+h.ConfigPath)
	if _, out := h.Run(t, "admin", "user", "show", "--username=alice", "--config="+h.ConfigPath); !strings.Contains(out, "disabled") {
		t.Fatalf("show of a disabled user: %s", out)
	}
	if code, out := h.Run(t, "admin", "user", "enable", "--username=alice", "--config="+h.ConfigPath); code != 0 || !strings.Contains(out, "enabled") {
		t.Fatalf("enable: %d %s", code, out)
	}
	if u, _ := h.Repo(t).GetUser(t.Context(), uid); u.DisabledAt != nil {
		t.Fatalf("disabled_at = %d after enable", *u.DisabledAt)
	}
	if _, out := h.Run(t, "admin", "user", "show", "--id="+uid.String(), "--config="+h.ConfigPath); !strings.Contains(out, "active") {
		t.Fatalf("show of an enabled user: %s", out)
	}
}

// What a user typed into a display name must not reach the operator's terminal as an escape
// sequence.
func TestListingsNeverPrintControlCharacters(t *testing.T) {
	h := newCLIHarness(t)
	evil := store.UserRow{
		ID: id.New(), Username: "eve", Display: "\x1b[31mred\x07\t\u202etxt",
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: h.Now(),
	}
	if err := h.Repo(t).CreateUser(t.Context(), evil); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	for _, args := range [][]string{{"admin", "user", "list"}, {"admin", "user", "show", "--id=" + evil.ID.String()}} {
		code, out := h.Run(t, append(args, "--config="+h.ConfigPath)...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		for _, r := range out {
			if r == '\x1b' || r == '\x07' || r == '\u202e' {
				t.Fatalf("%v printed control character %q: %q", args, r, out)
			}
		}
	}
}

func TestUserListPagesThroughEveryUser(t *testing.T) {
	h := newCLIHarness(t)
	var names []string
	for _, n := range []string{"ann", "bo", "cy"} {
		h.SeedUser(t, n)
		names = append(names, n)
	}
	_, out := h.Run(t, "admin", "user", "list", "--config="+h.ConfigPath)
	for _, n := range names {
		if !strings.Contains(out, n) {
			t.Fatalf("user list is missing %s: %s", n, out)
		}
	}
	_, out = h.Run(t, "admin", "user", "list", "--limit=1", "--config="+h.ConfigPath)
	if got := strings.Count(out, "\n"); got != 2 { // the header and one row
		t.Fatalf("--limit=1 printed %d lines: %q", got, out)
	}
}

func TestCommunityShowReportsPolicyMembersAndChannels(t *testing.T) {
	h := newCLIHarness(t)
	owner := h.SeedUser(t, "alice")
	cid := h.SeedCommunity(t, owner)
	if err := h.Repo(t).PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: cid, UserID: owner, Joined: h.Now()}); err != nil {
		t.Fatalf("PutMember: %v", err)
	}
	code, out := h.Run(t, "admin", "community", "show", "--id="+cid.String(), "--config="+h.ConfigPath)
	if code != 0 {
		t.Fatalf("show: %d %s", code, out)
	}
	for _, want := range []string{"Kryptering", owner.String(), "policy_version", "members", "channels", "{}"} {
		if !strings.Contains(out, want) {
			t.Fatalf("community show lacks %q: %s", want, out)
		}
	}
}

func TestBlobPurgeRemovesTheBytesThroughTheSharedImplementation(t *testing.T) {
	h := newCLIHarness(t)
	store0, err := blob.Open(h.cfg.Blobs.Dir, h.cfg.Blobs.Backend)
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	defer func() { _ = store0.Close() }()
	body := []byte("an attachment's ciphertext")
	sum := sha256.Sum256(body)
	if _, _, err := store0.Put(t.Context(), sum[:], bytes.NewReader(body), 1<<20); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := h.Repo(t).PutBlob(t.Context(), store.BlobRow{BlobID: sum[:], Size: uint64(len(body)),
		StorageRef: blob.StorageRef(h.cfg.Blobs.Backend, sum[:]), Created: h.Now()}); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	hexID := hex.EncodeToString(sum[:])
	code, out := h.Run(t, "admin", "blob", "purge", "--id="+hexID, "--reason=court order 42", "--config="+h.ConfigPath)
	if code != 0 || !strings.Contains(out, "purged") {
		t.Fatalf("purge: %d %s", code, out)
	}
	if _, err := store0.Stat(sum[:]); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the file survived: %v", err)
	}
	if ok, _ := h.Repo(t).GetBlobTombstone(t.Context(), sum[:]); !ok {
		t.Fatal("no tombstone")
	}
	rows, _ := h.Repo(t).ListAudit(t.Context(), 0, 10)
	if len(rows) != 1 || rows[0].Action != "blob.purge" || rows[0].Target != hexID || rows[0].Detail != "court order 42" || rows[0].Actor != nil {
		t.Fatalf("audit = %+v", rows)
	}
}

func TestTheCLIRefusesANewerSchema(t *testing.T) {
	h := newCLIHarness(t)
	h.SetGooseVersion(t, 1_000_000)
	code, _, stderr := h.Run3(t, "admin", "user", "list", "--config="+h.ConfigPath)
	if code != int(exit.Config) {
		t.Fatalf("exit = %d, want %d", code, exit.Config)
	}
	if !strings.Contains(stderr, "1000000") {
		t.Fatalf("stderr = %q", stderr)
	}
	// The refusal is before any write: a mutating verb refuses too, and leaves no audit row.
	uid := h.SeedUser(t, "alice")
	if code, _ := h.Run(t, "admin", "user", "disable", "--id="+uid.String(), "--config="+h.ConfigPath); code != int(exit.Config) {
		t.Fatalf("disable through a newer schema: exit %d", code)
	}
	if u, _ := h.Repo(t).GetUser(t.Context(), uid); u.DisabledAt != nil {
		t.Fatal("a verb wrote through a schema it does not understand")
	}
}

func TestTheCLIRefusesAnOlderSchemaAndNamesTheFix(t *testing.T) {
	h := newCLIHarness(t)
	db, err := openSQLiteForTest(h.cfg.DB.Path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.ExecContext(t.Context(),
		`DELETE FROM goose_db_version WHERE version_id = (SELECT max(version_id) FROM goose_db_version)`); err != nil {
		t.Fatalf("roll the recorded version back: %v", err)
	}
	_ = db.Close()
	code, _, stderr := h.Run3(t, "admin", "user", "list", "--config="+h.ConfigPath)
	if code != int(exit.Config) || !strings.Contains(stderr, "migrate up") {
		t.Fatalf("exit %d, stderr %q; want 78 naming `migrate up`", code, stderr)
	}
}
