package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
)

// leave-reload e2e flake: closing an ended call's room makes LiveKit send room_finished at once, and
// a webhook worker can handle it before the end has announced anyone out. Handle reads the call as
// not live (the record ended it first) and forgets the room's state — so an announce that came after
// the close found nobody, and a device that was in the call stayed in every viewer's voice list. The
// stub SFU delivers that room_finished synchronously from DeleteRoom, the worst interleaving: every
// device seen in the room must still be announced out with flags 0, on DELETE's end and on a
// teardown's alike.
func TestARoomFinishedHandledDuringTheCloseStillAnnouncesEveryDeviceOut(t *testing.T) {
	for _, path := range []string{"delete", "teardown"} {
		t.Run(path, func(t *testing.T) {
			f := newEventsFixture(t, api.CallsConfig{})
			f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
			if fr := f.gw.last(); fr.Device != f.dev || fr.Flags == 0 {
				t.Fatalf("the join's voice_state = %+v, want the device in the call", fr)
			}
			finished := 0
			f.stub.mu.Lock()
			f.stub.onDelete = func(room string) {
				finished++
				f.events.Handle(context.WithoutCancel(t.Context()), &livekit.WebhookEvent{Event: webhook.EventRoomFinished,
					Id: "EV_" + id.New().String(), Room: &livekit.Room{Name: room}})
			}
			f.stub.mu.Unlock()
			before := len(f.gw.all())
			switch path {
			case "delete":
				if status, body := f.e.Do(http.MethodDelete, "/v1/calls/"+f.callID.String(), f.tok, nil); status != http.StatusNoContent {
					t.Fatalf("DELETE = %d %s", status, f.e.ErrCode(body))
				}
			case "teardown":
				f.calls.EndChannelCalls(t.Context(), []id.ID{f.ch}, "test")
				f.calls.ProcessTeardownsForTest(context.WithoutCancel(t.Context()))
			}
			f.events.FlushAnnouncements()
			if finished == 0 {
				t.Fatal("the end never closed the room")
			}
			out := false
			for _, fr := range f.gw.all()[before:] {
				if fr.Device == f.dev && fr.Call == f.callID && fr.Flags == 0 {
					out = true
				}
			}
			if !out {
				t.Fatalf("no flags-0 voice_state for the device after the end; frames since: %+v", f.gw.all()[before:])
			}
			if got := f.events.Participants(f.callID); len(got) != 0 {
				t.Fatalf("the ended call lists %+v, want nobody", got)
			}
		})
	}
}
