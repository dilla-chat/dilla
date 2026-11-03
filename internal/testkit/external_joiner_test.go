package testkit_test

// Invariant 4's Add clause applied to the leaf an external commit creates (deviation B36,
// checkExternalJoiner in internal/ds/commit.go). internal/ds cannot write these: its tests drive
// the delivery service with committed fixtures, which hold no external commit, and a refusal that
// happens AFTER the merge — which is where this clause runs, because the merged tree is the first
// place the joiner's leaf exists — needs a real one. Each case is a real dilla-core client whose
// external commit breaks exactly one part of the clause, uploaded to the in-process instance.
//
// Every case then has a well-formed external join accepted and a message decrypted: a refusal
// after the merge rolls the transaction back and evicts the merged handle, and a group that kept
// the refused leaf would fail right there.

import (
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/testkit"
)

func TestAnExternalCommitsLeafMustBeItsUploadersOwn(t *testing.T) {
	cases := []struct {
		name string
		// probe runs after alice's group `chat` exists and carol, frank, dave, erin, ulla and mallory
		// are enrolled; it must end in the refusal the case is about.
		probe string
		// the stdout substring the refusal prints
		want string
	}{
		{
			// A device that holds no leaf uploads another device's external commit: the leaf
			// would land in carol's name under frank's session.
			name:  "a joiner's leaf names another device",
			probe: "expect_reject E_COMMIT_INVALID rule=external_joiner external_join carol chat as=frank",
			want:  "E_COMMIT_INVALID: external_joiner",
		},
		{
			// The same on the resync path, which does NOT run the rest of the joiner clause: alice
			// holds a leaf, so her upload is a resync, and only the identity check stands between
			// her session and a leaf in carol's name.
			name:  "a resync's leaf names another device",
			probe: "expect_reject E_COMMIT_INVALID rule=external_joiner resync carol chat as=alice",
			want:  "E_COMMIT_INVALID: external_joiner",
		},
		{
			// frank's credential names frank, but the leaf's signature key is not his DSK.
			name:  "a joiner's leaf key is not its DSK",
			probe: "expect_reject E_COMMIT_INVALID rule=external_joiner external_join frank chat leaf_key=fresh",
			want:  "E_COMMIT_INVALID: external_joiner",
		},
		{
			// Hardening C, point (c): the resync path skips the rest of the joiner clause, but not
			// the key. alice holds a leaf, so her upload is a resync, and its leaf carries a key that
			// is not her DSK. She then resyncs honestly, which must still be accepted.
			name: "a resync's leaf key is not its DSK",
			probe: "expect_reject E_COMMIT_INVALID rule=external_joiner resync alice chat leaf_key=fresh\n" +
				"resync alice chat",
			want: "E_COMMIT_INVALID: external_joiner",
		},
		{
			// dave's user signed no device list at all.
			name:  "a joiner's user signed no device list",
			probe: "expect_reject E_COMMIT_INVALID rule=external_joiner external_join dave chat",
			want:  "E_COMMIT_INVALID: external_joiner",
		},
		{
			// erin's user signed a list whose entry for erin's device is revoked. Since web-2a task 8
			// the instance acts on that list at publish: erin's device is revoked and its sessions are
			// gone before it can upload anything (protocol/02 § Device sessions item 6), so the refusal
			// is the session's, not the joiner clause's.
			name:  "a joiner's own list revokes it",
			probe: "expect_reject E_UNAUTHENTICATED external_join erin chat",
			want:  "E_UNAUTHENTICATED",
		},
		{
			// ulla's user signed a list that omits ulla's device: the device is authenticated
			// and unrevoked, so the refusal is the joiner clause itself (task 8 made erin's
			// probe end at the session).
			name:  "a joiner's DSK is in no entry of its user's newest list",
			probe: "expect_reject E_COMMIT_INVALID rule=external_joiner external_join ulla chat",
			want:  "E_COMMIT_INVALID: external_joiner",
		},
		{
			// mallory's device row is revoked while its session still resolves.
			name: "a joiner's device is revoked",
			probe: "mark_revoked mallory\n" +
				"expect_reject E_COMMIT_INVALID rule=external_joiner external_join mallory chat",
			want: "E_COMMIT_INVALID: external_joiner",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := testkit.Start(t, testkit.Options{DataDir: t.TempDir()})
			t.Cleanup(h.Stop)
			result := runScript(t, h, `instance dilla
client alice
client carol
client frank
client dave device_list=none
client erin device_list=revoked
client ulla device_list=unlisted
client mallory
group chat kind=text target=e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5 community=none creator=alice
`+c.probe+`
external_join carol chat
sync alice
send alice chat the refusal left no leaf behind
expect_decrypts carol chat the refusal left no leaf behind
`)
			if !strings.Contains(result.Stdout, c.want) {
				t.Fatalf("no step reports %q:\n%s", c.want, result.Stdout)
			}
		})
	}
}
