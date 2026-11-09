package service

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// communityPermsFake is a feed view with community-wide bits too (Task 22: moderation evidence and the Bot's status).
type communityPermsFake map[id.ID]uint64

func (f communityPermsFake) IsMember(user id.ID) bool                { _, ok := f[user]; return ok }
func (f communityPermsFake) ChannelPermissions(user, _ id.ID) uint64 { return f[user] }
func (f communityPermsFake) Permissions(user id.ID) uint64           { return f[user] }

// Task 22: the admin surfaces' app operations. Welcome settings (101 read, 102 save) run v1 1 and 2 with the v1 arguments
// (section 5, then the document), the save's expected revision first; moderation evidence (161) runs v1 61 with its
// receipt id as 16 bytes and revision 0.
func TestSealedAdminSurfacesRunTheV1Handler(t *testing.T) {
	d, h := sealedFixture(protocol.OpSettingsGet, protocol.OpSettingsPut, protocol.OpModerationEvidence)
	d.Feed = communityPermsFake{interactionID(5): 1<<0 | 1<<2}
	rev := uint64(4)
	h.results[protocol.OpSettingsGet] = []InteractionResult{{Revision: &rev, Data: []byte{0x80}}}
	got := collect(d, sealedRequest(protocol.OpAppSettingsGet, uint64(5)))
	var body []any
	if len(got) != 1 || got[0].Status != 200 || cborx.Unmarshal(got[0].Body, &body) != nil || len(body) != 2 || body[0] != uint64(4) {
		t.Fatalf("settings get = %+v", got)
	}
	if r := h.calls[0].req; r.Operation != protocol.OpSettingsGet || r.Revision != nil || len(r.Args) != 1 || r.Args[0] != uint64(5) {
		t.Fatalf("v1 settings get = %+v", r)
	}
	channel := interactionID(62)
	doc, err := cborx.Marshal([]any{uint64(1), channel[:], "Welcome {member} to {server}!", "Dilla"})
	if err != nil {
		t.Fatal(err)
	}
	if got := collect(d, sealedRequest(protocol.OpAppSettingsPut, uint64(4), uint64(5), doc)); len(got) != 1 || got[0].Status != 200 {
		t.Fatalf("settings put = %+v", got)
	}
	if r := h.calls[1].req; r.Operation != protocol.OpSettingsPut || r.Revision == nil || *r.Revision != 4 || r.Expires == nil || len(r.Args) != 2 {
		t.Fatalf("v1 settings put = %+v", r)
	}
	receipt := interactionID(60)
	if got := collect(d, sealedRequest(protocol.OpAppModerationEvidence, uint64(0), receipt[:], "spam", "buy now")); len(got) != 1 || got[0].Status != 200 {
		t.Fatalf("evidence = %+v", got)
	}
	if r := h.calls[2].req; r.Operation != protocol.OpModerationEvidence || r.Revision == nil || *r.Revision != 0 || len(r.Args) != 3 || r.Args[0] != receipt {
		t.Fatalf("v1 evidence = %+v", r)
	}
}

// v1 admitted moderation evidence only from a member holding view_audit_log or manage_messages (command_capabilities.go);
// the sealed form reads both from the feed, and fails closed with no feed.
func TestSealedModerationEvidenceNeedsAuditLogOrManageMessages(t *testing.T) {
	d, h := sealedFixture(protocol.OpModerationEvidence)
	receipt := interactionID(61)
	request := func() protocol.SealedRequest {
		return sealedRequest(protocol.OpAppModerationEvidence, uint64(0), receipt[:], "spam", "")
	}
	if got := collect(d, request()); len(got) != 1 || got[0].Status != 403 || len(h.calls) != 0 {
		t.Fatalf("no feed = %+v (%d calls)", got, len(h.calls))
	}
	d.Feed = communityPermsFake{interactionID(5): 1<<0 | 1<<1}
	if got := collect(d, request()); len(got) != 1 || got[0].Status != 403 || len(h.calls) != 0 {
		t.Fatalf("a member with neither bit = %+v (%d calls)", got, len(h.calls))
	}
	for _, bits := range []uint64{1<<0 | 1<<17, 1<<0 | 1<<2} {
		d.Feed = communityPermsFake{interactionID(5): bits}
		if got := collect(d, request()); len(got) != 1 || got[0].Status != 200 {
			t.Fatalf("bits %b = %+v", bits, got)
		}
	}
}

// Task 22: the Bot page's status read (94) answers [null, [1, needs_re_add, starts]] to a member who manages the community:
// the Bot channels it cannot join on its own (Task 16's NeedsReAdd), sorted, and the last music starts' first-frame times.
func TestSealedBotStatusAnswersNeedsReAddAndMusicStarts(t *testing.T) {
	d, _ := sealedFixture()
	a, b := interactionID(71), interactionID(70)
	d.Status = func() BotStatus {
		return BotStatus{NeedsReAdd: []id.ID{a, b}, MusicStarts: []MusicStart{{FirstFrame: 640 * time.Millisecond, Cold: true}, {FirstFrame: 180 * time.Millisecond}}}
	}
	if got := collect(d, sealedRequest(protocol.OpReadBotStatus)); len(got) != 1 || got[0].Status != 403 {
		t.Fatalf("no feed = %+v", got)
	}
	d.Feed = communityPermsFake{interactionID(5): 1<<0 | 1<<2}
	if got := collect(d, sealedRequest(protocol.OpReadBotStatus)); len(got) != 1 || got[0].Status != 403 {
		t.Fatalf("a member who does not manage the community = %+v", got)
	}
	d.Feed = communityPermsFake{interactionID(5): 1<<0 | 1<<16}
	got := collect(d, sealedRequest(protocol.OpReadBotStatus))
	var body []any
	if len(got) != 1 || got[0].Status != 200 || !got[0].Final || cborx.Unmarshal(got[0].Body, &body) != nil || len(body) != 2 || body[0] != nil {
		t.Fatalf("status = %+v", got)
	}
	raw, _ := body[1].([]byte)
	var data []any
	if cborx.Unmarshal(raw, &data) != nil || len(data) != 3 || data[0] != uint64(1) {
		t.Fatalf("status data = %v", body)
	}
	channels, _ := data[1].([]any)
	starts, _ := data[2].([]any)
	first, _ := channels[0].([]byte)
	second, _ := channels[1].([]byte)
	if len(channels) != 2 || string(first) != string(b[:]) || string(second) != string(a[:]) {
		t.Fatalf("needs re-add = %v", channels)
	}
	s0, _ := starts[0].([]any)
	s1, _ := starts[1].([]any)
	if len(starts) != 2 || s0[0] != uint64(640) || s0[1] != uint64(1) || s1[0] != uint64(180) || s1[1] != uint64(0) {
		t.Fatalf("starts = %v", starts)
	}
	if got := collect(d, sealedRequest(protocol.OpReadBotStatus, uint64(1))); len(got) != 1 || got[0].Status != 400 {
		t.Fatalf("an argument = %+v", got)
	}
	d.Status = nil
	if got := collect(d, sealedRequest(protocol.OpReadBotStatus)); len(got) != 1 || got[0].Status == 200 {
		t.Fatalf("no status source = %+v", got)
	}
}
