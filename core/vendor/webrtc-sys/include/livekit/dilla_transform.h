/*
 * DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
 *
 * A process-wide encoded-frame hook for dilla-sframe/1 (protocol/05-media-frames.md). Once a hook is
 * installed (dilla_install_hook, from Rust, before any PeerConnection exists):
 *
 *  - every RtpSender (PeerConnection::add_track, add_transceiver, add_transceiver_for_media) gets a
 *    DillaFrameTransformer synchronously, before the negotiation that starts its RTP; one without a
 *    track drops every frame. RtpSender::set_track attaches a fresh one for the incoming track
 *    before the swap;
 *  - PeerConnectionFactory::create_peer_connection refuses to create a PeerConnection without a
 *    hook (E_DILLA_NO_FRAME_HOOK), so there is no stock-LiveKit fallback;
 *  - every RtpReceiver gets one synchronously in PeerConnection::OnTrack, before Rust learns of the
 *    track;
 *  - no other transformer may be set: FrameCryptor and PacketTrailerHandler skip their
 *    SetEncoderToPacketizerFrameTransformer / SetDepacketizerToDecoderFrameTransformer calls
 *    (dilla_owns_transforms()), so LiveKit's own AES-GCM frame format never runs and never replaces
 *    the hook.
 *
 * The transformer has no pass-through of its own: every frame goes to Rust, which answers Drop,
 * Replace (the new bytes), PassUnchanged (only a zero-byte audio frame) or Hold. Held frames are one
 * strict FIFO per transformer (protocol/05 "Rotation": 2 000 ms, 256 frames, 8 MiB); while anything
 * is held, later frames queue behind it, and the FIFO is retried on every new frame.
 */

#pragma once

#include <stdint.h>

#include <deque>
#include <map>
#include <memory>
#include <string>

#include "api/frame_transformer_interface.h"
#include "api/rtp_receiver_interface.h"
#include "api/rtp_sender_interface.h"
#include "api/scoped_refptr.h"
#include "rtc_base/synchronization/mutex.h"
#include "rust/cxx.h"

namespace livekit_ffi {

struct DillaHook;
class DillaHookHolder;

/// Installs the process-wide hook. False (and the hook is dropped) when one is already installed.
bool dilla_install_hook(rust::Box<DillaHook> hook);

/// True once a hook is installed: dilla then owns every encoded-frame transform.
bool dilla_owns_transforms();

/// Attach a fresh DillaFrameTransformer to `sender` / `receiver`; a no-op without a hook (which
/// cannot happen once a PeerConnection exists: create_peer_connection refuses to run without one).
/// The sender form binds to the sender's current track id ("" without a track).
void dilla_attach_sender(
    const webrtc::scoped_refptr<webrtc::RtpSenderInterface>& sender);
/// The sender form for a track that is about to be set (RtpSender::set_track): binds to `track_id`.
void dilla_attach_sender_for_track(
    const webrtc::scoped_refptr<webrtc::RtpSenderInterface>& sender,
    const std::string& track_id);
void dilla_attach_receiver(
    const webrtc::scoped_refptr<webrtc::RtpReceiverInterface>& receiver);

class DillaFrameTransformer : public webrtc::FrameTransformerInterface {
 public:
  DillaFrameTransformer(std::shared_ptr<DillaHookHolder> hook,
                        bool send,
                        bool video,
                        std::string track_id);
  ~DillaFrameTransformer() override;

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
  struct Held {
    std::unique_ptr<webrtc::TransformableFrameInterface> frame;
    int64_t at_ms;
    size_t bytes;
  };

  /// 0 drop, 1 replace (with `out`), 2 hold, 3 pass unchanged.
  uint8_t Decide(webrtc::TransformableFrameInterface& frame,
                 rust::Vec<uint8_t>& out);
  void Emit(std::unique_ptr<webrtc::TransformableFrameInterface> frame);

  const std::shared_ptr<DillaHookHolder> hook_;
  const bool send_;
  const bool video_;
  const std::string track_id_;

  webrtc::Mutex queue_mutex_;
  std::deque<Held> held_;
  size_t held_bytes_ = 0;

  webrtc::Mutex callback_mutex_;
  webrtc::scoped_refptr<webrtc::TransformedFrameCallback> callback_;
  std::map<uint32_t, webrtc::scoped_refptr<webrtc::TransformedFrameCallback>>
      sink_callbacks_;
};

}  // namespace livekit_ffi
