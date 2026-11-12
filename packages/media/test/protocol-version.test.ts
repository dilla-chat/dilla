import { describe, expect, it } from 'vitest';
import { protocolVersion } from 'livekit-client';

// dillad's /rtc gate admits a closed list of LiveKit signalling protocols, ClientProtocols in
// internal/sfu/rtcjoin.go (branch review SFU-1), because LiveKit labels metric series with the
// number a client sends: ClientProtocol (17) is the web client's, RustClientProtocol (18) the GPUI
// desktop's (pinned by clients/desktop/dilla-media/tests/protocol.rs). livekit-client exports the
// number it sends as `protocolVersion`
// (src/version.ts). A bump of livekit-client that changes it makes every call 400 at /rtc until
// ClientProtocol moves with it: change both together, and TestTheGoSDKSpeaksTheAdmittedProtocol
// pins the Go SDK's side.
describe('the LiveKit signalling protocol', () => {
  it('is the one the /rtc gate admits (internal/sfu/rtcjoin.go ClientProtocol)', () => {
    expect(protocolVersion).toBe(17);
  });
});
