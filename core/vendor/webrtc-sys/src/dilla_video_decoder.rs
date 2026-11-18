// DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
//
// The Rust side of include/livekit/dilla_video_decoder.h: libwebrtc's own video decoders (the ones a call already
// uses), standalone, for the desktop's inline attachment player. Access units in, BGRA pictures out.

use cxx::UniquePtr;

#[cxx::bridge(namespace = "livekit_ffi")]
pub mod ffi {
    /// A decoded picture's size (after the fit to the output box) and the timestamp its access unit was fed with.
    #[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
    pub struct DillaFrameInfo {
        pub width: u32,
        pub height: u32,
        pub timestamp: u32,
    }

    unsafe extern "C++" {
        include!("livekit/dilla_video_decoder.h");

        type DillaVideoDecoder;

        fn new_dilla_video_decoder(
            codec: &str,
            out_width: u32,
            out_height: u32,
        ) -> UniquePtr<DillaVideoDecoder>;
        fn decode(
            self: Pin<&mut DillaVideoDecoder>,
            data: &[u8],
            timestamp: u32,
            key_frame: bool,
        ) -> i32;
        fn reorders(self: &DillaVideoDecoder) -> bool;
        fn peek_frame(self: Pin<&mut DillaVideoDecoder>, info: &mut DillaFrameInfo) -> bool;
        fn take_frame(self: Pin<&mut DillaVideoDecoder>, bgra: &mut [u8]) -> bool;
    }
}

pub use ffi::DillaFrameInfo;

/// Whether this build decodes H.264 and H.265 with VideoToolbox (macOS, the crate feature `dilla-video-toolbox`).
pub const VIDEO_TOOLBOX: bool = cfg!(feature = "dilla-video-toolbox");

// The decoder is used from one thread at a time (it moves to the player's decode thread); the glue guards the
// pictures an asynchronous decoder hands back from its own thread.
unsafe impl Send for ffi::DillaVideoDecoder {}

/// The codecs [`VideoDecoder::new`] knows by name.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum VideoCodec {
    Vp8,
    Vp9,
    Av1,
    /// Annex B access units (start codes, the parameter sets before each key frame).
    H264,
    /// Annex B; only where the platform decodes it (VideoToolbox on macOS).
    H265,
}

impl VideoCodec {
    fn sdp_name(self) -> &'static str {
        match self {
            Self::Vp8 => "VP8",
            Self::Vp9 => "VP9",
            Self::Av1 => "AV1",
            Self::H264 => "H264",
            Self::H265 => "H265",
        }
    }
}

/// One decoded picture: BGRA rows of `width * 4` bytes.
pub struct DecodedPicture {
    pub width: u32,
    pub height: u32,
    pub timestamp: u32,
    pub bgra: Vec<u8>,
}

/// A libwebrtc video decoder.
pub struct VideoDecoder {
    inner: UniquePtr<ffi::DillaVideoDecoder>,
}

impl VideoDecoder {
    /// A decoder for `codec` whose pictures are scaled down to fit `out_width` x `out_height`; `None` when this build
    /// has none for it.
    pub fn new(codec: VideoCodec, out_width: u32, out_height: u32) -> Option<Self> {
        let inner = ffi::new_dilla_video_decoder(codec.sdp_name(), out_width, out_height);
        (!inner.is_null()).then_some(Self { inner })
    }

    /// Feeds one access unit; `false` when the decoder refused it (the next key frame starts it again).
    pub fn decode(&mut self, data: &[u8], timestamp: u32, key_frame: bool) -> bool {
        self.inner.pin_mut().decode(data, timestamp, key_frame) >= 0
    }

    /// See the header: pictures come out in presentation order under the last fed timestamp.
    pub fn reorders(&self) -> bool {
        self.inner.reorders()
    }

    /// The oldest decoded picture, if any.
    pub fn next_picture(&mut self) -> Option<DecodedPicture> {
        let mut info = DillaFrameInfo::default();
        if !self.inner.pin_mut().peek_frame(&mut info) {
            return None;
        }
        let mut bgra = vec![0u8; info.width as usize * info.height as usize * 4];
        self.inner.pin_mut().take_frame(&mut bgra).then_some(DecodedPicture {
            width: info.width,
            height: info.height,
            timestamp: info.timestamp,
            bgra,
        })
    }
}
