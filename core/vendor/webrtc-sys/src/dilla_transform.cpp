/*
 * DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
 * See include/livekit/dilla_transform.h.
 */

#include "livekit/dilla_transform.h"

#include <algorithm>
#include <atomic>
#include <cctype>
#include <span>
#include <utility>
#include <vector>

#include "api/make_ref_counted.h"
#include "api/media_stream_interface.h"
#include "api/video/video_frame_metadata.h"
#include "modules/rtp_rtcp/source/rtp_video_header.h"
#include "rtc_base/logging.h"
#include "rtc_base/time_utils.h"
#include "webrtc-sys/src/dilla_transform.rs.h"

namespace livekit_ffi {

class DillaHookHolder {
 public:
  explicit DillaHookHolder(rust::Box<DillaHook> hook) : hook(std::move(hook)) {}
  rust::Box<DillaHook> hook;
};

namespace {

// protocol/05 "Rotation": the per-track hold for an unknown KID (and, here, for a receiver whose
// track Rust has not bound yet).
constexpr int64_t kHoldMaxMs = 2000;
constexpr size_t kHoldMaxFrames = 256;
constexpr size_t kHoldMaxBytes = 8 * 1024 * 1024;

constexpr uint8_t kReplace = 1;
constexpr uint8_t kHold = 2;
constexpr uint8_t kPassUnchanged = 3;

webrtc::Mutex g_hook_mutex;
std::shared_ptr<DillaHookHolder> g_hook;

// Every live core by handle. Weak: a core lives exactly as long as its transformer (and any run
// dilla_drain_all holds a strong reference for).
webrtc::Mutex g_registry_mutex;
std::map<uint64_t, std::weak_ptr<DillaTransformCore>> g_registry;
std::atomic<uint64_t> g_next_handle{1};

std::shared_ptr<DillaHookHolder> current_hook() {
  webrtc::MutexLock lock(&g_hook_mutex);
  return g_hook;
}

std::vector<std::shared_ptr<DillaTransformCore>> live_cores() {
  std::vector<std::shared_ptr<DillaTransformCore>> out;
  webrtc::MutexLock lock(&g_registry_mutex);
  for (auto& [handle, weak] : g_registry) {
    if (auto core = weak.lock()) {
      out.push_back(std::move(core));
    }
  }
  return out;
}

std::shared_ptr<DillaTransformCore> core_of(uint64_t handle) {
  webrtc::MutexLock lock(&g_registry_mutex);
  auto it = g_registry.find(handle);
  return it == g_registry.end() ? nullptr : it->second.lock();
}

bool is_opus(const std::string& mime) {
  std::string lower(mime);
  std::transform(lower.begin(), lower.end(), lower.begin(),
                 [](unsigned char c) { return std::tolower(c); });
  return lower == "audio/opus";
}

// Creates a core, registers it, tells Rust its handle, and wraps it in a transformer.
webrtc::scoped_refptr<DillaFrameTransformer> make_transformer(
    std::shared_ptr<DillaHookHolder> hook,
    bool send,
    bool video,
    std::string track_id,
    webrtc::scoped_refptr<webrtc::MediaStreamTrackInterface> track,
    const void* owner,
    webrtc::scoped_refptr<webrtc::VideoTrackSourceInterface> key_source) {
  uint64_t handle = g_next_handle.fetch_add(1);
  auto core = std::make_shared<DillaTransformCore>(
      hook, handle, send, video, std::move(track_id), std::move(track), owner,
      std::move(key_source));
  {
    webrtc::MutexLock lock(&g_registry_mutex);
    g_registry[handle] = core;
  }
  dilla_transformer_attached(
      *hook->hook, handle, send, video,
      rust::Str(core->track_id.data(), core->track_id.size()));
  return webrtc::make_ref_counted<DillaFrameTransformer>(std::move(core));
}

}  // namespace

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
  auto hook = current_hook();
  if (!hook || !sender) {
    return;
  }
  auto track = sender->track();
  bool video = sender->media_type() == webrtc::MediaType::VIDEO;
  // A sender without a track gets a transformer bound to no track: Rust drops every frame of it.
  std::string track_id = track ? track->id() : std::string();
  sender->SetEncoderToPacketizerFrameTransformer(make_transformer(
      hook, true, video, std::move(track_id), track, sender.get(), nullptr));
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
  webrtc::scoped_refptr<webrtc::VideoTrackSourceInterface> key_source;
  if (video && track && track->kind() == webrtc::MediaStreamTrackInterface::kVideoKind) {
    key_source = static_cast<webrtc::VideoTrackInterface*>(track.get())->GetSource();
  }
  receiver->SetDepacketizerToDecoderFrameTransformer(make_transformer(
      hook, false, video, std::move(track_id), track, receiver.get(),
      std::move(key_source)));
}

bool dilla_sender_accepts_track(
    const webrtc::scoped_refptr<webrtc::RtpSenderInterface>& sender,
    const std::string& track_id) {
  if (!sender) {
    return false;
  }
  std::shared_ptr<DillaTransformCore> newest;
  for (auto& core : live_cores()) {
    if (core->send && core->owner == sender.get() &&
        (!newest || core->handle > newest->handle)) {
      newest = core;
    }
  }
  return newest && !track_id.empty() && newest->track_id == track_id;
}

void dilla_set_sif_trailer(const std::vector<uint8_t>& trailer) {
  auto hook = current_hook();
  if (!hook) {
    return;
  }
  dilla_sif_trailer(*hook->hook,
                    rust::Slice<const uint8_t>(trailer.data(), trailer.size()));
}

namespace {
uint64_t handle_for(const MediaStreamTrack& track, bool send) {
  auto rtc = track.rtc_track();
  if (!rtc) {
    return 0;
  }
  uint64_t best = 0;
  for (auto& core : live_cores()) {
    if (core->send == send && core->track.get() == rtc.get()) {
      best = std::max(best, core->handle);
    }
  }
  return best;
}
}  // namespace

uint64_t dilla_receiver_handle(const MediaStreamTrack& track) {
  return handle_for(track, false);
}

uint64_t dilla_sender_handle(const MediaStreamTrack& track) {
  return handle_for(track, true);
}

void dilla_drain_all() {
  for (auto& core : live_cores()) {
    core->Run(nullptr);
  }
}

void dilla_request_key_frame(uint64_t handle) {
  auto core = core_of(handle);
  if (!core || core->send || !core->key_source) {
    return;
  }
  // VideoRtpTrackSource::GenerateKeyFrame -> VideoRtpReceiver::OnGenerateKeyFrame ->
  // RequestRecvKeyFrame(ssrc): a PLI to the publisher, through the SFU.
  core->key_source->GenerateKeyFrame();
}

DillaTransformCore::DillaTransformCore(
    std::shared_ptr<DillaHookHolder> hook,
    uint64_t handle,
    bool send,
    bool video,
    std::string track_id,
    webrtc::scoped_refptr<webrtc::MediaStreamTrackInterface> track,
    const void* owner,
    webrtc::scoped_refptr<webrtc::VideoTrackSourceInterface> key_source)
    : handle(handle),
      send(send),
      video(video),
      track_id(std::move(track_id)),
      track(std::move(track)),
      owner(owner),
      key_source(std::move(key_source)),
      hook_(std::move(hook)) {}

DillaTransformCore::~DillaTransformCore() {
  {
    webrtc::MutexLock lock(&g_registry_mutex);
    auto it = g_registry.find(handle);
    // Only this core's own entry (the weak pointer is already expired here).
    if (it != g_registry.end() && it->second.expired()) {
      g_registry.erase(it);
    }
  }
  dilla_transformer_detached(*hook_->hook, handle);
}

uint8_t DillaTransformCore::Decide(webrtc::TransformableFrameInterface& frame,
                                   rust::Vec<uint8_t>& out) {
  auto data = frame.GetData();
  bool key_frame = false;
  int32_t spatial_index = -1;
  // The transformer's kind is its sender's or receiver's media type, so the downcast matches the
  // frame libwebrtc hands it (TransformableVideoSenderFrame / TransformableVideoReceiverFrame).
  if (video) {
    auto& v = static_cast<webrtc::TransformableVideoFrameInterface&>(frame);
    key_frame = v.IsKeyFrame();
    spatial_index = v.Metadata().GetSpatialIndex();
  }
  std::string mime = frame.GetMimeType();
  return dilla_transform_frame(
      *hook_->hook, handle, send, video,
      rust::Str(track_id.data(), track_id.size()),
      rust::Str(mime.data(), mime.size()), frame.GetSsrc(), spatial_index,
      key_frame, rust::Slice<const uint8_t>(data.data(), data.size()), out);
}

void DillaTransformCore::Run(
    std::unique_ptr<webrtc::TransformableFrameInterface> frame) {
  webrtc::MutexLock run(&run_mutex_);
  std::vector<std::unique_ptr<webrtc::TransformableFrameInterface>> ready;
  {
    webrtc::MutexLock lock(&queue_mutex_);
    int64_t now = webrtc::TimeMillis();
    if (frame) {
      size_t bytes = frame->GetData().size();
      held_.push_back(Held{std::move(frame), now, bytes});
      held_bytes_ += bytes;
    }
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
        dilla_hold_expired(*hook_->hook, handle, send);
      } else if (verdict == kReplace) {
        front.frame->SetData(std::span<const uint8_t>(out.data(), out.size()));
        ready.push_back(std::move(front.frame));
      } else if (verdict == kPassUnchanged) {
        // Enforced here, whatever Rust answered: only a zero-byte Opus frame on an audio
        // transformer carries no content (protocol/05 "No plaintext path"). Anything else is a drop.
        if (!video && front.frame->GetData().empty() &&
            is_opus(front.frame->GetMimeType())) {
          ready.push_back(std::move(front.frame));
        } else {
          RTC_LOG(LS_WARNING) << "dilla: PassUnchanged refused for a non-empty or non-Opus frame";
          dilla_pass_refused(*hook_->hook, handle, send);
        }
      }
      // kDrop, an expired hold, a refused pass and anything unknown: the frame is discarded here.
      held_bytes_ -= front.bytes;
      held_.pop_front();
    }
  }
  // Still under run_mutex_: a concurrent run (a new frame and dilla_drain_all) cannot reorder.
  for (auto& f : ready) {
    Emit(std::move(f));
  }
}

size_t DillaTransformCore::HeldCount() {
  webrtc::MutexLock lock(&queue_mutex_);
  return held_.size();
}

void DillaTransformCore::Emit(
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

void DillaTransformCore::SetCallback(
    webrtc::scoped_refptr<webrtc::TransformedFrameCallback> callback) {
  webrtc::MutexLock lock(&callback_mutex_);
  callback_ = callback;
}

void DillaTransformCore::SetSinkCallback(
    webrtc::scoped_refptr<webrtc::TransformedFrameCallback> callback,
    uint32_t ssrc) {
  webrtc::MutexLock lock(&callback_mutex_);
  sink_callbacks_[ssrc] = callback;
}

void DillaTransformCore::ClearCallback() {
  webrtc::MutexLock lock(&callback_mutex_);
  callback_ = nullptr;
}

void DillaTransformCore::ClearSinkCallback(uint32_t ssrc) {
  webrtc::MutexLock lock(&callback_mutex_);
  sink_callbacks_.erase(ssrc);
}

DillaFrameTransformer::DillaFrameTransformer(
    std::shared_ptr<DillaTransformCore> core)
    : core_(std::move(core)) {}

DillaFrameTransformer::~DillaFrameTransformer() = default;

void DillaFrameTransformer::Transform(
    std::unique_ptr<webrtc::TransformableFrameInterface> frame) {
  core_->Run(std::move(frame));
}

void DillaFrameTransformer::RegisterTransformedFrameCallback(
    webrtc::scoped_refptr<webrtc::TransformedFrameCallback> callback) {
  core_->SetCallback(std::move(callback));
}

void DillaFrameTransformer::RegisterTransformedFrameSinkCallback(
    webrtc::scoped_refptr<webrtc::TransformedFrameCallback> callback,
    uint32_t ssrc) {
  core_->SetSinkCallback(std::move(callback), ssrc);
}

void DillaFrameTransformer::UnregisterTransformedFrameCallback() {
  core_->ClearCallback();
}

void DillaFrameTransformer::UnregisterTransformedFrameSinkCallback(
    uint32_t ssrc) {
  core_->ClearSinkCallback(ssrc);
}

}  // namespace livekit_ffi

// ---------------------------------------------------------------------------------------------
// Test support: synthetic frames through a real DillaFrameTransformer, with no PeerConnection.
// libwebrtc lets only its listed frame classes construct a TransformableFrameInterface (the
// Passkey); its own test mocks are among them, so the synthetic frames take those names. The
// prebuilt libwebrtc defines none of them.
// ---------------------------------------------------------------------------------------------

namespace webrtc {

class MockTransformableAudioFrame : public TransformableAudioFrameInterface {
 public:
  MockTransformableAudioFrame(std::string mime, std::vector<uint8_t> data)
      : TransformableAudioFrameInterface(Passkey()),
        mime_(std::move(mime)),
        data_(std::move(data)) {}
  std::span<const uint8_t> GetData() const override { return data_; }
  void SetData(std::span<const uint8_t> data) override {
    data_.assign(data.begin(), data.end());
  }
  uint8_t GetPayloadType() const override { return 111; }
  uint32_t GetSsrc() const override { return 1234; }
  uint32_t GetTimestamp() const override { return 0; }
  void SetRTPTimestamp(uint32_t) override {}
  std::string GetMimeType() const override { return mime_; }
  std::optional<Timestamp> ReceiveTime() const override { return std::nullopt; }
  std::optional<Timestamp> CaptureTime() const override { return std::nullopt; }
  std::optional<TimeDelta> SenderCaptureTimeOffset() const override {
    return std::nullopt;
  }
  std::span<const uint32_t> GetContributingSources() const override { return {}; }
  const std::optional<uint16_t> SequenceNumber() const override {
    return std::nullopt;
  }
  std::optional<uint64_t> AbsoluteCaptureTimestamp() const override {
    return std::nullopt;
  }
  std::optional<uint8_t> AudioLevel() const override { return std::nullopt; }

 private:
  std::string mime_;
  std::vector<uint8_t> data_;
};

class MockTransformableVideoFrame : public TransformableVideoFrameInterface {
 public:
  MockTransformableVideoFrame(std::string mime, std::vector<uint8_t> data, bool key)
      : TransformableVideoFrameInterface(Passkey()),
        mime_(std::move(mime)),
        data_(std::move(data)),
        key_(key) {}
  std::span<const uint8_t> GetData() const override { return data_; }
  void SetData(std::span<const uint8_t> data) override {
    data_.assign(data.begin(), data.end());
  }
  uint8_t GetPayloadType() const override { return 96; }
  uint32_t GetSsrc() const override { return 5678; }
  uint32_t GetTimestamp() const override { return 0; }
  void SetRTPTimestamp(uint32_t) override {}
  std::string GetMimeType() const override { return mime_; }
  std::optional<Timestamp> ReceiveTime() const override { return std::nullopt; }
  std::optional<Timestamp> CaptureTime() const override { return std::nullopt; }
  std::optional<TimeDelta> SenderCaptureTimeOffset() const override {
    return std::nullopt;
  }
  bool IsKeyFrame() const override { return key_; }
  VideoFrameMetadata Metadata() const override { return VideoFrameMetadata(); }
  void SetMetadata(const VideoFrameMetadata&) override {}
  const RTPVideoHeader& header() const override { return header_; }

 private:
  std::string mime_;
  std::vector<uint8_t> data_;
  bool key_;
  RTPVideoHeader header_;
};

}  // namespace webrtc

namespace livekit_ffi {

namespace {

class RecordingCallback : public webrtc::TransformedFrameCallback {
 public:
  void OnTransformedFrame(
      std::unique_ptr<webrtc::TransformableFrameInterface> frame) override {
    webrtc::MutexLock lock(&mutex_);
    auto data = frame->GetData();
    emitted_.emplace_back(data.begin(), data.end());
  }
  std::deque<std::vector<uint8_t>> Take() {
    webrtc::MutexLock lock(&mutex_);
    return std::exchange(emitted_, {});
  }
  size_t Count() {
    webrtc::MutexLock lock(&mutex_);
    return emitted_.size();
  }

 private:
  webrtc::Mutex mutex_;
  std::deque<std::vector<uint8_t>> emitted_;
};

struct TestTransformer {
  webrtc::scoped_refptr<DillaFrameTransformer> transformer;
  webrtc::scoped_refptr<RecordingCallback> callback;
  bool video;
};

webrtc::Mutex g_test_mutex;
std::map<uint64_t, TestTransformer> g_tests;

bool test_of(uint64_t handle, TestTransformer& out) {
  webrtc::MutexLock lock(&g_test_mutex);
  auto it = g_tests.find(handle);
  if (it == g_tests.end()) {
    return false;
  }
  out = it->second;
  return true;
}

}  // namespace

uint64_t dilla_test_attach(bool send, bool video, rust::Str track_id) {
  auto hook = current_hook();
  if (!hook) {
    return 0;
  }
  auto transformer = make_transformer(hook, send, video, std::string(track_id), nullptr,
                                      nullptr, nullptr);
  auto callback = webrtc::make_ref_counted<RecordingCallback>();
  transformer->RegisterTransformedFrameCallback(callback);
  uint64_t handle = transformer->handle();
  webrtc::MutexLock lock(&g_test_mutex);
  g_tests[handle] = TestTransformer{transformer, callback, video};
  return handle;
}

void dilla_test_frame(uint64_t handle, rust::Str mime, rust::Slice<const uint8_t> data,
                      bool key_frame) {
  TestTransformer t;
  if (!test_of(handle, t)) {
    return;
  }
  std::vector<uint8_t> bytes(data.begin(), data.end());
  std::unique_ptr<webrtc::TransformableFrameInterface> frame;
  if (t.video) {
    frame = std::make_unique<webrtc::MockTransformableVideoFrame>(std::string(mime),
                                                                   std::move(bytes), key_frame);
  } else {
    frame = std::make_unique<webrtc::MockTransformableAudioFrame>(std::string(mime),
                                                                   std::move(bytes));
  }
  t.transformer->Transform(std::move(frame));
}

rust::Vec<uint8_t> dilla_test_take_emitted(uint64_t handle) {
  // Every emitted frame, each as a big-endian u32 length followed by its bytes.
  rust::Vec<uint8_t> out;
  TestTransformer t;
  if (!test_of(handle, t)) {
    return out;
  }
  for (auto& f : t.callback->Take()) {
    uint32_t n = static_cast<uint32_t>(f.size());
    for (int s = 24; s >= 0; s -= 8) {
      out.push_back(static_cast<uint8_t>(n >> s));
    }
    for (auto b : f) {
      out.push_back(b);
    }
  }
  return out;
}

size_t dilla_test_emitted_count(uint64_t handle) {
  TestTransformer t;
  return test_of(handle, t) ? t.callback->Count() : 0;
}

size_t dilla_test_held(uint64_t handle) {
  auto core = core_of(handle);
  return core ? core->HeldCount() : 0;
}

void dilla_test_release(uint64_t handle) {
  TestTransformer t;
  {
    webrtc::MutexLock lock(&g_test_mutex);
    auto it = g_tests.find(handle);
    if (it == g_tests.end()) {
      return;
    }
    t = std::move(it->second);
    g_tests.erase(it);
  }
  // `t` goes out of scope here, outside g_test_mutex: the transformer and its core die, and Rust
  // hears `detached`.
}

}  // namespace livekit_ffi
