/*
 * DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
 * See include/livekit/dilla_transform.h.
 */

#include "livekit/dilla_transform.h"

#include <span>
#include <utility>
#include <vector>

#include "api/make_ref_counted.h"
#include "api/media_stream_interface.h"
#include "rtc_base/logging.h"
#include "rtc_base/time_utils.h"
#include "webrtc-sys/src/dilla_transform.rs.h"

namespace livekit_ffi {

namespace {

// protocol/05 "Rotation": the per-track hold for an unknown KID (and, here, for a receiver whose
// track Rust has not bound yet).
constexpr int64_t kHoldMaxMs = 2000;
constexpr size_t kHoldMaxFrames = 256;
constexpr size_t kHoldMaxBytes = 8 * 1024 * 1024;

constexpr uint8_t kDrop = 0;
constexpr uint8_t kReplace = 1;
constexpr uint8_t kHold = 2;
constexpr uint8_t kPassUnchanged = 3;

webrtc::Mutex g_hook_mutex;
std::shared_ptr<DillaHookHolder> g_hook;

std::shared_ptr<DillaHookHolder> current_hook() {
  webrtc::MutexLock lock(&g_hook_mutex);
  return g_hook;
}

}  // namespace

class DillaHookHolder {
 public:
  explicit DillaHookHolder(rust::Box<DillaHook> hook) : hook(std::move(hook)) {}
  rust::Box<DillaHook> hook;
};

bool dilla_install_hook(rust::Box<DillaHook> hook) {
  webrtc::MutexLock lock(&g_hook_mutex);
  if (g_hook) {
    return false;
  }
  g_hook = std::make_shared<DillaHookHolder>(std::move(hook));
  return true;
}

bool dilla_owns_transforms() {
  return current_hook() != nullptr;
}

void dilla_attach_sender(
    const webrtc::scoped_refptr<webrtc::RtpSenderInterface>& sender) {
  if (!sender) {
    return;
  }
  auto track = sender->track();
  // A sender without a track gets a transformer bound to no track: Rust drops every frame of it.
  dilla_attach_sender_for_track(sender, track ? track->id() : std::string());
}

void dilla_attach_sender_for_track(
    const webrtc::scoped_refptr<webrtc::RtpSenderInterface>& sender,
    const std::string& track_id) {
  auto hook = current_hook();
  if (!hook || !sender) {
    return;
  }
  bool video = sender->media_type() == webrtc::MediaType::VIDEO;
  sender->SetEncoderToPacketizerFrameTransformer(
      webrtc::make_ref_counted<DillaFrameTransformer>(hook, true, video,
                                                      track_id));
}

void dilla_attach_receiver(
    const webrtc::scoped_refptr<webrtc::RtpReceiverInterface>& receiver) {
  auto hook = current_hook();
  if (!hook || !receiver) {
    return;
  }
  auto track = receiver->track();
  std::string track_id = track ? track->id() : std::string();
  bool video = receiver->media_type() == webrtc::MediaType::VIDEO;
  receiver->SetDepacketizerToDecoderFrameTransformer(
      webrtc::make_ref_counted<DillaFrameTransformer>(hook, false, video,
                                                      track_id));
}

DillaFrameTransformer::DillaFrameTransformer(
    std::shared_ptr<DillaHookHolder> hook,
    bool send,
    bool video,
    std::string track_id)
    : hook_(std::move(hook)),
      send_(send),
      video_(video),
      track_id_(std::move(track_id)) {}

DillaFrameTransformer::~DillaFrameTransformer() = default;

uint8_t DillaFrameTransformer::Decide(webrtc::TransformableFrameInterface& frame,
                                      rust::Vec<uint8_t>& out) {
  auto data = frame.GetData();
  bool key_frame = false;
  int32_t spatial_index = -1;
  // The transformer's kind is its sender's or receiver's media type, so the downcast matches the
  // frame libwebrtc hands it (TransformableVideoSenderFrame / TransformableVideoReceiverFrame).
  if (video_) {
    auto& v = static_cast<webrtc::TransformableVideoFrameInterface&>(frame);
    key_frame = v.IsKeyFrame();
    spatial_index = v.Metadata().GetSpatialIndex();
  }
  std::string mime = frame.GetMimeType();
  return dilla_transform_frame(
      *hook_->hook, send_, video_,
      rust::Str(track_id_.data(), track_id_.size()),
      rust::Str(mime.data(), mime.size()), frame.GetSsrc(), spatial_index,
      key_frame, rust::Slice<const uint8_t>(data.data(), data.size()), out);
}

void DillaFrameTransformer::Transform(
    std::unique_ptr<webrtc::TransformableFrameInterface> frame) {
  std::vector<std::unique_ptr<webrtc::TransformableFrameInterface>> ready;
  {
    webrtc::MutexLock lock(&queue_mutex_);
    int64_t now = webrtc::TimeMillis();
    size_t bytes = frame->GetData().size();
    held_.push_back(Held{std::move(frame), now, bytes});
    held_bytes_ += bytes;
    while (!held_.empty()) {
      Held& front = held_.front();
      rust::Vec<uint8_t> out;
      uint8_t verdict = Decide(*front.frame, out);
      if (verdict == kHold) {
        bool over = now - front.at_ms >= kHoldMaxMs ||
                    held_.size() > kHoldMaxFrames || held_bytes_ > kHoldMaxBytes;
        if (!over) {
          break;
        }
        // Past a limit: dropped and counted, the oldest first; never released unauthenticated.
        dilla_hold_expired(*hook_->hook, send_,
                           rust::Str(track_id_.data(), track_id_.size()));
      } else if (verdict == kReplace) {
        front.frame->SetData(std::span<const uint8_t>(out.data(), out.size()));
        ready.push_back(std::move(front.frame));
      } else if (verdict == kPassUnchanged) {
        ready.push_back(std::move(front.frame));
      }
      // kDrop, an expired hold and anything unknown: the frame is discarded here.
      held_bytes_ -= front.bytes;
      held_.pop_front();
    }
  }
  for (auto& f : ready) {
    Emit(std::move(f));
  }
}

void DillaFrameTransformer::Emit(
    std::unique_ptr<webrtc::TransformableFrameInterface> frame) {
  webrtc::scoped_refptr<webrtc::TransformedFrameCallback> cb;
  {
    webrtc::MutexLock lock(&callback_mutex_);
    auto it = sink_callbacks_.find(frame->GetSsrc());
    cb = it != sink_callbacks_.end() ? it->second : callback_;
  }
  if (cb) {
    cb->OnTransformedFrame(std::move(frame));
  }
}

void DillaFrameTransformer::RegisterTransformedFrameCallback(
    webrtc::scoped_refptr<webrtc::TransformedFrameCallback> callback) {
  webrtc::MutexLock lock(&callback_mutex_);
  callback_ = callback;
}

void DillaFrameTransformer::RegisterTransformedFrameSinkCallback(
    webrtc::scoped_refptr<webrtc::TransformedFrameCallback> callback,
    uint32_t ssrc) {
  webrtc::MutexLock lock(&callback_mutex_);
  sink_callbacks_[ssrc] = callback;
}

void DillaFrameTransformer::UnregisterTransformedFrameCallback() {
  webrtc::MutexLock lock(&callback_mutex_);
  callback_ = nullptr;
}

void DillaFrameTransformer::UnregisterTransformedFrameSinkCallback(
    uint32_t ssrc) {
  webrtc::MutexLock lock(&callback_mutex_);
  sink_callbacks_.erase(ssrc);
}

}  // namespace livekit_ffi
