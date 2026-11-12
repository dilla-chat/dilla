package sfu

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/proto"
)

// ErrBadJoinRequest is a join_request LiveKit itself refuses with 400 before any session starts: not
// base64url, not a WrappedJoinRequest, a JoinRequest that does not decode, or one larger than
// http.DefaultMaxHeaderBytes (raw, or once decompressed).
var ErrBadJoinRequest = errors.New("sfu: malformed join_request")

// ErrClientProtocol is a join whose client info names a signalling protocol or an SDK no dilla client
// sends. LiveKit labels three of its histograms with the protocol number and one with the SDK enum,
// both as the client wrote them, so an unbounded value would mint a new metric series per join
// (branch review SFU-1).
var ErrClientProtocol = errors.New("sfu: client protocol not supported")

// ClientProtocol is the LiveKit signalling protocol the web client and the Go SDK speak:
// livekit-client 2.22.3 (pinned in packages/media/package.json) sends protocolVersion = 17
// (src/version.ts:4, as `protocol` on the v0 query, SignalClient.ts:1375, and as
// client_info.protocol in the v1 join_request, room/utils.ts:483), and server-sdk-go
// v2.18.2-0.20260922130803-2088dabd3442 (go.mod) sends PROTOCOL = 17 (protocolversion.go:17,
// signalling.go:58 and signallingjoinrequest.go:63). It equals livekit-server v1.13.7's
// types.CurrentProtocol. TestTheGoSDKSpeaksTheAdmittedProtocol fails first for the Go SDK, and
// packages/media/test/protocol-version.test.ts for livekit-client: move them together.
const ClientProtocol = 17

// RustClientProtocol is the signalling protocol the GPUI desktop speaks: its `livekit =0.10.1` crate
// (pinned in clients/desktop/dilla-media/Cargo.toml) connects through livekit-signaling 0.1.9, whose
// PROTOCOL_VERSION = 18 (src/lib.rs:63) is sent as `protocol` on the v0 query and as
// client_info.protocol; Room::connect does not let the caller lower it. dilla-media's
// tests/protocol.rs fails if that constant moves: move it, this constant and the test together.
const RustClientProtocol = 18

// ClientProtocols is every signalling protocol the /rtc gate admits: the short, closed list of what
// dilla's clients send, so LiveKit's protocol-labelled session histograms keep a bounded label set
// (branch review SFU-1). A client upgrade that raises its protocol is refused at /rtc until this
// list follows.
var ClientProtocols = [...]int32{ClientProtocol, RustClientProtocol}

// Join is what the /rtc gate reads from a signalling request's query before it reaches LiveKit.
type Join struct {
	// Resume is whether LiveKit will treat the request as a resume of a participant it holds.
	Resume bool
	// Protocol is the signalling protocol LiveKit records in its session metrics: 0 for a v0 query
	// with no parsable `protocol` or a v1 request with no client_info.protocol, as LiveKit reads them.
	Protocol int32
	// SDK is the v1 client_info.sdk as the client wrote it; on v0 LiveKit maps the `sdk` parameter
	// onto a closed set itself (utils.go:188-216), so it is UNKNOWN here.
	SDK livekit.ClientInfo_SDK
}

// ReadJoin reads a signalling request's query exactly as livekit-server v1.13.7 does
// (pkg/service/rtcservice.go:155-160, 175-213, 238-240, 269-289, utils.go:182-183):
//
//   - without join_request (the v0 form) a resume is reconnect "1" or "true" (utils.go boolValue) and
//     the protocol is the `protocol` parameter, parsed as a 32-bit decimal;
//   - with join_request (the v1 form, which /rtc accepts too) both come from the JoinRequest, inside a
//     base64url WrappedJoinRequest whose payload is uncompressed or gzip — with LiveKit's bounds: an
//     uncompressed payload over http.DefaultMaxHeaderBytes, or a gzip payload that decompresses past
//     it (service.DecompressGzip, the function LiveKit calls), is refused. A compression value
//     LiveKit does not know leaves the JoinRequest empty, a fresh join with no client info, as
//     LiveKit does.
//
// Anything LiveKit would refuse is ErrBadJoinRequest. Both sides read the first value of the same
// url.ParseQuery of the URL (the proxy admits only GET, so FormValue reads no body).
func ReadJoin(q url.Values) (Join, error) {
	wrapped := q.Get("join_request")
	if wrapped == "" {
		v := q.Get("reconnect")
		j := Join{Resume: v == "1" || v == "true"}
		if pv, err := strconv.ParseInt(q.Get("protocol"), 10, 32); err == nil {
			j.Protocol = int32(pv)
		}
		return j, nil
	}
	raw, err := base64.URLEncoding.DecodeString(wrapped)
	if err != nil {
		return Join{}, fmt.Errorf("%w: cannot base64 decode wrapped join request", ErrBadJoinRequest)
	}
	w := &livekit.WrappedJoinRequest{}
	if err := proto.Unmarshal(raw, w); err != nil {
		return Join{}, fmt.Errorf("%w: cannot unmarshal wrapped join request", ErrBadJoinRequest)
	}
	jr := &livekit.JoinRequest{}
	switch w.GetCompression() {
	case livekit.WrappedJoinRequest_NONE:
		if len(w.GetJoinRequest()) > http.DefaultMaxHeaderBytes {
			return Join{}, fmt.Errorf("%w: join request too large", ErrBadJoinRequest)
		}
		if err := proto.Unmarshal(w.GetJoinRequest(), jr); err != nil {
			return Join{}, fmt.Errorf("%w: cannot unmarshal join request", ErrBadJoinRequest)
		}
	case livekit.WrappedJoinRequest_GZIP:
		plain, err := service.DecompressGzip(w.GetJoinRequest())
		if err != nil {
			return Join{}, fmt.Errorf("%w: %w", ErrBadJoinRequest, err)
		}
		if err := proto.Unmarshal(plain, jr); err != nil {
			return Join{}, fmt.Errorf("%w: cannot unmarshal join request", ErrBadJoinRequest)
		}
	}
	ci := jr.GetClientInfo()
	return Join{Resume: jr.GetReconnect(), Protocol: ci.GetProtocol(), SDK: ci.GetSdk()}, nil
}

// JoinIsResume answers ReadJoin's Resume.
func JoinIsResume(q url.Values) (bool, error) {
	j, err := ReadJoin(q)
	return j.Resume, err
}

// CheckClient refuses a join whose protocol is not one of ClientProtocols, or whose v1 SDK is not one
// LiveKit's own enum names (the desktop's RUST among them). Every value LiveKit then labels a metric
// with is from a closed set.
func (j Join) CheckClient() error {
	if !slices.Contains(ClientProtocols[:], j.Protocol) {
		return fmt.Errorf("%w: protocol must be one of %v", ErrClientProtocol, ClientProtocols)
	}
	if _, ok := livekit.ClientInfo_SDK_name[int32(j.SDK)]; !ok {
		return fmt.Errorf("%w: unknown sdk %d", ErrClientProtocol, int32(j.SDK))
	}
	return nil
}
