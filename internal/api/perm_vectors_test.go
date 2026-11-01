package api_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// permVectors mirrors protocol/vectors/permissions.json, which is hand-frozen from
// this resolver (Go is the reference); packages/client-core/src/admin/perm.test.ts
// checks the TypeScript port against the same file.
type permVectors struct {
	Bits  map[string]uint64 `json:"bits"`
	Cases []struct {
		Name     string `json:"name"`
		Owner    string `json:"owner"`
		Everyone string `json:"everyone"`
		Roles    []struct {
			ID       string `json:"id"`
			Position uint64 `json:"position"`
			Allow    uint64 `json:"allow"`
			Deny     uint64 `json:"deny"`
		} `json:"roles"`
		Held       []string `json:"held"`
		Overwrites []struct {
			Kind   uint8  `json:"kind"`
			Target string `json:"target"`
			Allow  uint64 `json:"allow"`
			Deny   uint64 `json:"deny"`
		} `json:"overwrites"`
		User      string `json:"user"`
		Community uint64 `json:"community"`
		Channel   uint64 `json:"channel"`
	} `json:"cases"`
}

func vecID(t *testing.T, s string) id.ID {
	t.Helper()
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 16 {
		t.Fatalf("bad id %q", s)
	}
	var out id.ID
	copy(out[:], raw)
	return out
}

func TestPermissionVectors(t *testing.T) {
	raw, err := os.ReadFile("../../protocol/vectors/permissions.json")
	if err != nil {
		t.Fatal(err)
	}
	var v permVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) != 26 {
		t.Fatalf("cases = %d, want 26", len(v.Cases))
	}
	consts := map[string]api.Bits{
		"view_channel": api.PermViewChannel, "send_messages": api.PermSendMessages,
		"manage_messages": api.PermManageMessages, "pin_messages": api.PermPinMessages,
		"attach_files": api.PermAttachFiles, "add_reactions": api.PermAddReactions,
		"read_history": api.PermReadHistory, "connect": api.PermConnect, "speak": api.PermSpeak,
		"video": api.PermVideo, "screen_share": api.PermScreenShare, "create_invite": api.PermCreateInvite,
		"kick_members": api.PermKickMembers, "ban_members": api.PermBanMembers,
		"manage_channels": api.PermManageChannels, "manage_roles": api.PermManageRoles,
		"manage_community": api.PermManageCommunity, "view_audit_log": api.PermViewAuditLog,
		"mention_everyone": api.PermMentionEveryone, "bypass_slowmode": api.PermBypassSlowmode,
		"administrator": api.PermAdministrator, "mute_members": api.PermMuteMembers,
		"move_members": api.PermMoveMembers, "manage_nicknames": api.PermManageNicknames,
		"moderate_members": api.PermModerateMembers, "control_music": api.PermControlMusic,
	}
	if len(v.Bits) != len(consts) {
		t.Fatalf("bits = %d entries, want %d", len(v.Bits), len(consts))
	}
	for name, want := range consts {
		got, ok := v.Bits[name]
		if !ok || got != uint64(want) {
			t.Errorf("bit %s = %d (present %v), want %d", name, got, ok, uint64(want))
		}
	}
	for _, c := range v.Cases {
		snap := api.Snapshot{Owner: vecID(t, c.Owner), Everyone: vecID(t, c.Everyone), Held: map[id.ID]bool{}}
		for _, r := range c.Roles {
			snap.Roles = append(snap.Roles, store.RoleRow{ID: vecID(t, r.ID), Position: r.Position, Allow: r.Allow, Deny: r.Deny})
		}
		for _, h := range c.Held {
			snap.Held[vecID(t, h)] = true
		}
		for _, o := range c.Overwrites {
			snap.Overwrite = append(snap.Overwrite, store.OverwriteRow{TargetKind: o.Kind, TargetID: vecID(t, o.Target), Allow: o.Allow, Deny: o.Deny})
		}
		user := vecID(t, c.User)
		if got := uint64(snap.Resolve(user)); got != c.Community {
			t.Errorf("%s: Resolve = %d, want community %d", c.Name, got, c.Community)
		}
		if got := uint64(snap.ResolveChannel(user)); got != c.Channel {
			t.Errorf("%s: ResolveChannel = %d, want channel %d", c.Name, got, c.Channel)
		}
	}
}
