package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
)

// A call that is started, ended and started again on one channel within one second (the fake clock
// does not move) gets a room name that differs from its predecessor's, so ending the first or
// sweeping it can never delete the second's room. The name keeps the "<call_id hex>-" prefix the
// gate parses.
func TestACallsRoomNameNeverRepeatsWithinASecond(t *testing.T) {
	e, ch, tok, group, _ := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	var rooms []string
	for i := range 2 {
		status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
		if status != http.StatusCreated {
			t.Fatalf("start %d = %d", i, status)
		}
		callID := decodeCall(t, body).CallID
		row, err := e.Repo.GetVoiceSession(t.Context(), callID)
		if err != nil {
			t.Fatalf("GetVoiceSession: %v", err)
		}
		if !strings.HasPrefix(row.LivekitRoom, callID.String()+"-") {
			t.Fatalf("room %q lost the call id prefix", row.LivekitRoom)
		}
		rooms = append(rooms, row.LivekitRoom)
		if status, _ := e.Do(http.MethodDelete, "/v1/calls/"+callID.String(), tok, nil); status != http.StatusNoContent {
			t.Fatalf("end %d = %d", i, status)
		}
		// Ending a call closes its call group (DEV-46): the next call is on a freshly registered one.
		next := seedCallGroup(t, e, ch, ownerCommunityOf(t, e, ch), callGroupEpoch)
		seedLeaf(t, e, next, deviceOf(t, e, tok), 3, nil)
	}
	if rooms[0] == rooms[1] {
		t.Fatalf("two calls within one second share the room %q", rooms[0])
	}
}
