// DILLA PATCH: Dilla's own macOS share picker (core/vendor/README.md; GPUI gaps Task 45, D22):
// ScreenCaptureKit's displays and windows listed with their owners, thumbnails, and an own SCStream
// whose BGRA frames go to a Rust sink (src/dilla_screen_mac.mm).

#pragma once
#include <cstddef>
#include <cstdint>
#include <memory>
#include <mutex>

#include "rust/cxx.h"

namespace livekit_ffi {
class DillaScreenCapture;
}  // namespace livekit_ffi

#include "webrtc-sys/src/dilla_screen.rs.h"

namespace livekit_ffi {

// One running SCStream of a display or a window. Its frames go to the Rust sink until it is
// destroyed: the destructor stops the stream, removes its output and drains the output queue first,
// so no frame arrives after it returns. Never destroyed on that queue (the sink does not own it).
class DillaScreenCapture {
 public:
  struct Mac;  // The Objective-C side, defined in the .mm.

  DillaScreenCapture(std::unique_ptr<Mac> mac, rust::Box<DillaScreenSink> sink, uint32_t width,
                     uint32_t height);
  ~DillaScreenCapture();

  // Called on the output queue with each complete frame (its pixel buffer locked for the call).
  void Deliver(uint32_t width, uint32_t height, uint32_t stride, const uint8_t* bgra, size_t len);
  // The stream stopped by itself; the sink hears it once.
  void End();

  void set_fps(uint32_t fps) const;
  uint32_t width() const { return width_; }
  uint32_t height() const { return height_; }

  Mac& mac() { return *mac_; }

 private:
  std::unique_ptr<Mac> mac_;
  std::mutex lock_;
  rust::Box<DillaScreenSink> sink_;
  bool ended_ = false;
  uint32_t width_ = 0;
  uint32_t height_ = 0;
};

bool dilla_screen_access(bool request);
DillaScreenList dilla_screen_list();
rust::Vec<DillaScreenThumbnail> dilla_screen_thumbnails(rust::Slice<const uint64_t> displays,
                                                        rust::Slice<const uint64_t> windows,
                                                        uint32_t max_w, uint32_t max_h);
std::unique_ptr<DillaScreenCapture> dilla_screen_open(bool is_window, uint64_t id,
                                                      int32_t expect_pid, uint32_t fps, bool cursor,
                                                      rust::Box<DillaScreenSink> sink);

}  // namespace livekit_ffi
