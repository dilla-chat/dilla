//! Inline video playback (parity plan Package 12; Jonas 2026-11-15: before the switch). The web client played a video
//! attachment in Chromium's `<video controls>`; this plays the same files in the card.
//!
//! The decode approach (the commit that added it says why): libwebrtc's own decoders, which every call already links,
//! through a small patch to the vendored webrtc-sys (`dilla_video_decoder`): libvpx for VP8 and VP9, dav1d for AV1,
//! the H.264 decoder libwebrtc bundles on Linux and Windows and VideoToolbox on macOS (which also has H.265). No new
//! native library, so no new licence, no growth of the shipped binary beyond the glue, and no new glibc symbol. The
//! containers are read by two pure-Rust demuxers ([`demux`]); the sound goes through the audio card's path
//! (`media::open_audio`, rodio).
//!
//! - [`open`]: demux, make the decoder, decode the first picture (the poster). A container, codec or build this
//!   cannot play is [`MediaError::Unsupported`]: the card falls back to the system player, as before.
//! - [`VideoPlayback`]: a decode thread keeps a few pictures ahead of a wall clock; the card shows the newest picture
//!   due. The clock, the sound and the pictures start, pause and seek together.
//!
//! The decrypted file stays in memory (`Zeroizing`); the decoded pictures are plain buffers, freed, not wiped, like the
//! audio card's PCM (ui.rs's module note).

pub mod demux;

use std::collections::{BinaryHeap, VecDeque};
use std::sync::{Arc, Condvar, Mutex};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

use gpui::RenderImage;
use webrtc_sys::dilla_video_decoder::{DecodedPicture, VideoDecoder};
use zeroize::Zeroizing;

use super::media::{MediaDecoder, MediaError, Playback, open_audio};
use demux::{Rotation, VideoTrack};

/// Pictures are scaled down (never up) to fit this box before they are kept: a fullscreen 1080p picture stays sharp,
/// and a 4K file costs a quarter of its size per kept picture.
pub const OUT_BOX: u32 = 1920;
/// How many decoded pictures the thread keeps ahead of the clock.
const AHEAD: usize = 4;
/// How many access units the poster may take (a stream whose first pictures are held back by reordering).
const POSTER_TRIES: usize = 16;

/// One decoded picture, ready to draw.
#[derive(Clone)]
pub struct Frame {
    pub pts: Duration,
    pub image: Arc<RenderImage>,
    /// Its size as shown (after the track's rotation).
    pub width: u32,
    pub height: u32,
}

/// A video ready to play: its track and first picture.
pub struct OpenedVideo {
    track: Arc<VideoTrack>,
    pub poster: Frame,
    /// The sound of the file, when it has a track this plays.
    audio: Option<Box<dyn MediaDecoder>>,
}

impl OpenedVideo {
    pub fn duration(&self) -> Option<Duration> {
        self.track.duration
    }
    pub fn has_audio(&self) -> bool {
        self.audio.is_some()
    }
}

/// Runs a demuxer or decoder call so that a panic inside one (a hostile file found a bug) is "cannot play".
fn guarded<T>(f: impl FnOnce() -> T) -> Option<T> {
    std::panic::catch_unwind(std::panic::AssertUnwindSafe(f)).ok()
}

/// The container a MIME type and the bytes agree on.
enum Container {
    Mp4,
    Matroska,
}

fn container(mime: &str, b: &[u8]) -> Option<Container> {
    let iso = b.len() >= 12
        && matches!(
            &b[4..8],
            b"ftyp" | b"moov" | b"mdat" | b"wide" | b"free" | b"skip"
        );
    let ebml = b.starts_with(&[0x1a, 0x45, 0xdf, 0xa3]);
    match mime {
        "video/mp4" | "video/quicktime" if iso => Some(Container::Mp4),
        "video/webm" | "video/x-matroska" if ebml => Some(Container::Matroska),
        _ => None,
    }
}

/// Turns a decoded BGRA picture by the track's rotation (clockwise quarter turns).
fn rotate(p: DecodedPicture, r: Rotation) -> (u32, u32, Vec<u8>) {
    let (w, h) = (p.width as usize, p.height as usize);
    let src = p.bgra;
    let px = |x: usize, y: usize| {
        let i = (y * w + x) * 4;
        [src[i], src[i + 1], src[i + 2], src[i + 3]]
    };
    let turned = |ow: usize, oh: usize, from: &dyn Fn(usize, usize) -> (usize, usize)| {
        let mut out = vec![0u8; ow * oh * 4];
        for y in 0..oh {
            for x in 0..ow {
                let (sx, sy) = from(x, y);
                out[(y * ow + x) * 4..][..4].copy_from_slice(&px(sx, sy));
            }
        }
        out
    };
    match r {
        Rotation::None => (p.width, p.height, src),
        // Output (x, y) of a clockwise quarter turn shows source (y, h - 1 - x).
        Rotation::Cw90 => (p.height, p.width, turned(h, w, &|x, y| (y, h - 1 - x))),
        Rotation::Cw180 => (
            p.width,
            p.height,
            turned(w, h, &|x, y| (w - 1 - x, h - 1 - y)),
        ),
        Rotation::Cw270 => (p.height, p.width, turned(h, w, &|x, y| (w - 1 - y, x))),
    }
}

fn frame_of(p: DecodedPicture, pts: Duration, r: Rotation) -> Option<Frame> {
    let (width, height, bgra) = rotate(p, r);
    // GPUI draws an image's bytes as BGRA.
    let buf = image::RgbaImage::from_raw(width, height, bgra)?;
    Some(Frame {
        pts,
        image: Arc::new(RenderImage::new(vec![image::Frame::new(buf)])),
        width,
        height,
    })
}

/// Opens `bytes` (a decrypted video attachment typed `mime`): its video track, a decoder for it, its first picture,
/// and its sound. Any container, codec, header or decoder failure, and any panic in one, is
/// [`MediaError::Unsupported`].
pub fn open(bytes: Arc<Zeroizing<Vec<u8>>>, mime: &str) -> Result<OpenedVideo, MediaError> {
    guarded(move || open_inner(bytes, mime))
        .unwrap_or_else(|| Err(MediaError::Unsupported("the demuxer failed".to_owned())))
}

fn open_inner(bytes: Arc<Zeroizing<Vec<u8>>>, mime: &str) -> Result<OpenedVideo, MediaError> {
    let track = match container(mime, &bytes) {
        Some(Container::Mp4) => demux::mp4(bytes.clone())?,
        Some(Container::Matroska) => demux::matroska(bytes.clone())?,
        None => {
            return Err(MediaError::Unsupported(
                "not a container this plays".to_owned(),
            ));
        }
    };
    let track = Arc::new(track);
    let mut dec = Feeder::new(&track)?;
    let mut poster = None;
    for i in (track.key_before(Duration::ZERO)..).take(POSTER_TRIES) {
        if let Some(f) = dec.feed(&track, i).into_iter().next() {
            poster = Some(f);
            break;
        }
    }
    let poster = poster.ok_or_else(|| MediaError::Unsupported("no picture decodes".to_owned()))?;
    // The file's sound, if symphonia reads a track of it (a video without sound, or with a codec it lacks, plays mute).
    let audio = open_audio(bytes, mime)
        .ok()
        .map(|d| Box::new(d) as Box<dyn MediaDecoder>);
    Ok(OpenedVideo {
        track,
        poster,
        audio,
    })
}

/// A decoder and what it needs to time its pictures.
struct Feeder {
    dec: VideoDecoder,
    /// FFmpeg's pictures come in presentation order under the last fed timestamp: each takes the earliest
    /// presentation time still owed ([`VideoDecoder::reorders`]).
    owed: Option<BinaryHeap<std::cmp::Reverse<Duration>>>,
}

impl Feeder {
    fn new(track: &VideoTrack) -> Result<Self, MediaError> {
        let dec = VideoDecoder::new(track.codec, OUT_BOX, OUT_BOX).ok_or_else(|| {
            MediaError::Unsupported(format!("no {:?} decoder in this build", track.codec))
        })?;
        let owed = dec.reorders().then(BinaryHeap::new);
        Ok(Self { dec, owed })
    }

    /// Feeds sample `i` and answers the pictures that came out.
    fn feed(&mut self, track: &VideoTrack, i: usize) -> Vec<Frame> {
        let Some(sample) = track.samples.get(i) else {
            return Vec::new();
        };
        if let Some(au) = track.access_unit(i) {
            // A refused unit (or one still held for reordering) is not fatal: the next one goes on.
            let _ = guarded(|| self.dec.decode(&au, i as u32, sample.key));
            if let Some(owed) = &mut self.owed {
                owed.push(std::cmp::Reverse(sample.pts));
                // A unit that never produced a picture would otherwise shift every later one.
                while owed.len() > 16 {
                    owed.pop();
                }
            }
        }
        let mut out = Vec::new();
        while let Some(p) = self.dec.next_picture() {
            let pts = match &mut self.owed {
                Some(owed) => owed.pop().map_or(sample.pts, |r| r.0),
                None => track
                    .samples
                    .get(p.timestamp as usize)
                    .map_or(sample.pts, |s| s.pts),
            };
            if let Some(f) = frame_of(p, pts, track.rotation) {
                out.push(f);
            }
        }
        out
    }
}

/// What the decode thread and the player share.
#[derive(Default)]
struct Shared {
    /// Decoded pictures not yet shown, by presentation time.
    frames: VecDeque<Frame>,
    /// Every sample was fed (since the last seek).
    ended: bool,
    seek: Option<Duration>,
    stop: bool,
}

type Link = Arc<(Mutex<Shared>, Condvar)>;

fn lock(link: &Link) -> std::sync::MutexGuard<'_, Shared> {
    link.0.lock().unwrap_or_else(|e| e.into_inner())
}

fn insert(frames: &mut VecDeque<Frame>, f: Frame) {
    let at = frames.partition_point(|x| x.pts <= f.pts);
    frames.insert(at, f);
}

fn decode_thread(track: Arc<VideoTrack>, link: Link) {
    let mut feeder: Option<Feeder> = None;
    let mut next = 0usize;
    // After a seek: pictures before the target are skipped, all but the last one before it.
    let mut target: Option<Duration> = None;
    let mut held: Option<Frame> = None;
    loop {
        {
            let mut st = lock(&link);
            loop {
                if st.stop {
                    return;
                }
                if let Some(to) = st.seek.take() {
                    st.frames.clear();
                    st.ended = false;
                    next = track.key_before(to);
                    feeder = Feeder::new(&track).ok();
                    target = (!to.is_zero()).then_some(to);
                    held = None;
                }
                if next >= track.samples.len() || feeder.is_none() {
                    if let Some(h) = held.take() {
                        insert(&mut st.frames, h);
                    }
                    st.ended = true;
                    link.1.notify_all();
                    st = link.1.wait(st).unwrap_or_else(|e| e.into_inner());
                    continue;
                }
                if st.frames.len() >= AHEAD {
                    st = link.1.wait(st).unwrap_or_else(|e| e.into_inner());
                    continue;
                }
                break;
            }
        }
        let Some(f) = feeder.as_mut() else { continue };
        let out = f.feed(&track, next);
        next += 1;
        let mut st = lock(&link);
        if st.seek.is_some() {
            continue;
        }
        for f in out {
            match target {
                Some(t) if f.pts < t => held = Some(f),
                _ => {
                    target = None;
                    if let Some(h) = held.take() {
                        insert(&mut st.frames, h);
                    }
                    insert(&mut st.frames, f);
                }
            }
        }
    }
}

/// One playing video: its decode thread, its sound and its clock. Dropping it stops both.
pub struct VideoPlayback {
    link: Link,
    thread: Option<JoinHandle<()>>,
    audio: Option<Playback>,
    /// The clock: the position at `since`, running while `since` is set.
    base: Duration,
    since: Option<Instant>,
    duration: Option<Duration>,
    shown: Frame,
    volume: f32,
    muted: bool,
}

impl Drop for VideoPlayback {
    fn drop(&mut self) {
        lock(&self.link).stop = true;
        self.link.1.notify_all();
        if let Some(t) = self.thread.take() {
            let _ = t.join();
        }
    }
}

impl VideoPlayback {
    /// Starts `video` from its beginning, playing; its sound on the default output device (no device: it plays mute).
    pub fn start(video: OpenedVideo) -> Self {
        let audio = video.audio.and_then(|d| Playback::start(d).ok());
        Self::with_audio(video.track, video.poster, audio)
    }

    /// The same, its sound on a detached player the caller pulls (a test, or a host without an output device).
    pub fn start_detached(video: OpenedVideo) -> (Self, Option<rodio::queue::SourcesQueueOutput>) {
        let (audio, out) = match video.audio.map(Playback::start_detached) {
            Some(Ok((p, out))) => (Some(p), Some(out)),
            _ => (None, None),
        };
        (Self::with_audio(video.track, video.poster, audio), out)
    }

    fn with_audio(track: Arc<VideoTrack>, poster: Frame, audio: Option<Playback>) -> Self {
        let link: Link = Arc::new((
            Mutex::new(Shared {
                seek: Some(Duration::ZERO),
                ..Shared::default()
            }),
            Condvar::new(),
        ));
        let duration = track.duration.or(audio.as_ref().and_then(|a| a.duration()));
        let thread = {
            let link = link.clone();
            std::thread::Builder::new()
                .name("dilla-video".to_owned())
                .spawn(move || decode_thread(track, link))
                .ok()
        };
        Self {
            link,
            thread,
            audio,
            base: Duration::ZERO,
            since: Some(Instant::now()),
            duration,
            shown: poster,
            volume: 1.0,
            muted: false,
        }
    }

    pub fn duration(&self) -> Option<Duration> {
        self.duration
    }

    /// Where the clock is, never past the end.
    pub fn position(&self) -> Duration {
        let p = self.base + self.since.map_or(Duration::ZERO, |s| s.elapsed());
        self.duration.map_or(p, |d| p.min(d))
    }

    pub fn is_paused(&self) -> bool {
        self.since.is_none()
    }

    /// Whether it played to its end: the clock reached the duration, or (no duration known) every picture was shown.
    pub fn ended(&self) -> bool {
        match self.duration {
            Some(d) => self.position() >= d,
            None => {
                let st = lock(&self.link);
                st.ended && st.frames.is_empty() && self.position() >= self.shown.pts
            }
        }
    }

    pub fn play(&mut self) {
        if self.ended() {
            self.seek(Duration::ZERO);
        }
        if self.since.is_none() {
            self.since = Some(Instant::now());
        }
        if let Some(a) = &self.audio {
            a.play();
        }
    }

    pub fn pause(&mut self) {
        self.base = self.position();
        self.since = None;
        if let Some(a) = &self.audio {
            a.pause();
        }
    }

    /// Moves the clock, the sound and the pictures to `to` (clamped to the duration).
    pub fn seek(&mut self, to: Duration) {
        let to = self.duration.map_or(to, |d| to.min(d));
        self.base = to;
        if self.since.is_some() {
            self.since = Some(Instant::now());
        }
        if let Some(a) = &self.audio {
            let _ = a.seek(to);
        }
        let mut st = lock(&self.link);
        st.seek = Some(to);
        st.frames.clear();
        st.ended = false;
        self.link.1.notify_all();
    }

    pub fn has_audio(&self) -> bool {
        self.audio.is_some()
    }

    /// 0.0 to 1.0; a volume above 0 unmutes.
    pub fn set_volume(&mut self, v: f32) {
        self.volume = v.clamp(0.0, 1.0);
        self.muted = self.volume == 0.0;
        self.apply_volume();
    }

    pub fn volume(&self) -> f32 {
        self.volume
    }

    pub fn muted(&self) -> bool {
        self.muted
    }

    pub fn toggle_mute(&mut self) {
        self.muted = !self.muted;
        if !self.muted && self.volume == 0.0 {
            self.volume = 1.0;
        }
        self.apply_volume();
    }

    fn apply_volume(&self) {
        if let Some(a) = &self.audio {
            a.set_volume(if self.muted { 0.0 } else { self.volume });
        }
    }

    /// The picture on screen.
    pub fn frame(&self) -> &Frame {
        &self.shown
    }

    /// Brings the shown picture up to the clock: the newest decoded picture due replaces it. Answers the images that
    /// left the screen (their GPU textures go with `App::drop_image`); empty when nothing changed.
    pub fn tick(&mut self) -> Vec<Arc<RenderImage>> {
        let now = self.position();
        let mut gone = Vec::new();
        let mut st = lock(&self.link);
        let mut took = false;
        while st.frames.front().is_some_and(|f| f.pts <= now) {
            let f = st.frames.pop_front().expect("checked");
            gone.push(std::mem::replace(&mut self.shown, f).image);
            took = true;
        }
        if took {
            // Room for the thread to decode ahead again.
            self.link.1.notify_all();
        }
        gone
    }

    /// The decode thread's queue, for the tests: how many pictures wait and whether every sample was fed.
    #[cfg(test)]
    fn queued(&self) -> (usize, bool) {
        let st = lock(&self.link);
        (st.frames.len(), st.ended)
    }
}

#[cfg(test)]
mod tests;
