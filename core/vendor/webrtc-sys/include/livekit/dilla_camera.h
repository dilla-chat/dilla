// DILLA PATCH: the camera source of the desktop client (core/vendor/README.md): libwebrtc's own
// VideoCaptureModule (V4L2 on Linux, DirectShow on Windows) and AVFoundation on macOS (the prebuilt
// libwebrtc has no macOS VideoCaptureModule), handing I420 frames to Rust.

#pragma once
#include <memory>
#include <mutex>

#include "api/scoped_refptr.h"
#include "api/video/video_frame.h"
#include "api/video/video_frame_buffer.h"
#include "api/video/video_sink_interface.h"
#include "rust/cxx.h"
#if !defined(__APPLE__)
#include "modules/video_capture/video_capture.h"
#endif

namespace livekit_ffi {
class DillaCamera;
struct DillaCameraDevice;
}  // namespace livekit_ffi

#include "webrtc-sys/src/dilla_camera.rs.h"

namespace livekit_ffi {

#if defined(__APPLE__)

// One open camera (AVFoundation, src/dilla_camera_mac.mm). Its frames go to the Rust sink until it
// is destroyed: the destructor stops the session, detaches the frame delegate and drains its
// capture queue first, so no frame arrives after it returns.
class DillaCamera {
 public:
  struct Mac;  // The Objective-C side, defined in the .mm.

  DillaCamera(std::unique_ptr<Mac> mac, rust::Box<DillaCameraSink> sink);
  ~DillaCamera();

  // Called on the capture queue with each converted frame.
  void Deliver(const webrtc::I420BufferInterface& i420);
  int32_t start(uint32_t width, uint32_t height, uint32_t fps) const;
  uint32_t width() const { return width_; }
  uint32_t height() const { return height_; }

 private:
  std::unique_ptr<Mac> mac_;
  std::mutex lock_;
  rust::Box<DillaCameraSink> sink_;
  mutable uint32_t width_ = 0;
  mutable uint32_t height_ = 0;
};

#else

// One open camera. Its frames go to the Rust sink until it is destroyed: the destructor stops the
// capture and deregisters the sink first, so no frame arrives after it returns.
class DillaCamera : public webrtc::VideoSinkInterface<webrtc::VideoFrame> {
 public:
  DillaCamera(webrtc::scoped_refptr<webrtc::VideoCaptureModule> module,
              rust::Box<DillaCameraSink> sink);
  ~DillaCamera() override;

  void OnFrame(const webrtc::VideoFrame& frame) override;
  int32_t start(uint32_t width, uint32_t height, uint32_t fps) const;
  uint32_t width() const { return width_; }
  uint32_t height() const { return height_; }

 private:
  webrtc::scoped_refptr<webrtc::VideoCaptureModule> module_;
  std::mutex lock_;
  rust::Box<DillaCameraSink> sink_;
  mutable uint32_t width_ = 0;
  mutable uint32_t height_ = 0;
};

#endif

// Hands one I420 frame's planes to the sink (borrowed for the call only); every platform's frame
// path ends here (src/dilla_camera.cpp).
void dilla_camera_deliver(DillaCameraSink& sink, const webrtc::I420BufferInterface& i420);

rust::Vec<DillaCameraDevice> dilla_camera_devices();
std::unique_ptr<DillaCamera> dilla_camera_open(rust::Str unique_id,
                                               rust::Box<DillaCameraSink> sink);

}  // namespace livekit_ffi
