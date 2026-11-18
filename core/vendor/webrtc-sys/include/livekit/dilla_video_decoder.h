/*
 * DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
 *
 * A standalone video decoder for the desktop's inline attachment player (parity plan Package 12): libwebrtc's own
 * software decoders (libvpx VP8/VP9, dav1d AV1, the bundled FFmpeg's H.264 on Linux and Windows), fed whole access
 * units by Rust and answering BGRA pictures. VideoToolbox (macOS H.264/H.265, through LiveKit's VideoDecoderFactory)
 * only with the crate feature `dilla-video-toolbox`, off by default. No
 * PeerConnection, no network, no frame transformer: it decodes bytes the app already holds.
 */

#pragma once

#include <atomic>
#include <condition_variable>
#include <cstdint>
#include <deque>
#include <memory>
#include <mutex>
#include <optional>
#include <vector>

#include "api/environment/environment.h"
#include "api/video_codecs/video_decoder.h"
#include "rust/cxx.h"

namespace livekit_ffi {

struct DillaFrameInfo;

class DillaVideoDecoder : public webrtc::DecodedImageCallback {
 public:
  DillaVideoDecoder(const webrtc::Environment& env,
                    std::unique_ptr<webrtc::VideoDecoder> decoder,
                    uint32_t out_width,
                    uint32_t out_height,
                    bool asynchronous);
  ~DillaVideoDecoder() override;

  // Feeds one access unit (Annex B for H.264). Answers libwebrtc's code: 0 is a picture or none yet, < 0 an error.
  // An asynchronous decoder (VideoToolbox) answers its picture on its own thread after Decode returns; for one, this
  // waits (at most kAsyncWait) until that picture is in, so the pictures of the last units are not left in flight
  // when the caller stops feeding (the end of the file: nothing is fed after it to collect them).
  int32_t decode(rust::Slice<const uint8_t> data, uint32_t timestamp, bool key_frame);
  // Whether the decoder hands its pictures out in presentation order under the timestamp of the access unit fed
  // last (FFmpeg reorders internally), rather than under each picture's own access unit (libvpx, dav1d, VideoToolbox).
  bool reorders() const;
  // The oldest decoded picture's size and timestamp, if there is one.
  bool peek_frame(DillaFrameInfo& info);
  // Removes the oldest picture, copying its BGRA bytes (B, G, R, A in memory, rows of width * 4) into `bgra`, which
  // must be exactly width * height * 4 bytes as `peek_frame` answered; false (and the picture dropped) otherwise.
  bool take_frame(rust::Slice<uint8_t> bgra);

  int32_t Decoded(webrtc::VideoFrame& frame) override;

 private:
  struct Picture {
    uint32_t width;
    uint32_t height;
    uint32_t timestamp;
    std::vector<uint8_t> bgra;
  };

  // Held for the decoder's life: it may keep pointers into the environment's utilities.
  webrtc::Environment env_;
  std::unique_ptr<webrtc::VideoDecoder> decoder_;
  const uint32_t out_width_;
  const uint32_t out_height_;
  bool reorders_;
  const bool asynchronous_;
  // Set as the decoder is torn down: a late callback keeps nothing.
  std::atomic<bool> closing_{false};
  std::mutex mutex_;
  std::deque<Picture> pictures_;
  // Asynchronous decoders only, under `mutex_`: the timestamp of the unit `decode` is waiting for, and whether the
  // callback carrying that timestamp has come. Only that unit's own answer ends the wait: a late answer for a unit
  // whose wait already timed out, or for a refused one, names another timestamp and is ignored. `decoded_` wakes
  // `decode` when the answer comes.
  std::condition_variable decoded_;
  std::optional<uint32_t> awaited_;
  bool answered_ = false;
};

// A decoder for `codec` ("VP8", "VP9", "AV1", "H264", "H265"), its pictures scaled down to fit `out_width` x
// `out_height` (never up); null when this build has no decoder for it or it does not configure.
std::unique_ptr<DillaVideoDecoder> new_dilla_video_decoder(rust::Str codec, uint32_t out_width, uint32_t out_height);

}  // namespace livekit_ffi
