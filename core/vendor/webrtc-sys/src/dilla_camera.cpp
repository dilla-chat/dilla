// DILLA PATCH: the camera source of the desktop client (core/vendor/README.md). The frame hand-off
// is shared; the VideoCaptureModule backend below is Linux and Windows only (macOS:
// src/dilla_camera_mac.mm).

#include "livekit/dilla_camera.h"

#include <string>
#include <vector>

#include "api/video/i420_buffer.h"
#if !defined(__APPLE__)
#include "modules/video_capture/video_capture_factory.h"
#endif

namespace livekit_ffi {

void dilla_camera_deliver(DillaCameraSink& sink, const webrtc::I420BufferInterface& i420) {
  const int w = i420.width();
  const int h = i420.height();
  const int ch = (h + 1) / 2;
  sink.on_frame(
      static_cast<uint32_t>(w), static_cast<uint32_t>(h),
      rust::Slice<const uint8_t>(i420.DataY(),
                                 static_cast<size_t>(i420.StrideY()) * h),
      static_cast<uint32_t>(i420.StrideY()),
      rust::Slice<const uint8_t>(i420.DataU(),
                                 static_cast<size_t>(i420.StrideU()) * ch),
      static_cast<uint32_t>(i420.StrideU()),
      rust::Slice<const uint8_t>(i420.DataV(),
                                 static_cast<size_t>(i420.StrideV()) * ch),
      static_cast<uint32_t>(i420.StrideV()));
}

#if !defined(__APPLE__)

DillaCamera::DillaCamera(
    webrtc::scoped_refptr<webrtc::VideoCaptureModule> module,
    rust::Box<DillaCameraSink> sink)
    : module_(std::move(module)), sink_(std::move(sink)) {
  module_->RegisterCaptureDataCallback(this);
}

DillaCamera::~DillaCamera() {
  module_->StopCapture();
  module_->DeRegisterCaptureDataCallback();
  // A frame being delivered right now finishes before the sink goes.
  std::lock_guard<std::mutex> guard(lock_);
}

void DillaCamera::OnFrame(const webrtc::VideoFrame& frame) {
  webrtc::scoped_refptr<webrtc::I420BufferInterface> i420 =
      frame.video_frame_buffer()->ToI420();
  if (!i420) {
    return;
  }
  std::lock_guard<std::mutex> guard(lock_);
  dilla_camera_deliver(*sink_, *i420);
}

int32_t DillaCamera::start(uint32_t width, uint32_t height, uint32_t fps) const {
  std::unique_ptr<webrtc::VideoCaptureModule::DeviceInfo> info(
      webrtc::VideoCaptureFactory::CreateDeviceInfo());
  webrtc::VideoCaptureCapability wanted;
  wanted.width = static_cast<int32_t>(width);
  wanted.height = static_cast<int32_t>(height);
  wanted.maxFPS = static_cast<int32_t>(fps);
  wanted.videoType = webrtc::VideoType::kI420;
  webrtc::VideoCaptureCapability chosen = wanted;
  if (info) {
    webrtc::VideoCaptureCapability best;
    if (info->GetBestMatchedCapability(module_->CurrentDeviceName(), wanted,
                                       best) >= 0) {
      chosen = best;
    }
  }
  width_ = static_cast<uint32_t>(chosen.width);
  height_ = static_cast<uint32_t>(chosen.height);
  return module_->StartCapture(chosen);
}

rust::Vec<DillaCameraDevice> dilla_camera_devices() {
  rust::Vec<DillaCameraDevice> out;
  std::unique_ptr<webrtc::VideoCaptureModule::DeviceInfo> info(
      webrtc::VideoCaptureFactory::CreateDeviceInfo());
  if (!info) {
    return out;
  }
  const uint32_t n = info->NumberOfDevices();
  for (uint32_t i = 0; i < n; ++i) {
    char name[256] = {0};
    char id[256] = {0};
    if (info->GetDeviceName(i, name, sizeof(name), id, sizeof(id)) != 0) {
      continue;
    }
    out.push_back(DillaCameraDevice{rust::String(name), rust::String(id)});
  }
  return out;
}

std::unique_ptr<DillaCamera> dilla_camera_open(rust::Str unique_id,
                                               rust::Box<DillaCameraSink> sink) {
  std::string id(unique_id.data(), unique_id.size());
  webrtc::scoped_refptr<webrtc::VideoCaptureModule> module =
      webrtc::VideoCaptureFactory::Create(id.c_str());
  if (!module) {
    return nullptr;
  }
  return std::make_unique<DillaCamera>(std::move(module), std::move(sink));
}

#endif  // !defined(__APPLE__)

}  // namespace livekit_ffi
