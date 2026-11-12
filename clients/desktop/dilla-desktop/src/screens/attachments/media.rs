//! The inline media player's decoding (Jonas 2026-11-12: audio first with symphonia and rodio, no FFmpeg). Playback
//! goes behind [`MediaDecoder`], so the later Rust video stack adds a decoder without touching the card.
//!
//! - Containers and codecs: symphonia (MP3, AAC in MP4, Vorbis and Opus in Ogg, FLAC, WAV, and MKV/WebM demux).
//! - Opus: symphonia 0.5 has no Opus decoder; packets go to `opus-decoder`, a pure-Rust RFC 8251-conformant decoder
//!   (MIT or Apache-2.0, no FFI), so nothing links a second libopus beside the one in libwebrtc (Task 12, path (a) of
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
        dec: opus_decoder::OpusDecoder,
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

/// Opens `bytes` (a decrypted audio attachment, typed by `mime`) for playback.
pub fn open_audio(
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
    let duration = match (params.n_frames, params.sample_rate) {
        (Some(n), Some(rate)) if rate > 0 => Some(Duration::from_secs_f64(n as f64 / f64::from(rate))),
        _ => None,
    };
    let (codec, channels, rate) = if params.codec == CODEC_TYPE_OPUS {
        let (channels, pre_skip) = opus_head(params.extra_data.as_deref())
            .ok_or_else(|| MediaError::Unsupported("an Opus track without its header".to_owned()))?;
        let dec = opus_decoder::OpusDecoder::new(48_000, channels)
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
    if channels == 0 || rate == 0 {
        return Err(MediaError::Unsupported("no channel count or rate".to_owned()));
    }
    Ok(AudioDecoder {
        format,
        track: track_id,
        codec,
        channels,
        rate,
        duration,
    })
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
                    return Some(buf.samples().to_vec());
                }
                Codec::Opus {
                    dec, channels, skip, ..
                } => {
                    // 120 ms at 48 kHz is the longest Opus packet.
                    let mut pcm = vec![0f32; 5760 * *channels];
                    let Ok(n) = dec.decode_float(&packet.data, &mut pcm, false) else {
                        continue;
                    };
                    let drop = (*skip).min(n);
                    *skip -= drop;
                    if drop == n {
                        continue;
                    }
                    pcm.truncate(n * *channels);
                    pcm.drain(..drop * *channels);
                    return Some(pcm);
                }
            }
        }
    }

    fn seek(&mut self, to: Duration) -> Result<(), MediaError> {
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
                dec, skip, pre_skip, ..
            } => {
                dec.reset();
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
    _sink: rodio::MixerDeviceSink,
    player: rodio::Player,
    duration: Option<Duration>,
}

impl Playback {
    /// Starts `decoder` on the default output device.
    pub fn start(decoder: Box<dyn MediaDecoder>) -> Result<Self, MediaError> {
        let channels = NonZero::new(decoder.channels())
            .ok_or_else(|| MediaError::Unsupported("no channels".to_owned()))?;
        let rate = NonZero::new(decoder.sample_rate())
            .ok_or_else(|| MediaError::Unsupported("no rate".to_owned()))?;
        let duration = decoder.duration();
        let sink = rodio::DeviceSinkBuilder::open_default_sink()
            .map_err(|e| MediaError::Output(e.to_string()))?;
        let player = rodio::Player::connect_new(sink.mixer());
        player.append(Feed {
            decoder,
            block: Vec::new(),
            at: 0,
            channels,
            rate,
        });
        Ok(Self {
            _sink: sink,
            player,
            duration,
        })
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

/// Writes a decrypted video for the system player, which needs a file: into the instance's own `media-open` directory
/// (owner-only on Unix) under the attachment's safe name. The caller opens the answered path with the OS
/// (`App::open_with_system`). The file stays until [`wipe_open_dir`] runs at exit (the player may still be reading it).
pub fn write_for_system_player(
    instance_dir: &Path,
    name: &str,
    bytes: &[u8],
) -> std::io::Result<PathBuf> {
    let dir = open_dir(instance_dir);
    std::fs::create_dir_all(&dir)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700))?;
    }
    let path = dir.join(file_name_for(name));
    std::fs::write(&path, bytes)?;
    Ok(path)
}

/// A name that stays inside the directory: no separator, no leading dot, never empty.
fn file_name_for(name: &str) -> String {
    let clean: String = name
        .chars()
        .map(|c| if matches!(c, '/' | '\\' | ':' | '\0') { '_' } else { c })
        .collect();
    let clean = clean.trim_start_matches('.');
    if clean.is_empty() {
        "video".to_owned()
    } else {
        clean.to_owned()
    }
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
            let v = ((i as f32 * 440.0 * std::f32::consts::TAU / rate as f32).sin() * 16000.0) as i16;
            out.extend_from_slice(&v.to_le_bytes());
        }
        out
    }

    #[test]
    fn every_audio_format_of_the_task_decodes_to_the_tone() {
        for (bytes, mime, rate) in [
            (wav(1.0), "audio/wav", 8000),
            (include_bytes!("testdata/tone-440.mp3").to_vec(), "audio/mpeg", 44_100),
            (include_bytes!("testdata/tone-440.m4a").to_vec(), "audio/mp4", 44_100),
            (include_bytes!("testdata/tone-440.ogg").to_vec(), "audio/ogg", 44_100),
            (include_bytes!("testdata/tone-440.flac").to_vec(), "audio/flac", 44_100),
            (include_bytes!("testdata/tone-440.opus").to_vec(), "audio/ogg", 48_000),
        ] {
            let (d, samples) = decode_all(&bytes, mime);
            assert_eq!((d.channels(), d.sample_rate()), (1, rate), "{mime}");
            assert!(samples.len() as u32 >= rate / 3, "{mime}: {} samples", samples.len());
            assert!(rms(&samples) > 0.1, "{mime}: rms {}", rms(&samples));
        }
    }

    /// Opus through `opus-decoder`: one second at 48 kHz less the OpusHead pre-skip, within one 20 ms packet.
    #[test]
    fn opus_drops_its_pre_skip_and_keeps_the_length() {
        let (_, samples) = decode_all(include_bytes!("testdata/tone-440.opus"), "audio/opus");
        let n = samples.len() as i64;
        assert!((n - 48_000).abs() <= 960 + 312, "{n} samples");
    }

    #[test]
    fn a_seek_moves_to_the_middle_and_back_to_the_start() {
        let bytes = Arc::new(Zeroizing::new(include_bytes!("testdata/tone-440.ogg").to_vec()));
        let mut d = open_audio(bytes, "audio/ogg").unwrap();
        d.seek(Duration::from_millis(500)).unwrap();
        let rest: usize = std::iter::from_fn(|| d.next_block()).map(|b| b.len()).sum();
        assert!(rest < 44_100 * 3 / 4 && rest > 44_100 / 4, "{rest} samples after 0.5 s");
        d.seek(Duration::ZERO).unwrap();
        let all: usize = std::iter::from_fn(|| d.next_block()).map(|b| b.len()).sum();
        assert!(all > 44_100 * 3 / 4, "{all} samples from the start");
    }

    #[test]
    fn bytes_that_are_no_audio_are_unsupported() {
        let r = open_audio(Arc::new(Zeroizing::new(b"not audio at all".to_vec())), "audio/mpeg");
        assert!(matches!(r, Err(MediaError::Unsupported(_))));
    }

    #[test]
    fn a_video_file_name_stays_inside_the_instance_directory() {
        assert_eq!(file_name_for("../../etc/x.mp4"), "_.._etc_x.mp4");
        assert_eq!(file_name_for("..."), "video");
        assert_eq!(file_name_for("clip.mp4"), "clip.mp4");
        let dir = Path::new("/data/instances/0");
        assert_eq!(open_dir(dir), Path::new("/data/instances/0/media-open"));
    }
}
