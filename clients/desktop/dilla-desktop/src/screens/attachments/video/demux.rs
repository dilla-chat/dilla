//! The video track of an attachment, out of its container: MP4 (`re_mp4`) and WebM/Matroska (`matroska-demuxer`), both
//! pure Rust. Everything is read from the decrypted bytes in memory; a Matroska file's frames are gathered into one
//! [`Zeroizing`] buffer.
//!
//! Hostile files: both demuxers bound what they allocate by the bytes they were given, and a panic inside either is
//! caught by the caller (`video::open`). On top of that an MP4's sample counts are checked against its size before
//! `re_mp4` builds its sample table ([`mp4_counts_fit`]), and no track has more than [`MAX_SAMPLES`].

use std::io::Cursor;
use std::ops::Range;
use std::sync::Arc;
use std::time::Duration;

use webrtc_sys::dilla_video_decoder::VideoCodec;
use zeroize::Zeroizing;

use super::super::media::MediaError;

/// The most samples a video track may have (a day at 12 fps): past it the file is refused.
pub const MAX_SAMPLES: usize = 1 << 20;
/// The largest picture taken (either side, before the fit), as the call path caps a stream.
pub const MAX_SIDE: u32 = 8192;

fn unsupported(why: &str) -> MediaError {
    MediaError::Unsupported(why.to_owned())
}

/// How the picture is turned for display (an MP4 track header's matrix): clockwise quarter turns.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Rotation {
    #[default]
    None,
    Cw90,
    Cw180,
    Cw270,
}

/// One access unit, in decode order.
#[derive(Clone, Debug)]
pub struct Sample {
    pub range: Range<usize>,
    /// When it is shown.
    pub pts: Duration,
    pub key: bool,
}

/// H.264 and H.265 in MP4 or Matroska are length-prefixed NAL units with the parameter sets out of band: the decoder
/// takes Annex B, start codes and the parameter sets before each key frame.
#[derive(Clone, Debug)]
pub struct ParamSets {
    pub length_size: usize,
    pub nals: Vec<Vec<u8>>,
}

/// A container's video track.
pub struct VideoTrack {
    pub codec: VideoCodec,
    /// The coded size, before any rotation.
    pub width: u32,
    pub height: u32,
    pub rotation: Rotation,
    pub duration: Option<Duration>,
    /// Decode order.
    pub samples: Vec<Sample>,
    pub params: Option<ParamSets>,
    data: Arc<Zeroizing<Vec<u8>>>,
}

impl VideoTrack {
    /// The size the picture is shown at: turned by a quarter, width and height trade places.
    pub fn display_size(&self) -> (u32, u32) {
        match self.rotation {
            Rotation::Cw90 | Rotation::Cw270 => (self.height, self.width),
            Rotation::None | Rotation::Cw180 => (self.width, self.height),
        }
    }

    /// Sample `i` as the decoder takes it; `None` for a malformed one (a length past its end).
    pub fn access_unit(&self, i: usize) -> Option<Vec<u8>> {
        let s = self.samples.get(i)?;
        let raw = self.data.get(s.range.clone())?;
        match &self.params {
            None => Some(raw.to_vec()),
            Some(p) => annex_b(raw, p, s.key),
        }
    }

    /// The index of the key frame to start decoding at for a picture at `to`: the last key frame (in decode order)
    /// shown at or before it, or the first key frame.
    pub fn key_before(&self, to: Duration) -> usize {
        let mut best = None;
        for (i, s) in self.samples.iter().enumerate() {
            if s.key && (best.is_none() || s.pts <= to) {
                best = Some(i);
            }
        }
        best.unwrap_or(0)
    }
}

const START: [u8; 4] = [0, 0, 0, 1];

/// Length-prefixed NAL units to Annex B, the parameter sets first on a key frame.
pub fn annex_b(raw: &[u8], p: &ParamSets, key: bool) -> Option<Vec<u8>> {
    let mut out = Vec::with_capacity(raw.len() + 64);
    if key {
        for n in &p.nals {
            out.extend_from_slice(&START);
            out.extend_from_slice(n);
        }
    }
    let mut at = 0;
    while at < raw.len() {
        let len_end = at.checked_add(p.length_size)?;
        let len = raw
            .get(at..len_end)?
            .iter()
            .fold(0usize, |n, b| (n << 8) | usize::from(*b));
        let end = len_end.checked_add(len)?;
        out.extend_from_slice(&START);
        out.extend_from_slice(raw.get(len_end..end)?);
        at = end;
    }
    Some(out)
}

/// An `avcC` record (ISO/IEC 14496-15 §5.3.3.1): its NAL length size and SPS and PPS.
pub fn parse_avcc(b: &[u8]) -> Option<ParamSets> {
    let length_size = usize::from(*b.get(4)? & 3) + 1;
    let mut nals = Vec::new();
    let mut at = 5;
    for mask in [0x1f, 0xff] {
        let count = b.get(at)? & mask;
        at += 1;
        for _ in 0..count {
            let len = usize::from(u16::from_be_bytes([*b.get(at)?, *b.get(at + 1)?]));
            nals.push(b.get(at + 2..at + 2 + len)?.to_vec());
            at += 2 + len;
        }
    }
    (length_size != 3).then_some(ParamSets { length_size, nals })
}

/// An `hvcC` record (ISO/IEC 14496-15 §8.3.3.1): its NAL length size and its VPS, SPS and PPS arrays.
pub fn parse_hvcc(b: &[u8]) -> Option<ParamSets> {
    let length_size = usize::from(*b.get(21)? & 3) + 1;
    let arrays = *b.get(22)?;
    let mut at = 23;
    let mut nals = Vec::new();
    for _ in 0..arrays {
        let count = usize::from(u16::from_be_bytes([*b.get(at + 1)?, *b.get(at + 2)?]));
        at += 3;
        for _ in 0..count {
            let len = usize::from(u16::from_be_bytes([*b.get(at)?, *b.get(at + 1)?]));
            nals.push(b.get(at + 2..at + 2 + len)?.to_vec());
            at += 2 + len;
        }
    }
    (length_size != 3).then_some(ParamSets { length_size, nals })
}

/// The record inside a raw `avcC`/`hvcC` box (its 8-byte header dropped when present).
fn box_body<'a>(raw: &'a [u8], kind: &[u8; 4]) -> &'a [u8] {
    if raw.len() >= 8 && &raw[4..8] == kind {
        &raw[8..]
    } else {
        raw
    }
}

/// Whether a VP8 frame is a key frame (RFC 6386 §9.1: bit 0 of the frame tag is 0).
fn vp8_key(b: &[u8]) -> bool {
    b.first().is_some_and(|x| x & 1 == 0)
}

/// Whether a VP9 frame is a key frame (VP9 bitstream §6.2: the uncompressed header's frame_type is 0).
fn vp9_key(b: &[u8]) -> bool {
    let Some(&x) = b.first() else { return false };
    if x >> 6 != 0b10 {
        return false;
    }
    let profile = ((x >> 5) & 1) | (((x >> 4) & 1) << 1);
    // profile 3 has a reserved bit before show_existing_frame.
    let mut bit = if profile == 3 { 3 } else { 4 };
    let show_existing = (x >> (bit - 1)) & 1;
    if show_existing == 1 {
        return false;
    }
    bit -= 1;
    (x >> (bit - 1)) & 1 == 0
}

/// The sample counts an MP4 declares (`stsz`, `trun`) fit its size: `re_mp4` would otherwise build a table of four
/// billion samples from a 40-byte box.
pub fn mp4_counts_fit(b: &[u8]) -> bool {
    fn walk(b: &[u8], depth: u32, total: usize) -> bool {
        let mut at = 0usize;
        while at + 8 <= b.len() {
            let size32 = u32::from_be_bytes([b[at], b[at + 1], b[at + 2], b[at + 3]]) as usize;
            let kind = &b[at + 4..at + 8];
            let (size, head) = match size32 {
                0 => (b.len() - at, 8),
                1 => {
                    let Some(s) = b.get(at + 8..at + 16) else {
                        return false;
                    };
                    let s = u64::from_be_bytes(s.try_into().expect("8 bytes"));
                    (usize::try_from(s).unwrap_or(usize::MAX), 16)
                }
                s => (s, 8),
            };
            if size < head {
                // A box smaller than its own header: refused (a reader could step backwards over it forever).
                return false;
            }
            if at + size > b.len() {
                // A truncated last box: re_mp4 reads what is there.
                return true;
            }
            let body = &b[at + head..at + size];
            match kind {
                b"moov" | b"trak" | b"mdia" | b"minf" | b"stbl" | b"moof" | b"traf" => {
                    if depth > 8 || !walk(body, depth + 1, total) {
                        return false;
                    }
                }
                b"stsz" if body.len() >= 12 => {
                    let one = u32::from_be_bytes([body[4], body[5], body[6], body[7]]) as usize;
                    let n = u32::from_be_bytes([body[8], body[9], body[10], body[11]]) as usize;
                    if n > MAX_SAMPLES || (one > 0 && n.saturating_mul(one) > total) {
                        return false;
                    }
                }
                b"trun" if body.len() >= 8 => {
                    let n = u32::from_be_bytes([body[4], body[5], body[6], body[7]]) as usize;
                    if n > MAX_SAMPLES {
                        return false;
                    }
                }
                _ => {}
            }
            at += size;
        }
        true
    }
    walk(b, 0, b.len())
}

/// A track header matrix's `a`, `b`, `c` and `d` as a turn (ISO/IEC 14496-12 §6.2.2: x' = a x + c y, y' = b x + d y,
/// 16.16); anything but a quarter turn is shown unturned.
fn rotation_of(a: i32, b: i32, c: i32, d: i32) -> Rotation {
    let one = 0x1_0000;
    match (a, b, c, d) {
        (0, b, c, 0) if b == one && c == -one => Rotation::Cw90,
        (a, 0, 0, d) if a == -one && d == -one => Rotation::Cw180,
        (0, b, c, 0) if b == -one && c == one => Rotation::Cw270,
        _ => Rotation::None,
    }
}

fn duration_of(units: u64, scale: u64) -> Option<Duration> {
    if scale == 0 {
        return None;
    }
    Duration::try_from_secs_f64(units as f64 / scale as f64)
        .ok()
        .filter(|d| *d <= super::super::media::MAX_DURATION)
}

fn checked_size(w: u64, h: u64) -> Result<(u32, u32), MediaError> {
    if w == 0 || h == 0 || w > u64::from(MAX_SIDE) || h > u64::from(MAX_SIDE) {
        return Err(unsupported("an unplayable picture size"));
    }
    Ok((w as u32, h as u32))
}

/// The first video track of an MP4.
pub fn mp4(data: Arc<Zeroizing<Vec<u8>>>) -> Result<VideoTrack, MediaError> {
    use re_mp4::StsdBoxContent as C;
    if !mp4_counts_fit(&data) {
        return Err(unsupported("sample counts larger than the file"));
    }
    let file = re_mp4::Mp4::read_bytes(&data).map_err(|e| unsupported(&e.to_string()))?;
    let track = file
        .tracks()
        .values()
        .find(|t| t.kind == Some(re_mp4::TrackKind::Video))
        .ok_or_else(|| unsupported("no video track"))?;
    let trak = track.trak(&file);
    let (codec, params) = match &trak.mdia.minf.stbl.stsd.contents {
        C::Avc1(b) => (
            VideoCodec::H264,
            Some(
                parse_avcc(box_body(&b.avcc.raw, b"avcC"))
                    .ok_or_else(|| unsupported("a broken avcC"))?,
            ),
        ),
        C::Hvc1(b) | C::Hev1(b) => (
            VideoCodec::H265,
            Some(
                parse_hvcc(box_body(&b.hvcc.raw, b"hvcC"))
                    .ok_or_else(|| unsupported("a broken hvcC"))?,
            ),
        ),
        C::Vp08(_) => (VideoCodec::Vp8, None),
        C::Vp09(_) => (VideoCodec::Vp9, None),
        C::Av01(_) => (VideoCodec::Av1, None),
        _ => return Err(unsupported("no video codec here")),
    };
    let (width, height) = checked_size(u64::from(track.width), u64::from(track.height))?;
    if track.samples.len() > MAX_SAMPLES {
        return Err(unsupported("too many samples"));
    }
    let mut samples = Vec::with_capacity(track.samples.len());
    for s in &track.samples {
        let start = usize::try_from(s.offset).map_err(|_| unsupported("a sample past the end"))?;
        let end = usize::try_from(s.size)
            .ok()
            .and_then(|n| start.checked_add(n))
            .filter(|e| *e <= data.len())
            .ok_or_else(|| unsupported("a sample past the end"))?;
        let pts = duration_of(s.composition_timestamp.max(0) as u64, s.timescale)
            .unwrap_or(Duration::ZERO);
        samples.push(Sample {
            range: start..end,
            pts,
            key: s.is_sync,
        });
    }
    let duration = duration_of(track.duration, track.timescale);
    let m = &trak.tkhd.matrix;
    let rotation = rotation_of(m.a, m.b, m.c, m.d);
    Ok(VideoTrack {
        codec,
        width,
        height,
        rotation,
        duration,
        samples,
        params,
        data,
    })
}

/// The first playable video track of a WebM or Matroska file.
pub fn matroska(data: Arc<Zeroizing<Vec<u8>>>) -> Result<VideoTrack, MediaError> {
    use matroska_demuxer::{Frame, MatroskaFile, TrackType};
    let mut file = MatroskaFile::open(Cursor::new(data.as_slice()))
        .map_err(|e| unsupported(&e.to_string()))?;
    let scale = file.info().timestamp_scale().get();
    let duration = file
        .info()
        .duration()
        .filter(|d| d.is_finite() && *d >= 0.0)
        .and_then(|d| Duration::try_from_secs_f64(d * scale as f64 / 1e9).ok())
        .filter(|d| *d <= super::super::media::MAX_DURATION);
    let (number, codec, params, size) = file
        .tracks()
        .iter()
        .filter(|t| t.track_type() == TrackType::Video)
        .find_map(|t| {
            let (codec, params) = match t.codec_id() {
                "V_VP8" => (VideoCodec::Vp8, None),
                "V_VP9" => (VideoCodec::Vp9, None),
                "V_AV1" => (VideoCodec::Av1, None),
                "V_MPEG4/ISO/AVC" => (VideoCodec::H264, Some(parse_avcc(t.codec_private()?)?)),
                "V_MPEGH/ISO/HEVC" => (VideoCodec::H265, Some(parse_hvcc(t.codec_private()?)?)),
                _ => return None,
            };
            let v = t.video()?;
            Some((
                t.track_number().get(),
                codec,
                params,
                (v.pixel_width().get(), v.pixel_height().get()),
            ))
        })
        .ok_or_else(|| unsupported("no playable video track"))?;
    let (width, height) = checked_size(size.0, size.1)?;
    let mut arena: Zeroizing<Vec<u8>> = Zeroizing::new(Vec::new());
    let mut samples = Vec::new();
    let mut frame = Frame::default();
    loop {
        match file.next_frame(&mut frame) {
            Ok(true) => {}
            Ok(false) => break,
            // A damaged tail: what was read plays, as a browser plays it.
            Err(_) => break,
        }
        if frame.track != number {
            continue;
        }
        if samples.len() >= MAX_SAMPLES {
            return Err(unsupported("too many frames"));
        }
        let key = frame.is_keyframe.unwrap_or_else(|| match codec {
            VideoCodec::Vp8 => vp8_key(&frame.data),
            VideoCodec::Vp9 => vp9_key(&frame.data),
            _ => samples.is_empty(),
        });
        let start = arena.len();
        arena.extend_from_slice(&frame.data);
        let pts = Duration::from_nanos(frame.timestamp.saturating_mul(scale));
        samples.push(Sample {
            range: start..arena.len(),
            pts,
            key,
        });
    }
    // The frame buffer held plaintext too.
    zeroize::Zeroize::zeroize(&mut frame.data);
    if samples.is_empty() {
        return Err(unsupported("no frames"));
    }
    let duration = duration.or_else(|| samples.iter().map(|s| s.pts).max());
    Ok(VideoTrack {
        codec,
        width,
        height,
        rotation: Rotation::None,
        duration,
        samples,
        params,
        data: Arc::new(arena),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn length_prefixed_nal_units_become_annex_b_with_the_parameter_sets_on_a_key_frame() {
        let p = ParamSets {
            length_size: 4,
            nals: vec![vec![0x67, 1], vec![0x68, 2]],
        };
        let raw = [0, 0, 0, 2, 0x65, 9, 0, 0, 0, 1, 0x06];
        assert_eq!(
            annex_b(&raw, &p, true).unwrap(),
            [
                0, 0, 0, 1, 0x67, 1, 0, 0, 0, 1, 0x68, 2, 0, 0, 0, 1, 0x65, 9, 0, 0, 0, 1, 0x06
            ]
        );
        assert_eq!(
            annex_b(&raw, &p, false).unwrap(),
            [0, 0, 0, 1, 0x65, 9, 0, 0, 0, 1, 0x06]
        );
        // A length past the end is a malformed sample, never a panic.
        assert!(annex_b(&[0, 0, 0, 9, 1], &p, false).is_none());
        assert!(annex_b(&[0, 0], &p, false).is_none());
    }

    #[test]
    fn an_avcc_record_names_its_length_size_and_parameter_sets() {
        let rec = [1, 0x64, 0, 0x1f, 0xff, 0xe1, 0, 2, 0x67, 7, 1, 0, 1, 0x68];
        let p = parse_avcc(&rec).unwrap();
        assert_eq!(p.length_size, 4);
        assert_eq!(p.nals, vec![vec![0x67, 7], vec![0x68]]);
        assert!(parse_avcc(&rec[..9]).is_none(), "a truncated record");
    }

    #[test]
    fn vp8_and_vp9_key_frames_are_told_from_their_first_byte() {
        assert!(vp8_key(&[0x10]));
        assert!(!vp8_key(&[0x11]));
        // VP9 profile 0: marker 10, profile 00, show_existing 0, frame_type 0 (key) / 1 (inter).
        assert!(vp9_key(&[0b1000_0000]));
        assert!(!vp9_key(&[0b1000_0100]));
        assert!(!vp9_key(&[0b1000_1000]), "show_existing_frame");
        assert!(!vp9_key(&[0b0000_0000]), "no frame marker");
    }

    #[test]
    fn an_mp4_declaring_more_samples_than_its_size_is_refused_before_parsing() {
        let mut stsz = Vec::new();
        stsz.extend_from_slice(&20u32.to_be_bytes());
        stsz.extend_from_slice(b"stsz");
        stsz.extend_from_slice(&[0, 0, 0, 0]);
        stsz.extend_from_slice(&1000u32.to_be_bytes());
        stsz.extend_from_slice(&u32::MAX.to_be_bytes());
        let mut stbl = Vec::new();
        stbl.extend_from_slice(&((8 + stsz.len()) as u32).to_be_bytes());
        stbl.extend_from_slice(b"stbl");
        stbl.extend_from_slice(&stsz);
        assert!(!mp4_counts_fit(&stbl));
        assert!(
            !mp4_counts_fit(b"\0\0\0\x04free\0\0\0\0"),
            "a box inside its own header"
        );
        assert!(mp4_counts_fit(include_bytes!(
            "../testdata/video-h264-aac.mp4"
        )));
    }
}
