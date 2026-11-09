package catalog

import (
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
)

// Task 22: the admin surfaces' app operations are hidden entries (handler kind 1) offered with the feature v1 required,
// authorised by the permission v1 required: welcome settings with Welcome and manage_community; moderation evidence
// with any moderation feature (its view_audit_log or manage_messages check is the dispatcher's, an either-or a single
// permission_bit cannot say); the Bot page's status read with manage_community, whatever the features.
func TestAdminSurfacesAreHiddenEntriesWithTheirV1Rules(t *testing.T) {
	const welcome, warnings, removal, kick, ban = 1 << 1, 1 << 4, 1 << 5, 1 << 6, 1 << 7
	for name, w := range map[string]struct {
		op         protocol.Operation
		capability uint64
		perm       uint64
		mutation   bool
	}{
		"welcome read":    {protocol.OpAppSettingsGet, welcome, permManageCommunity, false},
		"welcome save":    {protocol.OpAppSettingsPut, welcome, permManageCommunity, true},
		"evidence (warn)": {protocol.OpAppModerationEvidence, warnings, 0, true},
		"evidence (rm)":   {protocol.OpAppModerationEvidence, removal, 0, true},
		"evidence (kick)": {protocol.OpAppModerationEvidence, kick, 0, true},
		"evidence (ban)":  {protocol.OpAppModerationEvidence, ban, 0, true},
		"bot status":      {protocol.OpReadBotStatus, 0, permManageCommunity, false},
	} {
		rules := Operations(FeaturesOf(w.capability))
		r, ok := rules[uint64(w.op)]
		if !ok {
			t.Fatalf("%s: operation %d is not offered with capability %b", name, w.op, w.capability)
		}
		if r.Permission != w.perm || r.Read == w.mutation || AppMutation(w.op) != w.mutation || r.ChannelScoped {
			t.Fatalf("%s: rule %+v (mutation %v)", name, r, AppMutation(w.op))
		}
		for _, c := range Commands(FeaturesOf(w.capability)) {
			if c.Operation == w.op && c.HandlerKind != HandlerRead {
				t.Fatalf("%s: handler kind %d, want the hidden kind 1", name, c.HandlerKind)
			}
		}
	}
	none := Operations(FeaturesOf(0))
	for _, op := range []protocol.Operation{protocol.OpAppSettingsGet, protocol.OpAppSettingsPut, protocol.OpAppModerationEvidence} {
		if _, ok := none[uint64(op)]; ok {
			t.Fatalf("operation %d is offered with no feature", op)
		}
	}
	if protocol.OpAppSettingsGet != protocol.OpSettingsGet+AppOperationBase || protocol.OpAppSettingsPut != protocol.OpSettingsPut+AppOperationBase ||
		protocol.OpAppModerationEvidence != protocol.OpModerationEvidence+AppOperationBase {
		t.Fatal("an admin app operation is not 100 + its v1 operation")
	}
}
