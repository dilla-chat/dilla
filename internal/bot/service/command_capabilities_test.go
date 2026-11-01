package service

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/polls"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

type commandCapabilitiesAuthorityFake struct {
	result  client.ActorAuthority
	present bool
	calls   int
	actor   id.ID
}

func (f *commandCapabilitiesAuthorityFake) BotActorAuthority(_ context.Context, community, actor id.ID) (client.ActorAuthority, bool, error) {
	f.calls++
	f.actor = actor
	return f.result, f.present, nil
}

func TestCommandCapabilitiesAllowsMemberSafeDiscoveryAndPreservesManagerGate(t *testing.T) {
	community, installation, bot, actor := interactionID(2), interactionID(3), interactionID(4), interactionID(6)
	authority := &commandCapabilitiesAuthorityFake{present: true}
	h := CommandCapabilitiesInteractions{Authority: authority, CommunityID: community, InstallationID: installation, BotUserID: bot,
		Capabilities: 0x3ff, Supported: []protocol.Operation{protocol.OpReminderList, protocol.OpMusicSearch, protocol.OpCapabilities,
			protocol.OpPollVote, protocol.OpPollClose, protocol.OpOverviewGet, protocol.OpPollCreate, protocol.OpPollVote, 999}}
	handler := h.Handlers()[protocol.OpCapabilities].Handle
	result, err := handler(context.Background(), InteractionActor{User: actor, Kind: 0}, protocol.Request{CommunityID: community, Operation: protocol.OpCapabilities})
	if err != nil || result.Status != 0 {
		t.Fatalf("member capabilities = %+v, %v; want success", result, err)
	}
	var data []any
	if err := cborx.Unmarshal(result.Data, &data); err != nil || len(data) != 5 || len(result.Data) > commandCapabilitiesDataLimit {
		t.Fatalf("capabilities = %x, decoded=%#v, err=%v", result.Data, data, err)
	}
	installBytes, installOK := data[1].([]byte)
	botBytes, botOK := data[2].([]byte)
	operations, operationsOK := data[4].([]any)
	if data[0] != uint64(1) || !installOK || !bytes.Equal(installBytes, installation[:]) || !botOK || !bytes.Equal(botBytes, bot[:]) || data[3] != uint64(1) || !operationsOK {
		t.Fatalf("capabilities tuple = %#v", data)
	}
	want := []uint64{3, 11, 70, 73, 74, 81}
	if len(operations) != len(want) {
		t.Fatalf("ordinary member operations = %#v, want %#v", operations, want)
	}
	for i := range want {
		if operations[i] != want[i] {
			t.Fatalf("ordinary member operations = %#v, want %#v", operations, want)
		}
	}
	if authority.calls != 1 || authority.actor != actor {
		t.Fatalf("authority calls=%d actor=%s, want one fresh lookup for sender %s", authority.calls, authority.actor, actor)
	}

	// Op 0 remains manager-only even though capability discovery itself is
	// member-safe. A delegated manager gets it through the current server bits.
	h.Supported = []protocol.Operation{protocol.OpOverviewGet, protocol.OpCapabilities}
	h.Authority = &commandCapabilitiesAuthorityFake{result: client.ActorAuthority{EffectiveBits: manageCommunityBit}, present: true}
	result, err = h.Handlers()[protocol.OpCapabilities].Handle(context.Background(), InteractionActor{User: actor}, protocol.Request{CommunityID: community, Operation: protocol.OpCapabilities})
	if err != nil || result.Status != 0 || cborx.Unmarshal(result.Data, &data) != nil || len(data[4].([]any)) != 2 {
		t.Fatalf("manager capability tuple=%#v err=%v", data, err)
	}

	h.Authority = &commandCapabilitiesAuthorityFake{present: false}
	result, err = h.Handlers()[protocol.OpCapabilities].Handle(context.Background(), InteractionActor{User: actor}, protocol.Request{CommunityID: community, Operation: protocol.OpCapabilities})
	if err != nil || result.Status != interactionStatusForbidden || len(result.Data) != 0 {
		t.Fatalf("nonmember capabilities = %+v, %v; want forbidden", result, err)
	}
}

func TestCommandCapabilitiesRejectsForeignCommunityAndBotActor(t *testing.T) {
	authority := &commandCapabilitiesAuthorityFake{present: true}
	community := interactionID(2)
	h := CommandCapabilitiesInteractions{Authority: authority, CommunityID: community, InstallationID: interactionID(3), BotUserID: interactionID(4), Supported: []protocol.Operation{protocol.OpCapabilities}}
	handler := h.Handlers()[protocol.OpCapabilities].Handle
	if _, err := handler(context.Background(), InteractionActor{User: interactionID(6)}, protocol.Request{CommunityID: interactionID(90), Operation: protocol.OpCapabilities}); err == nil || authority.calls != 0 {
		t.Fatalf("foreign community err=%v calls=%d, want refusal before lookup", err, authority.calls)
	}
	if _, err := handler(context.Background(), InteractionActor{User: interactionID(6), Kind: 1}, protocol.Request{CommunityID: community, Operation: protocol.OpCapabilities}); err == nil || authority.calls != 0 {
		t.Fatalf("bot actor err=%v calls=%d, want refusal before lookup", err, authority.calls)
	}
}

func TestCommandOperationAllowedUsesInstallationCapabilitiesAndActorPermissions(t *testing.T) {
	if commandOperationAllowed(protocol.OpMusicSearch, 0, client.ActorAuthority{}) {
		t.Fatal("music operation exposed without approved installation capability")
	}
	if !commandOperationAllowed(protocol.OpMusicSearch, commandCapMusic, client.ActorAuthority{}) {
		t.Fatal("music operation hidden without a target-independent community permission")
	}
	if commandOperationAllowed(protocol.OpMusicProvidersStatus, commandCapMusic, client.ActorAuthority{}) {
		t.Fatal("music provider status exposed without current manager permission")
	}
	if !commandOperationAllowed(protocol.OpMusicProvidersStatus, commandCapMusic, client.ActorAuthority{EffectiveBits: manageCommunityBit}) {
		t.Fatal("music provider status hidden from a current manager with approved music capability")
	}
}

func TestCommandCapabilitiesKeepsCreatorPollCloseDiscoverable(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	community, actor, pollID := pollServiceID(1), pollServiceID(2), pollServiceID(3)
	state := polls.NewStore(&pollTestState{}, community)
	poll := polls.Poll{ID: pollID, CommunityID: community, ChannelID: pollServiceID(4), CreatorID: actor,
		Question: "Q", Answers: []string{"A", "B"}, CreatedAt: uint64(now.Unix()), DueAt: uint64(now.Add(time.Hour).Unix())}
	if _, err := state.Create(ctx, pollID, [32]byte{1}, poll, now); err != nil {
		t.Fatal(err)
	}
	closeHandler := (PollInteractions{Store: state, Authority: &pollAuthorityFake{bits: pollPermView | pollPermReadHistory, member: true},
		CommunityID: community, Now: func() time.Time { return now }}).Handlers()[protocol.OpPollClose].Handle
	revision, expiry := uint64(1), uint64(now.Add(30*time.Second).Unix())
	closed, err := closeHandler(ctx, InteractionActor{User: actor, Kind: 0}, protocol.Request{
		RequestID: pollServiceID(9), CommunityID: community, Operation: protocol.OpPollClose,
		Revision: &revision, Expires: &expiry, Args: []any{pollID},
	})
	if err != nil || closed.Status != 0 {
		t.Fatalf("creator close status=%d err=%v; want creator with channel view/history to close", closed.Status, err)
	}
	operations := commandCapabilityOperations(t, CommandCapabilitiesInteractions{
		Authority: &commandCapabilitiesAuthorityFake{present: true}, CommunityID: community,
		InstallationID: interactionID(3), BotUserID: interactionID(4),
		Supported: []protocol.Operation{protocol.OpCapabilities, protocol.OpPollClose},
	}, actor)
	if !operations[protocol.OpPollClose] {
		t.Fatal("op3 hid registered poll close from a member who may be its creator")
	}
}

func TestCommandCapabilitiesKeepsMusicDiscoverableForCurrentCallDJ(t *testing.T) {
	ctx := context.Background()
	community, installation, channel, actor, callID := id.New(), id.New(), id.New(), id.New(), id.New()
	resolved := client.ActorAuthority{} // no community-wide connect permission
	session := &musicAuthoritySessionFake{
		actor: resolved, present: true, channel: musicPermissionControl,
		call:        client.ChannelCallState{CallID: &callID, GroupID: ptrID(id.New()), Participants: []client.CallParticipant{{UserID: actor}}},
		permissions: musicPermissionConnect | musicPermissionSpeak,
	}
	state := &musicAuthorityStoreFake{
		installation: store.BotInstallationRow{ID: installation, CommunityID: community, State: store.BotInstallationReady, Capabilities: musicBotCapability},
	}
	authority := &CommunityMusicInteractionAuthority{Session: session, Store: state, InstallationID: installation, CommunityID: community}
	binding := MusicCallBinding{CallID: callID, ChannelID: channel, Active: true}
	if ok, err := authority.CanUse(ctx, InteractionActor{User: actor}, binding); err != nil || !ok {
		t.Fatalf("CanUse = %v, %v; want current call member", ok, err)
	}
	if ok, err := authority.CanDJ(ctx, InteractionActor{User: actor}, binding); err != nil || !ok {
		t.Fatalf("CanDJ = %v, %v; want control_music in the voice channel", ok, err)
	}
	operations := commandCapabilityOperations(t, CommandCapabilitiesInteractions{
		Authority: &commandCapabilitiesAuthorityFake{result: resolved, present: true}, CommunityID: community,
		InstallationID: installation, BotUserID: id.New(), Capabilities: musicBotCapability,
		Supported: []protocol.Operation{protocol.OpCapabilities, protocol.OpMusicPause},
	}, actor)
	if !operations[protocol.OpMusicPause] {
		t.Fatal("op3 hid music pause from an authorized current-call DJ without community-wide connect")
	}
}

func commandCapabilityOperations(t *testing.T, h CommandCapabilitiesInteractions, actor id.ID) map[protocol.Operation]bool {
	t.Helper()
	result, err := h.Handlers()[protocol.OpCapabilities].Handle(context.Background(), InteractionActor{User: actor, Kind: 0},
		protocol.Request{CommunityID: h.CommunityID, Operation: protocol.OpCapabilities})
	if err != nil || result.Status != 0 {
		t.Fatalf("capabilities result=%+v err=%v", result, err)
	}
	var tuple []any
	if err := cborx.Unmarshal(result.Data, &tuple); err != nil || len(tuple) != 5 {
		t.Fatalf("capabilities tuple=%#v err=%v", tuple, err)
	}
	encoded, ok := tuple[4].([]any)
	if !ok {
		t.Fatalf("supported operations=%#v, want array", tuple[4])
	}
	operations := make(map[protocol.Operation]bool, len(encoded))
	for _, value := range encoded {
		n, ok := value.(uint64)
		if !ok {
			t.Fatalf("operation=%#v, want uint", value)
		}
		operations[protocol.Operation(n)] = true
	}
	return operations
}

// Task 10c: with Polls (bit 8) or Reminders (bit 9) off, op 3 no longer offers creating them, so
// /poll and /remind leave the picker; voting, closing, listing, cancelling, dismissing and the due
// list stay, so existing polls and reminders keep working.
func TestCommandCapabilitiesHidesOnlyCreationWhenPollsOrRemindersAreOff(t *testing.T) {
	community, installation, bot, actor := interactionID(2), interactionID(3), interactionID(4), interactionID(6)
	supported := []protocol.Operation{protocol.OpCapabilities, protocol.OpPollCreate, protocol.OpPollGet, protocol.OpPollList,
		protocol.OpPollVote, protocol.OpPollClose, protocol.OpPollVoters, protocol.OpReminderCreate, protocol.OpReminderList,
		protocol.OpReminderCancel, protocol.OpReminderDismiss, protocol.OpReminderDue}
	for _, tc := range []struct {
		name               string
		caps               uint16
		wantPoll, wantRemi bool
	}{
		{"both on", commandCapPolls | commandCapReminders, true, true},
		{"polls off", commandCapReminders, false, true},
		{"reminders off", commandCapPolls, true, false},
		{"both off (the original byte only)", 0xff, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := commandCapabilityOperations(t, CommandCapabilitiesInteractions{
				Authority: &commandCapabilitiesAuthorityFake{present: true}, CommunityID: community, InstallationID: installation,
				BotUserID: bot, Capabilities: tc.caps, Supported: supported,
			}, actor)
			if ops[protocol.OpPollCreate] != tc.wantPoll || ops[protocol.OpReminderCreate] != tc.wantRemi {
				t.Fatalf("create poll=%v reminder=%v, want %v %v", ops[protocol.OpPollCreate], ops[protocol.OpReminderCreate], tc.wantPoll, tc.wantRemi)
			}
			for _, kept := range []protocol.Operation{protocol.OpPollGet, protocol.OpPollList, protocol.OpPollVote, protocol.OpPollClose,
				protocol.OpPollVoters, protocol.OpReminderList, protocol.OpReminderCancel, protocol.OpReminderDismiss, protocol.OpReminderDue} {
				if !ops[kept] {
					t.Fatalf("op %d hidden with %s; existing polls and reminders must keep working", kept, tc.name)
				}
			}
		})
	}
}
