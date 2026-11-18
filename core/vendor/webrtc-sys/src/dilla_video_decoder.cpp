/*
 * DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
 * See include/livekit/dilla_video_decoder.h.
 */

#include "livekit/dilla_video_decoder.h"

#include <algorithm>
#include <chrono>
#include <string>
#include <utility>

#include "absl/strings/match.h"
#include "api/environment/environment_factory.h"
#include "api/video/color_space.h"
#include "api/video/encoded_image.h"
#include "api/video/i420_buffer.h"
#include "api/video/video_frame.h"
#include "api/video_codecs/sdp_video_format.h"
#include "api/video_codecs/video_codec.h"
#include "modules/video_coding/codecs/h264/include/h264.h"
#include "modules/video_coding/codecs/vp8/include/vp8.h"
#include "modules/video_coding/codecs/vp9/include/vp9.h"

#if defined(RTC_DAV1D_IN_INTERNAL_DECODER_FACTORY)
#include "modules/video_coding/codecs/av1/dav1d_decoder.h"  // nogncheck
#endif

#if defined(DILLA_VIDEO_TOOLBOX)
#include "livekit/video_decoder_factory.h"
#endif
#include "third_party/libyuv/include/libyuv/convert_argb.h"
#include "webrtc-sys/src/dilla_video_decoder.rs.h"

namespace livekit_ffi {

namespace {

// Rust drains the pictures after every access unit; a decoder that hands out more than this before it is drained
// (asynchronous VideoToolbox) loses its oldest, never grows without bound.
constexpr size_t kMaxPictures = 8;

// The largest coded picture taken at all (either side), as libwebrtc's own decoders cap it.
constexpr int kMaxSide = 8192;

// How long `decode` waits for an asynchronous decoder's picture of the unit it was just fed: VideoToolbox answers in a
// few milliseconds; a unit it never answers (dropped) costs this once, never stalls the units after it.
constexpr std::chrono::milliseconds kAsyncWait{100};

// Runs `f` when the scope ends, whichever way it ends.
template <typename F>
struct OnExit {
  F f;
  ~OnExit() { f(); }
};
template <typename F>
OnExit(F) -> OnExit<F>;

// `full_range` says the decoder's samples are full range whatever the frame's colour space says (VideoToolbox: see
// `Decoded`).
const libyuv::YuvConstants* matrix_of(const webrtc::VideoFrame& frame, bool full_range) {
  const auto& cs = frame.color_space();
  if (!cs.has_value()) {
    return full_range ? &libyuv::kYuvJPEGConstants : &libyuv::kYuvI601Constants;
  }
  const bool full = full_range || cs->range() == webrtc::ColorSpace::RangeID::kFull;
  switch (cs->matrix()) {
    case webrtc::ColorSpace::MatrixID::kBT709:
      return full ? &libyuv::kYuvF709Constants : &libyuv::kYuvH709Constants;
    case webrtc::ColorSpace::MatrixID::kBT2020_NCL:
    case webrtc::ColorSpace::MatrixID::kBT2020_CL:
      return full ? &libyuv::kYuvV2020Constants : &libyuv::kYuv2020Constants;
    default:
      return full ? &libyuv::kYuvJPEGConstants : &libyuv::kYuvI601Constants;
  }
}

}  // namespace

DillaVideoDecoder::DillaVideoDecoder(const webrtc::Environment& env,
                                     std::unique_ptr<webrtc::VideoDecoder> decoder,
                                     uint32_t out_width,
                                     uint32_t out_height,
                                     bool asynchronous)
    : env_(env),
      decoder_(std::move(decoder)),
      out_width_(std::max<uint32_t>(out_width, 2)),
      out_height_(std::max<uint32_t>(out_height, 2)),
      asynchronous_(asynchronous) {
  reorders_ = absl::StrContains(decoder_->GetDecoderInfo().implementation_name, "FFmpeg");
  decoder_->RegisterDecodeCompleteCallback(this);
}

DillaVideoDecoder::~DillaVideoDecoder() {
  // Release first: an asynchronous decoder (VideoToolbox) waits for its frames in flight there, and any callback they
  // make still finds this object whole; `closing_` makes those callbacks drop their pictures. Only then is the callback
  // taken away and the decoder destroyed (before the environment it may point into, by member order).
  closing_.store(true);
  decoder_->Release();
  decoder_->RegisterDecodeCompleteCallback(nullptr);
  decoder_.reset();
}

int32_t DillaVideoDecoder::decode(rust::Slice<const uint8_t> data, uint32_t timestamp, bool key_frame) {
  if (data.empty()) {
    return -1;
  }
  webrtc::EncodedImage image;
  image.SetEncodedData(webrtc::EncodedImageBuffer::Create(data.data(), data.size()));
  image.SetRtpTimestamp(timestamp);
  image.SetFrameType(key_frame ? webrtc::VideoFrameType::kVideoFrameKey : webrtc::VideoFrameType::kVideoFrameDelta);
  if (!asynchronous_) {
    return decoder_->Decode(image, /*render_time_ms=*/0);
  }
  // VideoToolbox answers on its own thread after Decode returns. Without this wait, the pictures of the last units
  // fed were still in flight when the caller stopped feeding at the end of the file, and were never collected (the
  // decode test saw 15 or 16 of 20 pictures on macOS 27, and failed the same way on macOS 15 CI).
  // The unit is named by its timestamp (the caller feeds each decoder increasing sample indices), and only the callback
  // carrying that timestamp answers it.
  {
    std::lock_guard<std::mutex> lock(mutex_);
    awaited_ = timestamp;
    answered_ = false;
  }
  const int32_t result = decoder_->Decode(image, /*render_time_ms=*/0);
  std::unique_lock<std::mutex> lock(mutex_);
  if (result >= 0) {
    // A unit never answered in time (dropped) costs this wait once; its late answer, if any, matches nothing.
    decoded_.wait_for(lock, kAsyncWait, [&] { return answered_; });
  }
  awaited_.reset();
  return result;
}

bool DillaVideoDecoder::reorders() const {
  return reorders_;
}

bool DillaVideoDecoder::peek_frame(DillaFrameInfo& info) {
  std::lock_guard<std::mutex> lock(mutex_);
  if (pictures_.empty()) {
    return false;
  }
  const Picture& picture = pictures_.front();
  info.width = picture.width;
  info.height = picture.height;
  info.timestamp = picture.timestamp;
  return true;
}

bool DillaVideoDecoder::take_frame(rust::Slice<uint8_t> bgra) {
  Picture picture;
  {
    std::lock_guard<std::mutex> lock(mutex_);
    if (pictures_.empty()) {
      return false;
    }
    picture = std::move(pictures_.front());
    pictures_.pop_front();
  }
  if (bgra.size() != picture.bgra.size()) {
    return false;
  }
  std::copy(picture.bgra.begin(), picture.bgra.end(), bgra.begin());
  return true;
}

int32_t DillaVideoDecoder::Decoded(webrtc::VideoFrame& frame) {
  // The callback answers the unit its timestamp names, kept or not (see `decode`), as it ends: after its picture is in.
  const uint32_t unit = frame.rtp_timestamp();
  OnExit answer{[this, unit] {
    {
      std::lock_guard<std::mutex> lock(mutex_);
      if (!awaited_ || *awaited_ != unit) {
        return;
      }
      answered_ = true;
    }
    decoded_.notify_all();
  }};
  if (closing_.load()) {
    return 0;
  }
  auto buffer = frame.video_frame_buffer();
  if (!buffer) {
    return 0;
  }
  const int w = buffer->width();
  const int h = buffer->height();
  if (w <= 0 || h <= 0 || w > kMaxSide || h > kMaxSide) {
    return 0;
  }
  webrtc::scoped_refptr<webrtc::I420BufferInterface> i420 = buffer->ToI420();
  if (!i420) {
    return 0;
  }
  // Fit the output box, keeping the aspect ratio; never scaled up. Even sizes, as I420 wants.
  const double scale = std::min({1.0, static_cast<double>(out_width_) / w, static_cast<double>(out_height_) / h});
  int ow = std::max(2, static_cast<int>(w * scale) & ~1);
  int oh = std::max(2, static_cast<int>(h * scale) & ~1);
  if (ow >= w && oh >= h) {
    ow = w;
    oh = h;
  } else {
    auto scaled = webrtc::I420Buffer::Create(ow, oh);
    scaled->ScaleFrom(*i420);
    i420 = scaled;
  }
  Picture picture;
  picture.width = static_cast<uint32_t>(ow);
  picture.height = static_cast<uint32_t>(oh);
  picture.timestamp = frame.rtp_timestamp();
  picture.bgra.resize(static_cast<size_t>(ow) * static_cast<size_t>(oh) * 4);
  // libyuv's "ARGB" is B, G, R, A in memory: what GPUI draws. libwebrtc's VideoToolbox decoder (the only asynchronous
  // one here) asks VideoToolbox for full-range NV12 (kCVPixelFormatType_420YpCbCr8BiPlanarFullRange), so its samples
  // are full range though the frame carries no colour space; read as video range, dark pictures came out darker and
  // light ones lighter.
  if (libyuv::I420ToARGBMatrix(i420->DataY(), i420->StrideY(), i420->DataU(), i420->StrideU(), i420->DataV(),
                               i420->StrideV(), picture.bgra.data(), ow * 4, matrix_of(frame, asynchronous_), ow,
                               oh) != 0) {
    return 0;
  }
  std::lock_guard<std::mutex> lock(mutex_);
  pictures_.push_back(std::move(picture));
  while (pictures_.size() > kMaxPictures) {
    pictures_.pop_front();
  }
  return 0;
}

namespace {

// libwebrtc's own software decoders, made directly: libvpx (VP8, VP9), dav1d (AV1), and the bundled FFmpeg's H.264
// where the build has it (Linux and Windows; the macOS prebuilt's FFmpeg lacks H.264, so its Configure fails and H.264
// goes to the system player there). No platform factory: VideoToolbox (macOS) and the NVIDIA/VA-API factories are not
// used unless the crate feature `dilla-video-toolbox` asks for VideoToolbox (DILLA_VIDEO_TOOLBOX), off by default:
// `decode` waits for VideoToolbox's asynchronous pictures (core/vendor/README.md).
std::unique_ptr<webrtc::VideoDecoder> software_decoder(const webrtc::Environment& env, const std::string& name) {
  if (absl::EqualsIgnoreCase(name, "VP8")) {
    return webrtc::CreateVp8Decoder(env);
  }
  if (absl::EqualsIgnoreCase(name, "VP9")) {
    return webrtc::VP9Decoder::Create();
  }
  if (absl::EqualsIgnoreCase(name, "H264")) {
    return webrtc::H264Decoder::IsSupported() ? webrtc::H264Decoder::Create() : nullptr;
  }
#if defined(RTC_DAV1D_IN_INTERNAL_DECODER_FACTORY)
  if (absl::EqualsIgnoreCase(name, "AV1")) {
    return webrtc::CreateDav1dDecoder();
  }
#endif
  return nullptr;
}

#if defined(DILLA_VIDEO_TOOLBOX)
std::unique_ptr<webrtc::VideoDecoder> platform_decoder(const webrtc::Environment& env, const std::string& name) {
  VideoDecoderFactory factory;
  for (const webrtc::SdpVideoFormat& format : factory.GetSupportedFormats()) {
    if (absl::EqualsIgnoreCase(format.name, name)) {
      return factory.Create(env, format);
    }
  }
  return nullptr;
}
#endif

}  // namespace

std::unique_ptr<DillaVideoDecoder> new_dilla_video_decoder(rust::Str codec, uint32_t out_width, uint32_t out_height) {
  const std::string name(codec.data(), codec.size());
  const webrtc::VideoCodecType type = webrtc::PayloadStringToCodecType(name);
  if (type == webrtc::kVideoCodecGeneric) {
    return nullptr;
  }
  webrtc::Environment env = webrtc::CreateEnvironment();
  std::unique_ptr<webrtc::VideoDecoder> decoder;
  bool asynchronous = false;
#if defined(DILLA_VIDEO_TOOLBOX)
  if (type == webrtc::kVideoCodecH264 || type == webrtc::kVideoCodecH265) {
    decoder = platform_decoder(env, name);
    asynchronous = decoder != nullptr;  // VideoToolbox
  }
#endif
  if (!decoder) {
    decoder = software_decoder(env, name);
  }
  if (!decoder) {
    return nullptr;
  }
  webrtc::VideoDecoder::Settings settings;
  settings.set_codec_type(type);
  settings.set_number_of_cores(2);
  settings.set_max_render_resolution({kMaxSide, kMaxSide});
  if (!decoder->Configure(settings)) {
    // An H.264 decoder whose FFmpeg lacks the codec fails here (the macOS prebuilt): no decoder, the system player.
    decoder->Release();
    return nullptr;
  }
  return std::make_unique<DillaVideoDecoder>(env, std::move(decoder), out_width, out_height, asynchronous);
}

}  // namespace livekit_ffi
