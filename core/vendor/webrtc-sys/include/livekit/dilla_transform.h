/*
 * DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
 *
 * A process-wide encoded-frame hook for dilla-sframe/1 (protocol/05-media-frames.md). Once a hook is
 * installed (dilla_install_hook, from Rust, before any PeerConnection exists):
 *
 *  - every RtpSender (PeerConnection::add_track, add_transceiver, add_transceiver_for_media) gets a
 *    DillaFrameTransformer synchronously, before the negotiation that starts its RTP; one without a
 *    track drops every frame. RtpSender::set_track never replaces a sender's transformer (libwebrtc's
 *    ChannelSend allows one transformer delegate per stream): it refuses a track other than the one
 *    the sender's transformer was attached for;
 *  - PeerConnectionFactory::create_peer_connection refuses to create a PeerConnection without a
 *    hook (E_DILLA_NO_FRAME_HOOK), so there is no stock-LiveKit fallback;
 *  - every RtpReceiver gets one synchronously in PeerConnection::OnTrack, before Rust learns of the
 *    track;
 *  - no other transformer may be set: FrameCryptor and PacketTrailerHandler skip their
 *    SetEncoderToPacketizerFrameTransformer / SetDepacketizerToDecoderFrameTransformer calls
 *    (dilla_owns_transforms()), so LiveKit's own AES-GCM frame format never runs and never replaces
 *    the hook.
 *
 * Every transformer has an opaque handle (a process-unique number, never reused) that Rust learns
 * when the transformer is attached and that names it in every later call. Rust binds a receiver by
 * that handle, found from the receiver's own MediaStreamTrack object (dilla_receiver_handle), never
 * by a track id string the SFU chose.
 *
 * The transformer has no pass-through of its own: every frame goes to Rust, which answers Drop,
 * Replace (the new bytes), PassUnchanged or Hold. PassUnchanged is honoured only for a zero-byte
 * Opus frame on an audio transformer (protocol/05 "No plaintext path"); for anything else it is a
 * drop. Held frames are one strict FIFO per transformer (protocol/05 "Rotation": 2 000 ms, 256
 * frames, 8 MiB); while anything is held, later frames queue behind it. The FIFO is retried on every
 * new frame and whenever dilla_drain_all runs (Rust calls it on a timer and after every key
 * install); a frame past a limit is dropped, oldest first, never released unauthenticated.
 */

#pragma once

#include <stdint.h>

#include <deque>
#include <map>
#include <memory>
#include <string>
#include <vector>

#include "api/frame_transformer_interface.h"
#include "api/media_stream_interface.h"
#include "api/rtp_receiver_interface.h"
#include "api/rtp_sender_interface.h"
#include "api/scoped_refptr.h"
#include "livekit/media_stream_track.h"
#include "rtc_base/synchronization/mutex.h"
#include "rust/cxx.h"

namespace livekit_ffi {

struct DillaHook;
class DillaHookHolder;
class DillaTransformCore;

/// Installs the process-wide hook. False (and the hook is dropped) when one is already installed.
bool dilla_install_hook(rust::Box<DillaHook> hook);

/// True once a hook is installed: dilla then owns every encoded-frame transform.
bool dilla_owns_transforms();

/// Attach a fresh DillaFrameTransformer to `sender` / `receiver`; a no-op without a hook (which
/// cannot happen once a PeerConnection exists: create_peer_connection refuses to run without one).
/// The sender form binds to the sender's current track ("" without a track).
void dilla_attach_sender(
    const webrtc::scoped_refptr<webrtc::RtpSenderInterface>& sender);
void dilla_attach_receiver(
    const webrtc::scoped_refptr<webrtc::RtpReceiverInterface>& receiver);

/// RtpSender::set_track under the hook: true when `track_id` is the track the sender's live
/// transformer was attached for, so the swap needs no new transformer. A sender's transformer is
/// never replaced (see the file comment).
bool dilla_sender_accepts_track(
    const webrtc::scoped_refptr<webrtc::RtpSenderInterface>& sender,
    const std::string& track_id);

/// LiveKit's KeyProvider::SetSifTrailer, forwarded to the hook (the room's server-injected-frame
/// trailer from the join response; replaced, never appended).
void dilla_set_sif_trailer(const std::vector<uint8_t>& trailer);

/// The handle of the live receiver (or sender) transformer attached for exactly this track object;
/// 0 when there is none. The newest wins when a receiver was attached twice.
uint64_t dilla_receiver_handle(const MediaStreamTrack& track);
uint64_t dilla_sender_handle(const MediaStreamTrack& track);

/// Retries every transformer's held frames now (expiring those past a limit).
void dilla_drain_all();

/// Asks the publisher of a receiving video transformer's track for a key frame (an RTCP PLI through
/// the SFU). Blocks on libwebrtc's worker thread: never call it from inside a transform.
void dilla_request_key_frame(uint64_t handle);

// Test support (webrtc_sys::dilla_transform::testing): a transformer with no PeerConnection, fed
// synthetic frames, whose output is recorded. Never used by a call.
uint64_t dilla_test_attach(bool send, bool video, rust::Str track_id);
void dilla_test_frame(uint64_t handle, rust::Str mime, rust::Slice<const uint8_t> data,
                      bool key_frame);
rust::Vec<uint8_t> dilla_test_take_emitted(uint64_t handle);
size_t dilla_test_emitted_count(uint64_t handle);
size_t dilla_test_held(uint64_t handle);
void dilla_test_release(uint64_t handle);

/// Everything one transformer owns: its handle, its FIFO, its callbacks. Shared so that
/// dilla_drain_all can run it without touching a transformer libwebrtc is destroying.
class DillaTransformCore {
 public:
  DillaTransformCore(std::shared_ptr<DillaHookHolder> hook,
                     uint64_t handle,
                     bool send,
                     bool video,
                     std::string track_id,
                     webrtc::scoped_refptr<webrtc::MediaStreamTrackInterface> track,
                     const void* owner,
                     webrtc::scoped_refptr<webrtc::VideoTrackSourceInterface> key_source);
  ~DillaTransformCore();

  /// Queues `frame` (or nothing) and runs the FIFO.
  void Run(std::unique_ptr<webrtc::TransformableFrameInterface> frame);
  size_t HeldCount();

  void SetCallback(webrtc::scoped_refptr<webrtc::TransformedFrameCallback> callback);
  void SetSinkCallback(webrtc::scoped_refptr<webrtc::TransformedFrameCallback> callback,
                       uint32_t ssrc);
  void ClearCallback();
  void ClearSinkCallback(uint32_t ssrc);

  const uint64_t handle;
  const bool send;
  const bool video;
  const std::string track_id;
  const webrtc::scoped_refptr<webrtc::MediaStreamTrackInterface> track;
  /// The RtpSender (proxy) or RtpReceiver this core was attached to; compared, never dereferenced.
  const void* const owner;
  /// A receiving video track's source: GenerateKeyFrame sends a PLI.
  const webrtc::scoped_refptr<webrtc::VideoTrackSourceInterface> key_source;

 private:
  struct Held {
    std::unique_ptr<webrtc::TransformableFrameInterface> frame;
    int64_t at_ms;
    size_t bytes;
  };

  /// 0 drop, 1 replace (with `out`), 2 hold, 3 pass unchanged.
  uint8_t Decide(webrtc::TransformableFrameInterface& frame, rust::Vec<uint8_t>& out);
  void Emit(std::unique_ptr<webrtc::TransformableFrameInterface> frame);

  const std::shared_ptr<DillaHookHolder> hook_;

  // Order: run_mutex_ (one FIFO run and its emits at a time, so frames leave in order), then
  // queue_mutex_, then callback_mutex_.
  webrtc::Mutex run_mutex_;
  webrtc::Mutex queue_mutex_;
  std::deque<Held> held_;
  size_t held_bytes_ = 0;

  webrtc::Mutex callback_mutex_;
  webrtc::scoped_refptr<webrtc::TransformedFrameCallback> callback_;
  std::map<uint32_t, webrtc::scoped_refptr<webrtc::TransformedFrameCallback>>
      sink_callbacks_;
};

class DillaFrameTransformer : public webrtc::FrameTransformerInterface {
 public:
  explicit DillaFrameTransformer(std::shared_ptr<DillaTransformCore> core);
  ~DillaFrameTransformer() override;

  uint64_t handle() const { return core_->handle; }

  void Transform(
      std::unique_ptr<webrtc::TransformableFrameInterface> frame) override;
  void RegisterTransformedFrameCallback(
      webrtc::scoped_refptr<webrtc::TransformedFrameCallback> callback) override;
  void RegisterTransformedFrameSinkCallback(
      webrtc::scoped_refptr<webrtc::TransformedFrameCallback> callback,
      uint32_t ssrc) override;
  void UnregisterTransformedFrameCallback() override;
  void UnregisterTransformedFrameSinkCallback(uint32_t ssrc) override;

 private:
  const std::shared_ptr<DillaTransformCore> core_;
};

}  // namespace livekit_ffi
