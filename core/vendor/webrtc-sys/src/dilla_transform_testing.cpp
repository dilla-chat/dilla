/*
 * DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's. Compiled only with
 * the `dilla-test-frames` feature (build.rs), which only dilla-media's dev-dependency enables.
 */

#include "livekit/dilla_transform_testing.h"

#include <deque>
#include <map>
#include <utility>
#include <vector>

#include "api/make_ref_counted.h"
#include "api/video/video_frame_metadata.h"
#include "modules/rtp_rtcp/source/rtp_video_header.h"
#include "webrtc-sys/src/dilla_transform_testing.rs.h"
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
  auto transformer = dilla_test_new_transformer(send, video, std::string(track_id));
  if (!transformer) {
    return 0;
  }
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
  return dilla_test_held_frames(handle);
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
