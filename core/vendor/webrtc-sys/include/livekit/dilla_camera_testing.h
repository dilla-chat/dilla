/*
 * DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
 *
 * Test support for the macOS camera (webrtc_sys::dilla_camera_testing): the authorisation gate of
 * src/dilla_camera_mac.mm runs with a forced status and request answer instead of the machine's TCC
 * state. Built only with the `dilla-test-frames` feature on macOS; never part of a shipping binary.
 */

#pragma once

#include <stdint.h>

#ifndef DILLA_TEST_FRAMES
#error "dilla_camera_testing is built only with the dilla-test-frames feature"
#endif

namespace livekit_ffi {

// `status` is an AVAuthorizationStatus (0 not determined, 1 restricted, 2 denied, 3 authorized), or
// -1 for the real one; `grants` answers a request. Resets the counters and the asked-once flag.
// While forced, the id "dilla-test-camera" opens a camera with no device (its start fails).
void dilla_camera_test_force(int32_t status, bool grants);
// Requests made since the last force.
uint64_t dilla_camera_test_requests();
// Opens the gate refused since the last force.
uint64_t dilla_camera_test_refusals();
// DillaCamera::start calls since the last force.
uint64_t dilla_camera_test_starts();

}  // namespace livekit_ffi
