package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
)

// The parity world's call seeding (Task 28 wave 2, lane G): under -parity-fixture only, two control routes put a live
// call and a ring in front of the web client a reference is shot from, without an SFU or a media peer. They deliver
// the frames dillad itself sends — op 50 voice_state (who is in a call) and op 53 call_ring (a DM ring) — to the live
// connections of the users the fixture names, so the browser draws the voice row's people, a channel's preview and the
// ring banner from what it was sent. Like the diagnostics marker it exists only in this test host: dillad never mounts
// it, and the instance's own state (calls, rings, rooms) is untouched.
//
//	POST /debug/parity/voice {"to":[user…],"channel":c,"call":k,"participants":[{"user":u,"device":d,"flags":f}…]}
//	POST /debug/parity/ring  {"to":user,"call":k,"channel":c,"caller":u,"expires":unix}
//
// Ids are 32 lower-case hex digits. Both answer 204 once the frames are handed to the gateway, which delivers them to
// live connections only (an offline user gets nothing, as from dillad).

// parityDeliverer is where the seeded frames go; *gateway.Gateway is one.
type parityDeliverer interface {
	DeliverUser(user id.ID, f gateway.Frame)
}

type parityParticipant struct {
	User   string `json:"user"`
	Device string `json:"device"`
	Flags  uint64 `json:"flags"`
}

type parityVoice struct {
	To           []string            `json:"to"`
	Channel      string              `json:"channel"`
	Call         string              `json:"call"`
	Participants []parityParticipant `json:"participants"`
}

type parityRing struct {
	To      string `json:"to"`
	Call    string `json:"call"`
	Channel string `json:"channel"`
	Caller  string `json:"caller"`
	Expires uint64 `json:"expires"`
}

// voiceFlagsMax is the five bits of a voice_state's flags (02 § voice_state).
const voiceFlagsMax = 31

// parityCalls mounts the two routes; gw is read per request (a Restore replaces the server under the host).
func parityCalls(gw func() parityDeliverer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /debug/parity/voice", func(w http.ResponseWriter, r *http.Request) {
		var req parityVoice
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		frames, to, err := req.frames()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		g := gw()
		for _, u := range to {
			for _, f := range frames {
				g.DeliverUser(u, f)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /debug/parity/ring", func(w http.ResponseWriter, r *http.Request) {
		var req parityRing
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		to, f, err := req.frame()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gw().DeliverUser(to, f)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func parseIDs(names ...string) ([]id.ID, error) {
	out := make([]id.ID, 0, len(names))
	for _, s := range names {
		v, err := id.Parse(s)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// frames is one voice_state per participant, scoped to the call's channel.
func (v parityVoice) frames() ([]gateway.Frame, []id.ID, error) {
	ids, err := parseIDs(v.Channel, v.Call)
	if err != nil {
		return nil, nil, err
	}
	channel, call := ids[0], ids[1]
	to, err := parseIDs(v.To...)
	if err != nil {
		return nil, nil, err
	}
	frames := make([]gateway.Frame, 0, len(v.Participants))
	for _, p := range v.Participants {
		ids, err := parseIDs(p.User, p.Device)
		if err != nil {
			return nil, nil, err
		}
		if p.Flags > voiceFlagsMax {
			return nil, nil, fmt.Errorf("flags %d: a voice_state carries five bits", p.Flags)
		}
		payload, err := gateway.VoiceStatePayload(ids[0], ids[1], call, p.Flags)
		if err != nil {
			return nil, nil, err
		}
		frames = append(frames, gateway.Frame{Op: gateway.OpVoiceState, Payload: payload,
			Scope: gateway.FrameScope{Channels: []id.ID{channel}}})
	}
	return frames, to, nil
}

// frame is the ring's state 1 (ringing) call_ring, scoped to its DM.
func (g parityRing) frame() (id.ID, gateway.Frame, error) {
	ids, err := parseIDs(g.To, g.Call, g.Channel, g.Caller)
	if err != nil {
		return id.ID{}, gateway.Frame{}, err
	}
	payload, err := gateway.CallRingPayload(ids[1], ids[2], ids[3], 1, g.Expires)
	if err != nil {
		return id.ID{}, gateway.Frame{}, err
	}
	return ids[0], gateway.Frame{Op: gateway.OpCallRing, Payload: payload,
		Scope: gateway.FrameScope{Channels: []id.ID{ids[2]}}}, nil
}
