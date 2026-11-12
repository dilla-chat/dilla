/*
 * DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
 *
 * Test support for the dilla-sframe/1 hook (webrtc_sys::dilla_transform_testing): a transformer with
 * no PeerConnection, fed synthetic frames, whose output is recorded. Built only with the
 * `dilla-test-frames` feature; never part of a shipping binary.
 */

#pragma once

#include <stdint.h>

#include <string>

#include "livekit/dilla_transform.h"
#include "rust/cxx.h"

#ifndef DILLA_TEST_FRAMES
#error "dilla_transform_testing is built only with the dilla-test-frames feature"
#endif

namespace livekit_ffi {

// Defined in dilla_transform.cpp under DILLA_TEST_FRAMES.
webrtc::scoped_refptr<DillaFrameTransformer> dilla_test_new_transformer(bool send,
                                                                       bool video,
                                                                       std::string track_id);
size_t dilla_test_held_frames(uint64_t handle);

uint64_t dilla_test_attach(bool send, bool video, rust::Str track_id);
void dilla_test_frame(uint64_t handle, rust::Str mime, rust::Slice<const uint8_t> data,
                      bool key_frame);
rust::Vec<uint8_t> dilla_test_take_emitted(uint64_t handle);
size_t dilla_test_emitted_count(uint64_t handle);
size_t dilla_test_held(uint64_t handle);
void dilla_test_release(uint64_t handle);

}  // namespace livekit_ffi
