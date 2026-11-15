//! The inline media player's decoding (Jonas 2026-11-12: audio first with symphonia and rodio, no FFmpeg). Playback
//! goes behind [`MediaDecoder`], so the later Rust video stack adds a decoder without touching the card.
//!
//! - Containers and codecs: symphonia (MP3, AAC in MP4, Vorbis and Opus in Ogg, FLAC, WAV, and MKV/WebM demux).
//! - Opus: symphonia 0.5 has no Opus decoder; packets go to `opus-pure`, a pure-Rust port of libopus 1.x
//!   (BSD-3-Clause, no FFI), so nothing links a second libopus beside the one in libwebrtc (Task 12, path (a) of
//!   the controller's ruling).
//! - Output: rodio's [`Player`] (play, pause, seek, volume) on the default device.
//! - Video: not decoded here. A video card shows its poster and opens the file in the system player
//!   ([`write_for_system_player`]).
//!
//! The decrypted bytes stay in memory: the decoder reads them through a cursor over the [`Zeroizing`] buffer, and
//! nothing is written to disk, except a video handed to the system player (see there).

use std::io::Cursor;
use std::num::NonZero;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use symphonia::core::audio::SampleBuffer;
use symphonia::core::codecs::{CODEC_TYPE_NULL, CODEC_TYPE_OPUS, Decoder, DecoderOptions};
use symphonia::core::formats::{FormatOptions, FormatReader, SeekMode, SeekTo};
use symphonia::core::io::{MediaSourceStream, MediaSourceStreamOptions};
use symphonia::core::meta::MetadataOptions;
use symphonia::core::probe::Hint;
use symphonia::core::units::Time;
use zeroize::Zeroizing;

/// Why a file does not play.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum MediaError {
    /// No container or codec here reads it.
    Unsupported(String),
    /// It decodes wrongly (a corrupt or truncated file).
    Decode(String),
    /// No output device.
    Output(String),
}

/// One playable stream: interleaved f32 samples at a fixed rate and channel count. A video decoder (a later plan) will
/// implement it for the video's audio and add its frames beside it.
pub trait MediaDecoder: Send {
    fn channels(&self) -> u16;
    fn sample_rate(&self) -> u32;
    fn duration(&self) -> Option<Duration>;
    /// The next block of interleaved samples; `None` at the end.
    fn next_block(&mut self) -> Option<Vec<f32>>;
    /// Moves to `to` (to the packet at or before it); the next block starts there.
    fn seek(&mut self, to: Duration) -> Result<(), MediaError>;
}

/// The decrypted file, shared without copying and zeroized when the last holder drops it.
#[derive(Clone)]
struct Bytes(Arc<Zeroizing<Vec<u8>>>);

impl AsRef<[u8]> for Bytes {
    fn as_ref(&self) -> &[u8] {
        &self.0
    }
}

/// The packet decoder: symphonia's own, or Opus.
enum Codec {
    Symphonia(Box<dyn Decoder>),
    Opus {
        dec: Box<opus_pure::OpusDecoder>,
        channels: usize,
        /// The OpusHead pre-skip still to drop (in samples per channel at 48 kHz).
        skip: usize,
        pre_skip: usize,
    },
}

/// [`MediaDecoder`] over symphonia's demuxers.
pub struct AudioDecoder {
    format: Box<dyn FormatReader>,
    track: u32,
    codec: Codec,
    channels: u16,
    rate: u32,
    duration: Option<Duration>,
    /// A block decoded while opening (to learn a stream's layout its header does not state), played first.
    pending: Option<Vec<f32>>,
}

/// The longest duration shown; a header claiming more says nothing believable, so the duration is unknown.
pub const MAX_DURATION: Duration = Duration::from_secs(24 * 60 * 60);
/// The most channels and the highest rate played; a header beyond them is refused as unplayable.
const MAX_CHANNELS: u16 = 8;
const MAX_RATE: u32 = 384_000;

/// A stream's duration from its header's frame count and rate: `None` (unknown) for a missing, non-finite or absurd
/// value, never a panic on a forged header.
pub fn duration_of(n_frames: Option<u64>, rate: Option<u32>) -> Option<Duration> {
    let (n, rate) = (n_frames?, rate?);
    if rate == 0 {
        return None;
    }
    Duration::try_from_secs_f64(n as f64 / f64::from(rate))
        .ok()
        .filter(|d| *d <= MAX_DURATION)
}

/// Runs a decoder call so that a panic inside a codec (a hostile file found a bug) is "cannot play", never a crash.
fn guarded<T>(f: impl FnOnce() -> T) -> Option<T> {
    std::panic::catch_unwind(std::panic::AssertUnwindSafe(f)).ok()
}

/// The OpusHead (RFC 7845 §5.1) fields a decoder needs: channels and pre-skip.
fn opus_head(extra: Option<&[u8]>) -> Option<(usize, usize)> {
    let h = extra?;
    if h.len() < 19 || &h[0..8] != b"OpusHead" {
        return None;
    }
    let channels = usize::from(h[9]);
    let pre_skip = usize::from(u16::from_le_bytes([h[10], h[11]]));
    (channels == 1 || channels == 2).then_some((channels, pre_skip))
}

/// Opens `bytes` (a decrypted audio attachment, typed by `mime`) for playback. Every probe, codec or header failure,
/// every absurd header value and every codec panic is [`MediaError::Unsupported`]: the card shows "cannot play".
pub fn open_audio(bytes: Arc<Zeroizing<Vec<u8>>>, mime: &str) -> Result<AudioDecoder, MediaError> {
    guarded(move || open_audio_inner(bytes, mime))
        .unwrap_or_else(|| Err(MediaError::Unsupported("the decoder failed".to_owned())))
}

fn open_audio_inner(
    bytes: Arc<Zeroizing<Vec<u8>>>,
    mime: &str,
) -> Result<AudioDecoder, MediaError> {
    let source = MediaSourceStream::new(
        Box::new(Cursor::new(Bytes(bytes))),
        MediaSourceStreamOptions::default(),
    );
    let mut hint = Hint::new();
    hint.mime_type(mime);
    let probed = symphonia::default::get_probe()
        .format(
            &hint,
            source,
            &FormatOptions {
                enable_gapless: true,
                ..FormatOptions::default()
            },
            &MetadataOptions::default(),
        )
        .map_err(|e| MediaError::Unsupported(e.to_string()))?;
    let format = probed.format;
    let track = format
        .tracks()
        .iter()
        .find(|t| t.codec_params.codec != CODEC_TYPE_NULL)
        .ok_or_else(|| MediaError::Unsupported("no audio track".to_owned()))?;
    let params = track.codec_params.clone();
    let track_id = track.id;
    let duration = duration_of(params.n_frames, params.sample_rate);
    let (codec, channels, rate) = if params.codec == CODEC_TYPE_OPUS {
        let (channels, pre_skip) = opus_head(params.extra_data.as_deref()).ok_or_else(|| {
            MediaError::Unsupported("an Opus track without its header".to_owned())
        })?;
        let dec = opus_pure::OpusDecoder::new(48_000, channels)
            .map(Box::new)
            .map_err(|e| MediaError::Unsupported(e.to_string()))?;
        (
            Codec::Opus {
                dec,
                channels,
                skip: pre_skip,
                pre_skip,
            },
            channels as u16,
            48_000,
        )
    } else {
        let dec = symphonia::default::get_codecs()
            .make(&params, &DecoderOptions::default())
            .map_err(|e| MediaError::Unsupported(e.to_string()))?;
        let channels = params.channels.map_or(0, |c| c.count() as u16);
        let rate = params.sample_rate.unwrap_or(0);
        (Codec::Symphonia(dec), channels, rate)
    };
    let mut d = AudioDecoder {
        format,
        track: track_id,
        codec,
        channels,
        rate,
        duration,
        pending: None,
    };
    if d.channels == 0 || d.rate == 0 {
        // Some containers (MP3, AAC in MP4) state the layout only in the first frame: decode it to learn it.
        let (block, channels, rate) = d
            .raw_next()
            .ok_or_else(|| MediaError::Unsupported("no channel count or rate".to_owned()))?;
        d.channels = channels;
        d.rate = rate;
        d.pending = Some(block);
    }
    if d.channels == 0 || d.channels > MAX_CHANNELS || d.rate == 0 || d.rate > MAX_RATE {
        return Err(MediaError::Unsupported(
            "an unplayable channel count or rate".to_owned(),
        ));
    }
    Ok(d)
}

impl MediaDecoder for AudioDecoder {
    fn channels(&self) -> u16 {
        self.channels
    }

    fn sample_rate(&self) -> u32 {
        self.rate
    }

    fn duration(&self) -> Option<Duration> {
        self.duration
    }

    fn next_block(&mut self) -> Option<Vec<f32>> {
        if let Some(b) = self.pending.take() {
            return Some(b);
        }
        let (channels, rate) = (self.channels, self.rate);
        // A codec panic, or a block whose layout changed mid-stream (the output cannot follow it), ends playback.
        let (block, c, r) = guarded(|| self.raw_next()).flatten()?;
        (c == channels && r == rate).then_some(block)
    }

    fn seek(&mut self, to: Duration) -> Result<(), MediaError> {
        self.pending = None;
        // A demuxer or codec panic while seeking (symphonia's MP3 seek panics on some streams) is a refused seek.
        guarded(|| self.seek_inner(to))
            .unwrap_or_else(|| Err(MediaError::Decode("the seek failed".to_owned())))
    }
}

impl AudioDecoder {
    /// The next block, with the layout it was decoded in.
    fn raw_next(&mut self) -> Option<(Vec<f32>, u16, u32)> {
        loop {
            let packet = self.format.next_packet().ok()?;
            if packet.track_id() != self.track {
                continue;
            }
            match &mut self.codec {
                Codec::Symphonia(dec) => {
                    let Ok(decoded) = dec.decode(&packet) else {
                        // A damaged packet is skipped, as a browser's decoder skips it.
                        continue;
                    };
                    if decoded.frames() == 0 {
                        continue;
                    }
                    let spec = *decoded.spec();
                    let mut buf = SampleBuffer::<f32>::new(decoded.capacity() as u64, spec);
                    buf.copy_interleaved_ref(decoded);
                    return Some((
                        buf.samples().to_vec(),
                        spec.channels.count() as u16,
                        spec.rate,
                    ));
                }
                Codec::Opus {
                    dec,
                    channels,
                    skip,
                    ..
                } => {
                    // 120 ms at 48 kHz is the longest Opus packet.
                    let mut pcm = vec![0f32; 5760 * *channels];
                    let Ok(n) = dec.decode(&packet.data, 5760, &mut pcm) else {
                        continue;
                    };
                    let n = n.min(5760);
                    let drop = (*skip).min(n);
                    *skip -= drop;
                    if drop == n {
                        continue;
                    }
                    pcm.truncate(n * *channels);
                    pcm.drain(..drop * *channels);
                    return Some((pcm, *channels as u16, 48_000));
                }
            }
        }
    }

    fn seek_inner(&mut self, to: Duration) -> Result<(), MediaError> {
        self.format
            .seek(
                SeekMode::Coarse,
                SeekTo::Time {
                    time: Time::new(to.as_secs(), f64::from(to.subsec_nanos()) / 1e9),
                    track_id: Some(self.track),
                },
            )
            .map_err(|e| MediaError::Decode(e.to_string()))?;
        match &mut self.codec {
            Codec::Symphonia(dec) => dec.reset(),
            Codec::Opus {
                dec,
                skip,
                pre_skip,
                ..
            } => {
                let _ = dec.reset_state();
                // From the start, the pre-skip is dropped again; elsewhere the decoder's own warm-up suffices.
                *skip = if to.is_zero() { *pre_skip } else { 0 };
            }
        }
        Ok(())
    }
}

/// A [`MediaDecoder`] as a rodio source.
struct Feed {
    decoder: Box<dyn MediaDecoder>,
    block: Vec<f32>,
    at: usize,
    channels: NonZero<u16>,
    rate: NonZero<u32>,
}

impl Iterator for Feed {
    type Item = f32;
    fn next(&mut self) -> Option<f32> {
        while self.at >= self.block.len() {
            self.block = self.decoder.next_block()?;
            self.at = 0;
        }
        let s = self.block[self.at];
        self.at += 1;
        Some(s)
    }
}

impl rodio::Source for Feed {
    fn current_span_len(&self) -> Option<usize> {
        None
    }
    fn channels(&self) -> rodio::ChannelCount {
        self.channels
    }
    fn sample_rate(&self) -> rodio::SampleRate {
        self.rate
    }
    fn total_duration(&self) -> Option<Duration> {
        self.decoder.duration()
    }
    fn try_seek(&mut self, pos: Duration) -> Result<(), rodio::source::SeekError> {
        self.decoder
            .seek(pos)
            .map_err(|_| rodio::source::SeekError::NotSupported {
                underlying_source: "dilla attachment audio",
            })?;
        self.block.clear();
        self.at = 0;
        Ok(())
    }
}

/// One playing attachment: the device stream and its player. Dropping it stops the sound.
pub struct Playback {
    // Held for the player's life: dropping the device sink silences it.
    _sink: Option<rodio::MixerDeviceSink>,
    player: rodio::Player,
    duration: Option<Duration>,
}

fn feed(decoder: Box<dyn MediaDecoder>) -> Result<Feed, MediaError> {
    let channels = NonZero::new(decoder.channels())
        .ok_or_else(|| MediaError::Unsupported("no channels".to_owned()))?;
    let rate = NonZero::new(decoder.sample_rate())
        .ok_or_else(|| MediaError::Unsupported("no rate".to_owned()))?;
    Ok(Feed {
        decoder,
        block: Vec::new(),
        at: 0,
        channels,
        rate,
    })
}

impl Playback {
    /// Starts `decoder` on the default output device.
    pub fn start(decoder: Box<dyn MediaDecoder>) -> Result<Self, MediaError> {
        let duration = decoder.duration();
        let source = feed(decoder)?;
        let sink = rodio::DeviceSinkBuilder::open_default_sink()
            .map_err(|e| MediaError::Output(e.to_string()))?;
        let player = rodio::Player::connect_new(sink.mixer());
        player.append(source);
        Ok(Self {
            _sink: Some(sink),
            player,
            duration,
        })
    }

    /// The same player detached from any device: its output is the answered source, which the caller pulls (a test,
    /// or a host without an output device).
    pub fn start_detached(
        decoder: Box<dyn MediaDecoder>,
    ) -> Result<(Self, rodio::queue::SourcesQueueOutput), MediaError> {
        let duration = decoder.duration();
        let source = feed(decoder)?;
        let (player, out) = rodio::Player::new();
        player.append(source);
        Ok((
            Self {
                _sink: None,
                player,
                duration,
            },
            out,
        ))
    }

    pub fn play(&self) {
        self.player.play();
    }
    pub fn pause(&self) {
        self.player.pause();
    }
    pub fn is_paused(&self) -> bool {
        self.player.is_paused()
    }
    /// Whether the file played to its end.
    pub fn ended(&self) -> bool {
        self.player.empty()
    }
    pub fn position(&self) -> Duration {
        self.player.get_pos()
    }
    pub fn duration(&self) -> Option<Duration> {
        self.duration
    }
    pub fn seek(&self, to: Duration) -> Result<(), MediaError> {
        self.player
            .try_seek(to)
            .map_err(|e| MediaError::Decode(e.to_string()))
    }
    /// 0.0 to 1.0.
    pub fn set_volume(&self, v: f32) {
        self.player.set_volume(v.clamp(0.0, 1.0));
    }
    pub fn volume(&self) -> f32 {
        self.player.volume()
    }
}

/// The directory a video handed to the system player is written to: the instance's own data directory
/// (`<instance>/media-open`), never a shared temp dir. [`wipe_open_dir`] empties it at start and at exit.
pub fn open_dir(instance_dir: &Path) -> PathBuf {
    instance_dir.join("media-open")
}

/// Removes every file handed to the system player (at start, in case the app did not exit cleanly, and at exit).
pub fn wipe_open_dir(instance_dir: &Path) {
    let _ = std::fs::remove_dir_all(open_dir(instance_dir));
}

/// A container's file extension and its signature check.
type Container = (&'static str, fn(&[u8]) -> bool);

/// The video containers handed to the system player, by exact MIME type: the web client's playable videos
/// (derive.ts `PLAYABLE_VIDEO`) and the two phone formats a person most often sends. Anything else is refused.
fn video_container(mime: &str) -> Option<Container> {
    fn iso_bmff(b: &[u8]) -> bool {
        b.len() >= 12 && &b[4..8] == b"ftyp"
    }
    fn quicktime(b: &[u8]) -> bool {
        b.len() >= 8
            && matches!(
                &b[4..8],
                b"ftyp" | b"moov" | b"mdat" | b"wide" | b"free" | b"skip"
            )
    }
    fn ebml(b: &[u8]) -> bool {
        b.starts_with(&[0x1a, 0x45, 0xdf, 0xa3])
    }
    fn ogg(b: &[u8]) -> bool {
        b.starts_with(b"OggS")
    }
    match mime {
        "video/mp4" => Some(("mp4", iso_bmff)),
        "video/quicktime" => Some(("mov", quicktime)),
        "video/webm" => Some(("webm", ebml)),
        "video/x-matroska" => Some(("mkv", ebml)),
        "video/ogg" => Some(("ogv", ogg)),
        _ => None,
    }
}

/// 128 random bits from the OS (`getrandom`) as 32 hex digits: nothing of the sender's.
fn random_name() -> std::io::Result<String> {
    let mut b = [0u8; 16];
    getrandom::fill(&mut b).map_err(|e| std::io::Error::other(e.to_string()))?;
    Ok(b.iter().map(|x| format!("{x:02x}")).collect())
}

/// Marks a written file as downloaded from the internet, so the OS asks before running anything from it: the
/// `Zone.Identifier` stream with `ZoneId=3` on Windows, the `com.apple.quarantine` attribute on macOS. Linux has no
/// such mark; there the random name and the checked container extension are the protection.
fn mark_downloaded(path: &Path) {
    #[cfg(windows)]
    {
        let mut ads = path.as_os_str().to_owned();
        ads.push(":Zone.Identifier");
        let _ = std::fs::write(ads, "[ZoneTransfer]\r\nZoneId=3\r\n");
    }
    #[cfg(target_os = "macos")]
    {
        let stamp = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |d| d.as_secs());
        let _ = std::process::Command::new("/usr/bin/xattr")
            .arg("-w")
            .arg("com.apple.quarantine")
            .arg(format!("0081;{stamp:x};Dilla Preview;"))
            .arg(path)
            .status();
    }
    #[cfg(not(any(windows, target_os = "macos")))]
    let _ = path;
}

/// Writes a decrypted video for the system player, which needs a file (security review, Task 12):
///
/// - only an allowed video type (`video_container`) whose bytes start with that container's signature is written;
///   anything else is refused, so no executable, script or launcher ever reaches the OS's opener;
/// - the file's name is random and its extension the container's: nothing of the sender's file name is used;
/// - it goes into the instance's own `media-open` directory (owner-only on Unix), marked as downloaded, and stays
///   until [`wipe_open_dir`] runs at start and at exit (the player may still be reading it).
///
/// The caller opens the answered path with the OS (`App::open_with_system`).
pub fn write_for_system_player(
    instance_dir: &Path,
    mime: &str,
    bytes: &[u8],
) -> std::io::Result<PathBuf> {
    let refuse = |why: &str| std::io::Error::new(std::io::ErrorKind::InvalidData, why.to_owned());
    let (ext, matches) =
        video_container(mime).ok_or_else(|| refuse("not a video type the system player gets"))?;
    if !matches(bytes) {
        return Err(refuse("the file is not the container its type names"));
    }
    let dir = open_dir(instance_dir);
    std::fs::create_dir_all(&dir)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700))?;
    }
    let path = dir.join(format!("{}.{ext}", random_name()?));
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&path)?;
    std::io::Write::write_all(&mut file, bytes)?;
    drop(file);
    mark_downloaded(&path);
    Ok(path)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn decode_all(bytes: &[u8], mime: &str) -> (AudioDecoder, Vec<f32>) {
        let mut d = open_audio(Arc::new(Zeroizing::new(bytes.to_vec())), mime).expect("opens");
        let mut all = Vec::new();
        while let Some(b) = d.next_block() {
            all.extend(b);
        }
        (d, all)
    }

    /// The 440 Hz test tones (1 s, mono) carry energy: the decoder produced the tone, not silence.
    fn rms(s: &[f32]) -> f32 {
        (s.iter().map(|x| x * x).sum::<f32>() / s.len().max(1) as f32).sqrt()
    }

    fn wav(seconds: f32) -> Vec<u8> {
        let rate = 8000u32;
        let n = (rate as f32 * seconds) as u32;
        let mut out = Vec::new();
        out.extend_from_slice(b"RIFF");
        out.extend_from_slice(&(36 + n * 2).to_le_bytes());
        out.extend_from_slice(b"WAVEfmt ");
        out.extend_from_slice(&16u32.to_le_bytes());
        out.extend_from_slice(&1u16.to_le_bytes());
        out.extend_from_slice(&1u16.to_le_bytes());
        out.extend_from_slice(&rate.to_le_bytes());
        out.extend_from_slice(&(rate * 2).to_le_bytes());
        out.extend_from_slice(&2u16.to_le_bytes());
        out.extend_from_slice(&16u16.to_le_bytes());
        out.extend_from_slice(b"data");
        out.extend_from_slice(&(n * 2).to_le_bytes());
        for i in 0..n {
            let v =
                ((i as f32 * 440.0 * std::f32::consts::TAU / rate as f32).sin() * 16000.0) as i16;
            out.extend_from_slice(&v.to_le_bytes());
        }
        out
    }

    #[test]
    fn every_audio_format_of_the_task_decodes_to_the_tone() {
        for (bytes, mime, rate) in [
            (wav(1.0), "audio/wav", 8000),
            (
                include_bytes!("testdata/tone-440.mp3").to_vec(),
                "audio/mpeg",
                44_100,
            ),
            (
                include_bytes!("testdata/tone-440.m4a").to_vec(),
                "audio/mp4",
                44_100,
            ),
            (
                include_bytes!("testdata/tone-440.ogg").to_vec(),
                "audio/ogg",
                44_100,
            ),
            (
                include_bytes!("testdata/tone-440.flac").to_vec(),
                "audio/flac",
                44_100,
            ),
            (
                include_bytes!("testdata/tone-440.opus").to_vec(),
                "audio/ogg",
                48_000,
            ),
        ] {
            let (d, samples) = decode_all(&bytes, mime);
            assert_eq!((d.channels(), d.sample_rate()), (1, rate), "{mime}");
            assert!(
                samples.len() as u32 >= rate / 3,
                "{mime}: {} samples",
                samples.len()
            );
            assert!(rms(&samples) > 0.05, "{mime}: rms {}", rms(&samples));
        }
    }

    /// Opus through `opus-pure`: one second at 48 kHz less the OpusHead pre-skip, within one 20 ms packet.
    #[test]
    fn opus_drops_its_pre_skip_and_keeps_the_length() {
        let (_, samples) = decode_all(include_bytes!("testdata/tone-440.opus"), "audio/opus");
        let n = samples.len() as i64;
        assert!((n - 48_000).abs() <= 960 + 312, "{n} samples");
    }

    #[test]
    fn a_seek_moves_to_the_middle_and_back_to_the_start() {
        let bytes = Arc::new(Zeroizing::new(
            include_bytes!("testdata/tone-440.ogg").to_vec(),
        ));
        let mut d = open_audio(bytes, "audio/ogg").unwrap();
        d.seek(Duration::from_millis(500)).unwrap();
        let rest: usize = std::iter::from_fn(|| d.next_block()).map(|b| b.len()).sum();
        assert!(
            rest < 44_100 * 3 / 4 && rest > 44_100 / 4,
            "{rest} samples after 0.5 s"
        );
        d.seek(Duration::ZERO).unwrap();
        let all: usize = std::iter::from_fn(|| d.next_block()).map(|b| b.len()).sum();
        assert!(all > 44_100 * 3 / 4, "{all} samples from the start");
    }

    #[test]
    fn bytes_that_are_no_audio_are_unsupported() {
        let r = open_audio(
            Arc::new(Zeroizing::new(b"not audio at all".to_vec())),
            "audio/mpeg",
        );
        assert!(matches!(r, Err(MediaError::Unsupported(_))));
    }

    #[test]
    fn a_video_file_name_stays_inside_the_instance_directory() {
        let dir = Path::new("/data/instances/0");
        assert_eq!(open_dir(dir), Path::new("/data/instances/0/media-open"));
    }

    fn scratch(tag: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("dilla-t12-media-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    const MP4: &[u8] = b"\0\0\0\x18ftypmp42\0\0\0\0mp42isom";

    /// Security review (HIGH): the sender's name never reaches the path; whatever it says, the file is a random name
    /// with the extension of its checked container type, inside the instance's own directory.
    #[test]
    fn a_hostile_name_never_reaches_the_path_given_to_the_system_player() {
        let dir = scratch("names");
        let mut seen = std::collections::HashSet::new();
        for hostile in [
            "evil.exe",
            "x.desktop",
            "../../a.mp4",
            "run.command",
            "a.lnk",
            "s.scr",
        ] {
            let path = write_for_system_player(&dir, "video/mp4", MP4).expect("an MP4 is written");
            assert_eq!(path.parent(), Some(open_dir(&dir).as_path()), "{hostile}");
            assert_eq!(path.extension().and_then(|e| e.to_str()), Some("mp4"));
            let stem = path.file_stem().unwrap().to_str().unwrap();
            assert!(
                stem.len() == 32 && stem.bytes().all(|b| b.is_ascii_hexdigit()),
                "{stem}"
            );
            assert!(seen.insert(path.clone()), "a fresh name each time");
        }
        wipe_open_dir(&dir);
        assert!(!open_dir(&dir).exists());
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_container_that_does_not_match_its_type_is_refused() {
        let dir = scratch("magic");
        // An executable, a script and a WebM claiming to be MP4s; an MP4 claiming to be a WebM.
        for (mime, bytes) in [
            ("video/mp4", &b"MZ\x90\0\x03\0\0\0\x04\0\0\0\xff\xff"[..]),
            ("video/mp4", b"#!/bin/sh\nrm -rf ~\n"),
            ("video/mp4", b"\x1a\x45\xdf\xa3\x01\0\0\0"),
            ("video/webm", MP4),
            ("video/mp4", b""),
        ] {
            assert!(
                write_for_system_player(&dir, mime, bytes).is_err(),
                "{mime} {bytes:?}"
            );
        }
        assert!(
            !open_dir(&dir).exists() || std::fs::read_dir(open_dir(&dir)).unwrap().next().is_none()
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn an_unknown_video_type_is_refused() {
        let dir = scratch("mime");
        for mime in [
            "application/x-msdownload",
            "video/x-ms-wmv",
            "text/x-desktop",
            "",
            "video/mp4; x=1",
        ] {
            assert!(write_for_system_player(&dir, mime, MP4).is_err(), "{mime}");
        }
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Security review (MEDIUM): a forged header's frame count never panics the duration.
    #[test]
    fn an_absurd_duration_is_unknown_not_a_panic() {
        assert_eq!(duration_of(Some(u64::MAX), Some(1)), None);
        assert_eq!(
            duration_of(Some(u64::MAX), Some(u32::MAX)),
            None,
            "over 24 h"
        );
        assert_eq!(duration_of(Some(10), Some(0)), None);
        assert_eq!(
            duration_of(Some(88_200), Some(44_100)),
            Some(Duration::from_secs(2))
        );
        assert_eq!(duration_of(None, Some(44_100)), None);
    }

    /// Every truncation of every test file, and garbage, is "cannot play" or plays: never a panic.
    #[test]
    fn truncated_and_garbage_audio_never_panics() {
        let files: [&[u8]; 5] = [
            include_bytes!("testdata/tone-440.mp3"),
            include_bytes!("testdata/tone-440.m4a"),
            include_bytes!("testdata/tone-440.ogg"),
            include_bytes!("testdata/tone-440.flac"),
            include_bytes!("testdata/tone-440.opus"),
        ];
        let mut corpus: Vec<Vec<u8>> = Vec::new();
        for f in files {
            for cut in [
                0,
                1,
                4,
                12,
                44,
                64,
                200,
                f.len() / 3,
                f.len() / 2,
                f.len() - 1,
            ] {
                corpus.push(f[..cut.min(f.len())].to_vec());
            }
            // The same file with a stretch of its body flipped.
            let mut flipped = f.to_vec();
            for b in flipped.iter_mut().skip(f.len() / 4).take(f.len() / 4) {
                *b ^= 0x5a;
            }
            corpus.push(flipped);
        }
        let mut seed = 0x2545_f491_u32;
        for len in [16, 512, 4096] {
            corpus.push(
                (0..len)
                    .map(|_| {
                        seed ^= seed << 13;
                        seed ^= seed >> 17;
                        seed ^= seed << 5;
                        seed as u8
                    })
                    .collect(),
            );
        }
        let mut wav_header = wav(0.1);
        wav_header[22..24].copy_from_slice(&u16::MAX.to_le_bytes());
        wav_header[24..28].copy_from_slice(&u32::MAX.to_le_bytes());
        corpus.push(wav_header);
        for (i, bytes) in corpus.into_iter().enumerate() {
            for mime in ["audio/mpeg", "audio/ogg", "audio/wav"] {
                if let Ok(mut d) = open_audio(Arc::new(Zeroizing::new(bytes.clone())), mime) {
                    assert!(
                        d.channels() >= 1 && d.channels() <= 8 && d.sample_rate() > 0,
                        "#{i}"
                    );
                    let mut blocks = 0;
                    while d.next_block().is_some() && blocks < 10_000 {
                        blocks += 1;
                    }
                    // Seeks anywhere, the absurd included, are an answer, never a panic; decoding goes on after.
                    for to in [
                        Duration::ZERO,
                        Duration::from_millis(500),
                        Duration::from_secs(3600),
                        Duration::from_secs(u64::MAX),
                        Duration::MAX,
                    ] {
                        let _ = d.seek(to);
                        let _ = d.next_block();
                    }
                }
            }
        }
    }
}
