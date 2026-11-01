package client

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/jonasthim/dilla/internal/id"
)

const (
	maxCallTokenBytes = 16 << 10
	maxCallURLBytes   = 2048
	maxICEEntries     = 8
	maxICEURLs        = 8
	maxICEValueBytes  = 4096
	maxCallMembers    = 4096
)

// Call contains short-lived media credentials. Keep values out of diagnostics.
type Call struct {
	CallID, GroupID   id.ID
	LiveKitURL, Token string
	ICEServers        []ICEServer
	Caps              CallCaps
	Rung              []id.ID
}

func (Call) String() string               { return "Call{token:[redacted]}" }
func (Call) GoString() string             { return "client.Call{token:[redacted]}" }
func (Call) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type ICEServer struct {
	URLs                 []string
	Username, Credential string
}

func (ICEServer) String() string               { return "ICEServer{credential:[redacted]}" }
func (ICEServer) GoString() string             { return "client.ICEServer{credential:[redacted]}" }
func (ICEServer) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type CallCaps struct {
	MaxAudioBitrateBPS, MaxShareBitrateBPS uint64
	VP9                                    bool
}

type CallParticipant struct {
	UserID, DeviceID id.ID
	Flags            uint64
}

// ChannelCallState reports the channel's live call, if any. A nil ID means no call.
type ChannelCallState struct {
	GroupID, CallID *id.ID
	Participants    []CallParticipant
}

// StartCall opens or refreshes this channel's short-lived relay credentials.
// It does not join the MLS call group or make media ready; callers must first
// reconcile and join that group through the normal MLS flow.
func (s *Session) StartCall(ctx context.Context, channel id.ID) (Call, error) {
	var out Call
	channelPath, err := pathID(channel)
	if err != nil {
		return out, err
	}
	body, err := encode([]any{})
	if err != nil {
		return out, err
	}
	raw, _, err := s.do(ctx, http.MethodPost, "channels/"+channelPath+"/calls", nil, body, MaxSmallBody, true)
	if err != nil {
		return out, err
	}
	a, err := decode(raw, -1)
	if err != nil || len(a) != 6 && len(a) != 7 {
		return out, unknown()
	}
	if out.CallID, _ = asID(a[0]); out.CallID.IsZero() || !equalID(out.CallID, channel) {
		return Call{}, unknown()
	}
	if out.GroupID, _ = asID(a[1]); out.GroupID.IsZero() {
		return Call{}, unknown()
	}
	if out.LiveKitURL, _ = a[2].(string); !validLiveKitURL(out.LiveKitURL, s.c.allowLoopback) {
		return Call{}, unknown()
	}
	if out.Token, _ = a[3].(string); !validCallSecret(out.Token, maxCallTokenBytes) {
		return Call{}, unknown()
	}
	if out.ICEServers, err = parseICEServers(a[4]); err != nil {
		return Call{}, unknown()
	}
	if out.Caps, err = parseCallCaps(a[5]); err != nil {
		return Call{}, unknown()
	}
	if len(a) == 7 {
		rows, ok := a[6].([]any)
		if !ok || len(rows) > maxCallMembers {
			return Call{}, unknown()
		}
		out.Rung = make([]id.ID, len(rows))
		for i, v := range rows {
			if out.Rung[i], ok = asID(v); !ok {
				return Call{}, unknown()
			}
		}
	}
	return out, nil
}

func validLiveKitURL(raw string, allowLoopback bool) bool {
	if len(raw) == 0 || len(raw) > maxCallURLBytes || strings.ContainsAny(raw, "\r\n\x00") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.ForceQuery {
		return false
	}
	if u.Scheme == "wss" {
		return true
	}
	if u.Scheme != "ws" || !allowLoopback {
		return false
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() || host == "localhost"
}

func validCallSecret(v string, maxLength int) bool {
	return len(v) > 0 && len(v) <= maxLength && !strings.ContainsAny(v, "\r\n\x00")
}

func parseICEServers(v any) ([]ICEServer, error) {
	rows, ok := v.([]any)
	if !ok || len(rows) > maxICEEntries {
		return nil, ErrProtocol
	}
	out := make([]ICEServer, len(rows))
	for i, row := range rows {
		a, ok := row.([]any)
		if !ok || len(a) != 3 {
			return nil, ErrProtocol
		}
		urls, ok := a[0].([]any)
		if !ok || len(urls) == 0 || len(urls) > maxICEURLs {
			return nil, ErrProtocol
		}
		out[i].URLs = make([]string, len(urls))
		for j, raw := range urls {
			u, ok := raw.(string)
			if !ok || len(u) == 0 || len(u) > maxICEValueBytes || strings.ContainsAny(u, "\r\n\x00") {
				return nil, ErrProtocol
			}
			out[i].URLs[j] = u
		}
		if out[i].Username, ok = a[1].(string); !ok || len(out[i].Username) > maxICEValueBytes || strings.ContainsAny(out[i].Username, "\r\n\x00") {
			return nil, ErrProtocol
		}
		if out[i].Credential, ok = a[2].(string); !ok || len(out[i].Credential) > maxICEValueBytes || strings.ContainsAny(out[i].Credential, "\r\n\x00") {
			return nil, ErrProtocol
		}
	}
	return out, nil
}

func parseCallCaps(v any) (CallCaps, error) {
	var out CallCaps
	a, ok := v.([]any)
	if !ok || len(a) != 3 {
		return out, ErrProtocol
	}
	if out.MaxAudioBitrateBPS, ok = asUint(a[0]); !ok || out.MaxAudioBitrateBPS == 0 || out.MaxAudioBitrateBPS > 1_000_000_000 {
		return CallCaps{}, ErrProtocol
	}
	if out.MaxShareBitrateBPS, ok = asUint(a[1]); !ok || out.MaxShareBitrateBPS == 0 || out.MaxShareBitrateBPS > 1_000_000_000 {
		return CallCaps{}, ErrProtocol
	}
	vp9, ok := asUint(a[2])
	if !ok || vp9 > 1 {
		return CallCaps{}, ErrProtocol
	}
	out.VP9 = vp9 == 1
	return out, nil
}

func (s *Session) ChannelCall(ctx context.Context, channel id.ID) (ChannelCallState, error) {
	var out ChannelCallState
	p, err := pathID(channel)
	if err != nil {
		return out, err
	}
	raw, _, err := s.do(ctx, http.MethodGet, "channels/"+p+"/call", nil, nil, MaxSmallBody, false)
	if err != nil {
		return out, err
	}
	a, err := decode(raw, 3)
	if err != nil {
		return out, ErrProtocol
	}
	if a[0] != nil {
		v, ok := asID(a[0])
		if !ok {
			return ChannelCallState{}, ErrProtocol
		}
		out.GroupID = &v
	}
	if a[1] != nil {
		v, ok := asID(a[1])
		if !ok || !equalID(v, channel) {
			return ChannelCallState{}, ErrProtocol
		}
		out.CallID = &v
	}
	if out.CallID != nil && out.GroupID == nil {
		return ChannelCallState{}, ErrProtocol
	}
	rows, ok := a[2].([]any)
	if !ok || len(rows) > maxCallMembers || out.CallID == nil && len(rows) != 0 {
		return ChannelCallState{}, ErrProtocol
	}
	out.Participants = make([]CallParticipant, len(rows))
	for i, row := range rows {
		p, ok := row.([]any)
		if !ok || len(p) != 3 {
			return ChannelCallState{}, ErrProtocol
		}
		if out.Participants[i].UserID, ok = asID(p[0]); !ok {
			return ChannelCallState{}, ErrProtocol
		}
		if out.Participants[i].DeviceID, ok = asID(p[1]); !ok {
			return ChannelCallState{}, ErrProtocol
		}
		if out.Participants[i].Flags, ok = asUint(p[2]); !ok || out.Participants[i].Flags > 1<<32-1 {
			return ChannelCallState{}, ErrProtocol
		}
	}
	return out, nil
}

// MaxCallRTTMS is the largest RTT POST /v1/calls/{call_id}/rtt accepts, in milliseconds.
const MaxCallRTTMS = 5000

// ReportCallRTT reports this device's round-trip time to the SFU (whole milliseconds, at most MaxCallRTTMS) for the call's
// stage: the instance writes it as this device's dilla.rtt participant attribute (Signal task 8c, the bot's "63 ms"). The
// instance refuses more than one report per device every 1.5 s (429) and a device that is not in the call (404).
func (s *Session) ReportCallRTT(ctx context.Context, callID id.ID, ms uint32) error {
	if ms > MaxCallRTTMS {
		return ErrInvalidConfig
	}
	p, err := pathID(callID)
	if err != nil {
		return err
	}
	body, err := encode([]any{ms})
	if err != nil {
		return err
	}
	_, _, err = s.do(ctx, http.MethodPost, "calls/"+p+"/rtt", nil, body, MaxSmallBody, true)
	return err
}

// EndCall leaves a call, optionally ending it only if this device is alone.
func (s *Session) EndCall(ctx context.Context, callID id.ID, ifAlone bool) error {
	p, err := pathID(callID)
	if err != nil {
		return err
	}
	var q url.Values
	if ifAlone {
		q = url.Values{"if_alone": []string{"1"}}
	}
	_, _, err = s.do(ctx, http.MethodDelete, "calls/"+p, q, nil, MaxSmallBody, true)
	return err
}
