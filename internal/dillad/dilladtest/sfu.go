package dilladtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"slices"
	"sync"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/sfu"
)

// Default ports of the harness SFU, the same as dillad's own livekit.port and livekit.udp_port.
const (
	DefaultSFUPort    = 7880
	DefaultSFUUDPPort = 7882
)

// testSFUConfig is the in-process LiveKit a browser can reach on a one-interface box and on a CI
// runner: node_ip 127.0.0.1 with the loopback candidate on, **and** advertise_internal_ip, which
// also offers the host's LAN address. Firefox's nICEr never pairs a non-loopback local candidate
// with a loopback remote one (ice_component.cpp:1088-1090 at FIREFOX_155_0_RELEASE), so with
// loopback alone Firefox fails "could not establish pc connection" while Chromium connects (G35
// runs L2 and A1). sfu.DefaultConfig() leaves AdvertiseInternalIP false and must never be used
// unmodified for a browser test.
func testSFUConfig(port, udpPort int, apiKey, secret string) sfu.Config {
	c := sfu.DefaultConfig()
	c.Port = port
	c.UDPPort = udpPort
	c.BindAddress = "127.0.0.1"
	c.NodeIP = "127.0.0.1"
	c.EnableLoopbackCandidate = true
	c.AdvertiseInternalIP = true
	c.APIKey = apiKey
	c.APISecret = secret
	return c
}

// sfuConfig is testSFUConfig with the host's own SFU options applied.
func (o HostOptions) sfuConfig(port, udpPort int, apiKey, secret string) sfu.Config {
	c := testSFUConfig(port, udpPort, apiKey, secret)
	if o.SFUNoInternalIP {
		c.AdvertiseInternalIP = false
	}
	c.TestAV1 = o.SFUEnableAV1
	if o.SFUDepartureTimeout > 0 {
		c.DepartureTimeout = o.SFUDepartureTimeout
	}
	return c
}

// newSFUSecret is a fresh 64-hex-character LiveKit API secret, well over the 32 characters
// sfu.Config.YAML insists on.
func newSFUSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SFU is the in-process LiveKit the host started, or nil when HostOptions.SFU was false.
func (h *Host) SFU() *sfu.Server { return h.sfu }

// debugRooms are the rooms POST /debug/sfu/token opened (create: true). They belong to no call, so
// the call routes' room sweep (api.Calls.SweepRooms, every api.RoomSweepInterval) would delete them
// and disconnect a browser test mid-run; harnessSFU hides them from it. It outlives a Restore, as
// the SFU does.
type debugRooms struct {
	mu    sync.Mutex
	names map[string]struct{}
	voice map[string][]*livekit.ParticipantInfo
}

// StubVoiceParticipant installs a deterministic SFU presence for a stored call fixture.
// The audience resolver still reads the real voice session and channel permissions.
func (h *Host) StubVoiceParticipant(room, identity string) {
	h.debugRooms.mu.Lock()
	defer h.debugRooms.mu.Unlock()
	if h.debugRooms.voice == nil {
		h.debugRooms.voice = make(map[string][]*livekit.ParticipantInfo)
	}
	h.debugRooms.voice[room] = append(h.debugRooms.voice[room], &livekit.ParticipantInfo{Identity: identity, State: livekit.ParticipantInfo_ACTIVE})
}

func (d *debugRooms) add(room string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.names == nil {
		d.names = map[string]struct{}{}
	}
	d.names[room] = struct{}{}
}

func (d *debugRooms) has(room string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.names[room]
	return ok
}

// harnessSFU is the in-process LiveKit as the instance sees it: every method is the server's own,
// except that Rooms leaves out the debug rooms. Only the room sweep and a device cut list rooms, so a
// debug room — raw debug tokens, connected straight to LiveKit (plan MD-13) — is never swept or cut,
// while every room the call routes open is swept exactly as in production.
type harnessSFU struct {
	*sfu.Server
	debug *debugRooms
}

func (s harnessSFU) Participants(ctx context.Context, room string) ([]*livekit.ParticipantInfo, error) {
	s.debug.mu.Lock()
	participants, ok := s.debug.voice[room]
	s.debug.mu.Unlock()
	if ok {
		return participants, nil
	}
	return s.Server.Participants(ctx, room)
}

func (s harnessSFU) Rooms(ctx context.Context) ([]string, error) {
	rooms, err := s.Server.Rooms(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(rooms, s.debug.has), nil
}

// withheld reports whether the webhook receiver drops event instead of delivering it.
func (h *Host) withheld(event string) bool {
	set, _ := h.withhold.Load().(map[string]struct{})
	_, held := set[event]
	return held
}

// setWithheld turns the holding back of one webhook event on or off; the set is replaced whole.
func (h *Host) setWithheld(event string, on bool) {
	h.withholdMu.Lock()
	defer h.withholdMu.Unlock()
	old, _ := h.withhold.Load().(map[string]struct{})
	next := make(map[string]struct{}, len(old)+1)
	for e := range old {
		next[e] = struct{}{}
	}
	if on {
		next[event] = struct{}{}
	} else {
		delete(next, event)
	}
	h.withhold.Store(next)
}

// mountSFU adds the SFU routes to the control listener. They are test-only and never on the
// public mux: GET /debug/sfu names the SFU's signalling URLs, and POST /debug/sfu/token mints a raw
// LiveKit token with every grant for any room and identity, which is exactly what a browser test
// needs to connect to LiveKit directly — the SFU-as-adversary case dillad's /rtc gate cannot
// produce (plan MD-13).
func (h *Host) mountSFU(mux *http.ServeMux) {
	mux.HandleFunc("GET /debug/sfu", func(w http.ResponseWriter, _ *http.Request) {
		if h.sfu == nil {
			http.Error(w, "this host runs no SFU (start it with -sfu)", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]string{"url": h.sfu.URL(), "http_url": h.sfu.HTTPURL()})
	})
	// POST /debug/sfu/withhold {"event":"participant_left","on":true} drops that webhook event before
	// it reaches the instance (so a test can hold a departure back, as a lost webhook would); on:false
	// restores delivery. A withheld event is dropped, not queued: it is never delivered later and turning
	// the hold off does not replay it. Only participant_left is accepted; any other name is 400.
	mux.HandleFunc("POST /debug/sfu/withhold", func(w http.ResponseWriter, r *http.Request) {
		if h.sfu == nil {
			http.Error(w, "this host runs no SFU (start it with -sfu)", http.StatusNotFound)
			return
		}
		var body struct {
			Event string `json:"event"`
			On    bool   `json:"on"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Event != "participant_left" {
			http.Error(w, `only "participant_left" can be withheld`, http.StatusBadRequest)
			return
		}
		h.setWithheld(body.Event, body.On)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /debug/sfu/token", func(w http.ResponseWriter, r *http.Request) {
		if h.sfu == nil {
			http.Error(w, "this host runs no SFU (start it with -sfu)", http.StatusNotFound)
			return
		}
		var body struct {
			Room     string `json:"room"`
			Identity string `json:"identity"`
			// Create asks for the room to exist before the token is used: room.auto_create is
			// false (task 10, MD-16), so LiveKit refuses a join to a room nobody opened with
			// CreateRoom. A test that wants that refusal leaves it false.
			Create bool `json:"create"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Room == "" || body.Identity == "" {
			http.Error(w, "room and identity are both required", http.StatusBadRequest)
			return
		}
		if body.Create {
			if err := h.sfu.CreateRoom(r.Context(), body.Room); err != nil {
				http.Error(w, "create room: "+err.Error(), http.StatusInternalServerError)
				return
			}
			h.debugRooms.add(body.Room)
		}
		// A raw debug token carries every grant, data included (MD-13: the canary and the harness
		// smoke connect straight to LiveKit, never through the /rtc gate).
		token, err := h.sfu.Token(body.Room, body.Identity, &livekit.ParticipantPermission{
			CanSubscribe: true, CanPublish: true, CanPublishData: true,
		}, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"url": h.sfu.URL(), "http_url": h.sfu.HTTPURL(), "token": token})
	})
}
