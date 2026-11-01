package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jonasthim/dilla/internal/cborx"
)

func TestStartCallBindsCallToChannel(t *testing.T) {
	channel, group := testID(41), testID(42)
	var gotMethod, gotPath, gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		if r.Method == http.MethodPost {
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			var request []any
			if err := cborx.Unmarshal(body, &request); err != nil || len(request) != 0 {
				t.Errorf("start body = %x, want []", body)
			}
		}
		reply(w, http.StatusCreated, callStartBody(channel, group))
	}, 0)
	s, err := c.Session("call-session")
	if err != nil {
		t.Fatal(err)
	}
	call, err := s.StartCall(context.Background(), channel)
	if err != nil {
		t.Fatal(err)
	}
	if call.CallID != channel || call.GroupID != group || call.LiveKitURL != "wss://media.example.invalid/rtc" || call.Token != "secret-token" || call.Caps.MaxAudioBitrateBPS != 64000 || !call.Caps.VP9 {
		t.Fatalf("unexpected call response: %#v", call)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/channels/"+channel.String()+"/calls" || gotQuery != "" {
		t.Fatalf("request = %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	if call.String() != "Call{token:[redacted]}" {
		t.Fatalf("String leaked or changed: %q", call.String())
	}
	encoded, err := json.Marshal(call)
	if err != nil || strings.Contains(string(encoded), call.Token) {
		t.Fatal("call JSON leaked relay credential")
	}
	if _, err := url.ParseRequestURI(call.LiveKitURL); err != nil {
		t.Fatal(err)
	}
}

func TestCallRoutesDeclareDocumentedStatuses(t *testing.T) {
	post := "channels/" + testID(41).String() + "/calls"
	get := "channels/" + testID(41).String() + "/call"
	del := "calls/" + testID(41).String()
	if want, ok := expectedStatus(http.MethodPost, post); !ok || want != http.StatusCreated {
		t.Fatalf("start status = (%d, %v), want 201", want, ok)
	}
	if !statusAllowed(http.MethodPost, post, http.StatusOK, http.StatusCreated) {
		t.Fatal("start replay status 200 was rejected")
	}
	if want, ok := expectedStatus(http.MethodGet, get); !ok || want != http.StatusOK {
		t.Fatalf("channel call status = (%d, %v), want 200", want, ok)
	}
	if want, ok := expectedStatus(http.MethodDelete, del); !ok || want != http.StatusNoContent {
		t.Fatalf("end call status = (%d, %v), want 204", want, ok)
	}
}

func TestStartCallRejectsForeignCallAndMalformedSecretFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []any
	}{
		{name: "foreign channel", body: callStartBody(testID(99), testID(42))},
		{name: "http url", body: []any{testID(41), testID(42), "http://voice.example.test/rtc", "secret-token", []any{}, []any{uint64(64000), uint64(1000000), uint64(0)}}},
		{name: "empty token", body: []any{testID(41), testID(42), "wss://voice.example.test/rtc", "", []any{}, []any{uint64(64000), uint64(1000000), uint64(0)}}},
		{name: "bad caps", body: []any{testID(41), testID(42), "wss://voice.example.test/rtc", "secret-token", []any{}, []any{uint64(64000), uint64(1000000), uint64(2)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, http.StatusOK, discovery(testID(99)))
					return
				}
				reply(w, http.StatusCreated, tc.body)
			}, 0)
			s, _ := c.Session("call-session")
			if _, err := s.StartCall(context.Background(), testID(41)); err == nil {
				t.Fatal("StartCall accepted malformed response")
			}
		})
	}
}

func TestLiveKitURLAllowsWebSocketOnlyForConfiguredLoopback(t *testing.T) {
	if !validLiveKitURL("ws://127.0.0.1:8524", true) {
		t.Fatal("configured loopback WebSocket URL was rejected")
	}
	for _, raw := range []string{
		"ws://media.example.invalid/rtc",
		"ws://127.0.0.1:8524?token=secret",
		"ws://user@127.0.0.1:8524",
	} {
		if validLiveKitURL(raw, true) {
			t.Fatalf("unsafe WebSocket URL accepted: %q", raw)
		}
	}
	if validLiveKitURL("ws://127.0.0.1:8524", false) {
		t.Fatal("loopback WebSocket URL accepted without loopback development opt-in")
	}
	if !validLiveKitURL("wss://media.example.invalid/rtc", false) {
		t.Fatal("secure WebSocket URL rejected")
	}
}

func TestChannelCallAndEndCallUseExpectedRoutes(t *testing.T) {
	channel, group, callID := testID(41), testID(42), testID(41)
	var gotPath, gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		switch r.Method {
		case http.MethodGet:
			reply(w, http.StatusOK, []any{group, callID, []any{[]any{testID(43), testID(44), uint64(1)}}})
		case http.MethodDelete:
			if len(r.URL.Query()) == 0 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Header().Set("Content-Type", "application/cbor")
			w.WriteHeader(http.StatusNoContent)
		}
	}, 0)
	s, _ := c.Session("call-session")
	state, err := s.ChannelCall(context.Background(), channel)
	if err != nil || state.GroupID == nil || *state.GroupID != group || state.CallID == nil || *state.CallID != callID || len(state.Participants) != 1 {
		t.Fatalf("ChannelCall = %#v, %v", state, err)
	}
	if err := s.EndCall(context.Background(), callID, true); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/calls/"+callID.String() || gotQuery != "if_alone=1" {
		t.Fatalf("delete route = %s?%s", gotPath, gotQuery)
	}
}

func TestChannelCallAcceptsOpenGroupWithoutLiveCall(t *testing.T) {
	channel, group := testID(41), testID(42)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		reply(w, http.StatusOK, []any{group, nil, []any{}})
	}, 0)
	s, _ := c.Session("call-session")
	state, err := s.ChannelCall(context.Background(), channel)
	if err != nil || state.GroupID == nil || *state.GroupID != group || state.CallID != nil || len(state.Participants) != 0 {
		t.Fatalf("idle channel call = %#v, %v", state, err)
	}
}

func callStartBody(channel, group interface{}) []any {
	return []any{channel, group, "wss://media.example.invalid/rtc", "secret-token", []any{}, []any{uint64(64000), uint64(1000000), uint64(1)}}
}

// Task 8c fix round 2: the bot reports its RTT through dillad (POST /v1/calls/{call_id}/rtt [ms]); out of range is refused
// before any request.
func TestReportCallRTTPostsTheValue(t *testing.T) {
	callID := testID(51)
	var gotPath, gotMethod string
	var gotBody []byte
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		gotPath, gotMethod = r.URL.Path, r.Method
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/cbor")
		w.WriteHeader(http.StatusNoContent)
	}, 0)
	s, _ := c.Session("call-session")
	if err := s.ReportCallRTT(context.Background(), callID, 63); err != nil {
		t.Fatal(err)
	}
	want, _ := encode([]any{uint32(63)})
	if gotMethod != http.MethodPost || gotPath != "/v1/calls/"+callID.String()+"/rtt" || !bytes.Equal(gotBody, want) {
		t.Fatalf("request = %s %s %x, want POST rtt %x", gotMethod, gotPath, gotBody, want)
	}
	gotPath = ""
	if err := s.ReportCallRTT(context.Background(), callID, MaxCallRTTMS+1); !errors.Is(err, ErrInvalidConfig) || gotPath != "" {
		t.Fatalf("an out-of-range rtt = %v (path %q), want ErrInvalidConfig and no request", err, gotPath)
	}
}
