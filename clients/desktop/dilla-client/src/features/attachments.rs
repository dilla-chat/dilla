//! Area **attachments** (Task 12): encryption through `dilla_core::attachment`, upload with the 25 MB cap, download checked against `size + 16` before decrypting, thumbnails, save after a user action.
//!
//! Ports: packages/client-core/src/attachments/{crypto,download,name,thumb,tray}.ts, and the worker's `attachFiles`,
//! `discardAttachment`, `openAttachment` and `beforeSend` (worker/controller.ts).
//!
//! What never leaves this module: a descriptor's key, nonce and blob id. The published slice
//! ([`crate::state::attachments::AttachmentState`]) names a tray file and its phase only, as `publishTray` does; an
//! opened file is handed to its caller in a [`Zeroizing`] buffer and kept nowhere here.

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use dilla_core::attachment::{
    MAX_THUMB_PLAINTEXT, TAG_LEN, blob_id, open_blob, open_thumb, seal_blob, seal_thumb,
};
use zeroize::Zeroizing;

use crate::client::{Client, ClientError, Inner};
use crate::gateway::frames::Value;
use crate::http::routes::{self as r, NoBody, Raw};
use crate::state::attachments::{TrayItem, TrayPhase};
use crate::state::{ChannelId, MessageId, hex};
use crate::sync::api::{Api, ApiError, with_core};
use crate::sync::value;

/// crypto.ts `BROWSER_ATTACHMENT_CAP`: 25 MiB, the largest file sent or opened.
pub const ATTACHMENT_CAP: u64 = 25 * 1024 * 1024;
/// crypto.ts `MAX_ATTACHMENTS`: the files one message carries.
pub const MAX_ATTACHMENTS: usize = 10;
/// thumb.ts `RENDERABLE_IMAGE`: the image types drawn inline.
pub const RENDERABLE_IMAGE: [&str; 4] = ["image/png", "image/jpeg", "image/webp", "image/gif"];
/// derive.ts `PLAYABLE_AUDIO`.
pub const PLAYABLE_AUDIO: [&str; 9] = [
    "audio/mpeg",
    "audio/mp4",
    "audio/aac",
    "audio/ogg",
    "audio/opus",
    "audio/webm",
    "audio/wav",
    "audio/x-wav",
    "audio/flac",
];
/// derive.ts `PLAYABLE_VIDEO`.
pub const PLAYABLE_VIDEO: [&str; 3] = ["video/mp4", "video/webm", "video/ogg"];
/// Attachments.tsx `GIF_AUTOPLAY_MAX`: a GIF plays inline only up to this size.
pub const GIF_AUTOPLAY_MAX: u64 = 8 * 1024 * 1024;

const THUMB_SIDES: [u32; 4] = [320, 240, 160, 120];
const THUMB_QUALITIES: [u8; 4] = [80, 60, 40, 25];

/// crypto.ts `AttachmentError`, with the codes the web client shows.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum AttachmentError {
    #[error("E_BLOB_HASH")]
    BlobHash,
    #[error("E_BLOB_OPEN{}", if .0.is_empty() { String::new() } else { format!(": {}", .0) })]
    BlobOpen(String),
    #[error("E_ATTACHMENT_TOO_LARGE")]
    TooLarge,
    #[error("E_ATTACHMENT_COUNT")]
    Count,
    #[error("E_ATTACHMENT_MISSING")]
    Missing,
    #[error("E_TRAY_NOT_READY")]
    NotReady,
    /// The channel has no group on this device yet (the worker's `E_NOT_READY`).
    #[error("E_NOT_READY")]
    NoGroup,
    /// A route's refusal, a core refusal or a local I/O failure, with its stable code.
    #[error("{0}")]
    Other(String),
}

impl AttachmentError {
    /// The stable code (`E_…`) a card or a tray entry shows.
    pub fn code(&self) -> String {
        match self {
            Self::BlobOpen(_) => "E_BLOB_OPEN".to_owned(),
            Self::Other(code) => code.clone(),
            e => e.to_string(),
        }
    }
}

impl From<ApiError> for AttachmentError {
    fn from(e: ApiError) -> Self {
        Self::Other(e.code)
    }
}

impl From<AttachmentError> for ClientError {
    fn from(e: AttachmentError) -> Self {
        ClientError::Sync(e.code())
    }
}

impl From<dilla_core::error::ProtocolError> for AttachmentError {
    fn from(e: dilla_core::error::ProtocolError) -> Self {
        use dilla_core::error::ProtocolError as P;
        match e {
            P::BlobHash => Self::BlobHash,
            P::BlobOpen => Self::BlobOpen(String::new()),
            other => Self::Other(other.code().to_owned()),
        }
    }
}

/// derive.ts `attachmentKind` (L-TS-58): how a card shows an attachment.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AttachmentKind {
    Image,
    Audio,
    Video,
    File,
}

/// L-TS-58. The size does not change the kind: a file over the cap keeps it and is marked too large.
pub fn attachment_kind(mime: &str) -> AttachmentKind {
    if RENDERABLE_IMAGE.contains(&mime) {
        AttachmentKind::Image
    } else if PLAYABLE_AUDIO.contains(&mime) {
        AttachmentKind::Audio
    } else if PLAYABLE_VIDEO.contains(&mime) {
        AttachmentKind::Video
    } else {
        AttachmentKind::File
    }
}

/// name.ts `safeName` (L-TS-32): a display name without control and bidi characters or path separators, at most 255
/// UTF-8 bytes, cut at a scalar boundary; `fallback` when nothing is left.
pub fn safe_name(name: &str, fallback: &str) -> String {
    let banned = |c: char| {
        matches!(c as u32,
            0x0000..=0x001f | 0x007f..=0x009f | 0x061c | 0x200e..=0x200f | 0x202a..=0x202e | 0x2066..=0x206f)
    };
    let clean: String = name
        .chars()
        .filter(|&c| !banned(c))
        .map(|c| if c == '/' || c == '\\' { '_' } else { c })
        .collect();
    // JS `trim` removes Unicode white space and line terminators; char::is_whitespace is the same set.
    let clean = clean.trim();
    let mut out = String::new();
    for c in clean.chars() {
        if out.len() + c.len_utf8() > 255 {
            break;
        }
        out.push(c);
    }
    if out.is_empty() {
        fallback.to_owned()
    } else {
        out
    }
}

/// The MIME type of a file picked from disk or dropped, by its extension (a browser's `File.type`); anything else is
/// `application/octet-stream`, as tray.ts names an untyped file.
pub fn mime_of(name: &str) -> &'static str {
    let ext = name
        .rsplit_once('.')
        .map(|(_, e)| e.to_ascii_lowercase())
        .unwrap_or_default();
    match ext.as_str() {
        "png" => "image/png",
        "jpg" | "jpeg" => "image/jpeg",
        "webp" => "image/webp",
        "gif" => "image/gif",
        "mp3" => "audio/mpeg",
        "m4a" => "audio/mp4",
        "aac" => "audio/aac",
        "ogg" | "oga" => "audio/ogg",
        "opus" => "audio/opus",
        "weba" => "audio/webm",
        "wav" => "audio/wav",
        "flac" => "audio/flac",
        "mp4" | "m4v" => "video/mp4",
        "webm" => "video/webm",
        "ogv" => "video/ogg",
        "pdf" => "application/pdf",
        "txt" => "text/plain",
        _ => "application/octet-stream",
    }
}

/// thumb.ts `isThumbnailBytes`: a WebP (`RIFF....WEBP`) or a JPEG (`FF D8 FF`) only.
pub fn is_thumbnail_bytes(b: &[u8]) -> bool {
    (b.len() >= 12 && &b[0..4] == b"RIFF" && &b[8..12] == b"WEBP")
        || (b.len() >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff)
}

/// thumb.ts `Thumbnail`: the preview's bytes (none when no size fits the envelope) and the original's size.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Thumbnail {
    pub bytes: Option<Vec<u8>>,
    pub w: u32,
    pub h: u32,
}

/// The most a picture may take decoded (width x height x 4 bytes x frames) before it is drawn whole, made into a
/// preview or played as a GIF (security review, Task 12): a larger one stays a still or a neutral box.
pub const DECODE_BUDGET: u64 = 256 * 1024 * 1024;

/// A GIF's frames and the largest area any of them (or its screen) declares, read from its blocks without decoding.
fn gif_frames(b: &[u8]) -> Option<(u64, u64)> {
    let area = |at: usize| -> Option<u64> {
        let w = u16::from_le_bytes([*b.get(at)?, *b.get(at + 1)?]);
        let h = u16::from_le_bytes([*b.get(at + 2)?, *b.get(at + 3)?]);
        Some(u64::from(w) * u64::from(h))
    };
    let mut max = area(6)?;
    let packed = *b.get(10)?;
    let mut at = 13
        + if packed & 0x80 != 0 {
            3 << ((packed & 7) + 1)
        } else {
            0
        };
    let skip_blocks = |mut at: usize| -> Option<usize> {
        loop {
            let len = usize::from(*b.get(at)?);
            at += 1 + len;
            if len == 0 {
                return Some(at);
            }
        }
    };
    let mut frames = 0u64;
    while let Some(&tag) = b.get(at) {
        match tag {
            0x2c => {
                max = max.max(area(at + 5)?);
                frames += 1;
                let packed = *b.get(at + 9)?;
                at += 10
                    + if packed & 0x80 != 0 {
                        3 << ((packed & 7) + 1)
                    } else {
                        0
                    };
                // The LZW minimum code size, then the data's sub-blocks.
                at = skip_blocks(at + 1)?;
            }
            0x21 => at = skip_blocks(at + 2)?,
            _ => break,
        }
    }
    Some((frames.max(1), max))
}

/// An animated PNG's frame count (`acTL`), 1 for a still.
fn png_frames(b: &[u8]) -> u64 {
    let mut at = 8;
    while at + 12 <= b.len() {
        let len = u32::from_be_bytes([b[at], b[at + 1], b[at + 2], b[at + 3]]) as usize;
        let kind = &b[at + 4..at + 8];
        if kind == b"acTL" && at + 12 <= b.len() {
            return u64::from(u32::from_be_bytes([
                b[at + 8],
                b[at + 9],
                b[at + 10],
                b[at + 11],
            ]))
            .max(1);
        }
        if kind == b"IDAT" {
            break;
        }
        at = at.saturating_add(12).saturating_add(len);
    }
    1
}

/// An animated WebP's frame count (`ANMF` chunks), 1 for a still.
fn webp_frames(b: &[u8]) -> u64 {
    let mut at = 12;
    let mut n = 0u64;
    while at + 8 <= b.len() {
        let len = u32::from_le_bytes([b[at + 4], b[at + 5], b[at + 6], b[at + 7]]) as usize;
        if &b[at..at + 4] == b"ANMF" {
            n += 1;
        }
        at = at.saturating_add(8).saturating_add(len + (len & 1));
    }
    n.max(1)
}

/// What a picture takes decoded, all frames (width x height x 4 x frames), from its headers only; `None` when the
/// headers do not say (not a picture, or cut short).
pub fn decoded_size(bytes: &[u8]) -> Option<u64> {
    if bytes.starts_with(b"GIF8") {
        let (frames, area) = gif_frames(bytes)?;
        return Some(area.saturating_mul(4).saturating_mul(frames));
    }
    let reader = image::ImageReader::new(std::io::Cursor::new(bytes))
        .with_guessed_format()
        .ok()?;
    let format = reader.format()?;
    let (w, h) = reader.into_dimensions().ok()?;
    let frames = match format {
        image::ImageFormat::Png => png_frames(bytes),
        image::ImageFormat::WebP => webp_frames(bytes),
        _ => 1,
    };
    Some(
        u64::from(w)
            .saturating_mul(u64::from(h))
            .saturating_mul(4)
            .saturating_mul(frames),
    )
}

/// Whether a picture may be decoded whole: its headers say it fits [`DECODE_BUDGET`].
pub fn fits_decode_budget(bytes: &[u8]) -> bool {
    decoded_size(bytes).is_some_and(|n| n <= DECODE_BUDGET)
}

/// The largest side a preview is made from.
const THUMB_MAX_SIDE: u32 = 16_384;

/// The decode bound of an image whose preview is made: a picture over it gets no preview (never an allocation the size
/// a hostile header names).
const THUMB_DECODE_ALLOC: u64 = 512 * 1024 * 1024;

/// thumb.ts `makeThumbnail`: the longest side scaled to 320 (never up), then 240, 160, 120, each at the qualities 0.8,
/// 0.6, 0.4, 0.25, until one fits [`MAX_THUMB_PLAINTEXT`]. The web client prefers WebP and falls back to JPEG when its
/// encoder has none; this one takes the JPEG path (the `image` crate's WebP encoder is lossless only), which every
/// client opens (thumb.ts `isThumbnailBytes`). `None` for bytes that do not decode as an image.
pub fn make_thumbnail(bytes: &[u8]) -> Option<Thumbnail> {
    // Only the first frame is decoded; its own size (any frame's, for a GIF) must fit the budget and the side cap.
    let one_frame = if bytes.starts_with(b"GIF8") {
        gif_frames(bytes).map(|(_, area)| area.saturating_mul(4))
    } else {
        decoded_size(bytes).map(|n| {
            let frames = match image::guess_format(bytes) {
                Ok(image::ImageFormat::Png) => png_frames(bytes),
                Ok(image::ImageFormat::WebP) => webp_frames(bytes),
                _ => 1,
            };
            n / frames.max(1)
        })
    };
    if one_frame.is_none_or(|n| n > DECODE_BUDGET) {
        return None;
    }
    let mut reader = image::ImageReader::new(std::io::Cursor::new(bytes))
        .with_guessed_format()
        .ok()?;
    let mut limits = image::Limits::default();
    limits.max_image_width = Some(THUMB_MAX_SIDE);
    limits.max_image_height = Some(THUMB_MAX_SIDE);
    limits.max_alloc = Some(THUMB_DECODE_ALLOC);
    reader.limits(limits);
    let img = reader.decode().ok()?;
    let (w, h) = (img.width(), img.height());
    for side in THUMB_SIDES {
        let scale = (f64::from(side) / f64::from(w.max(h))).min(1.0);
        let tw = ((f64::from(w) * scale).round() as u32).max(1);
        let th = ((f64::from(h) * scale).round() as u32).max(1);
        let small = img
            .resize_exact(tw, th, image::imageops::FilterType::Triangle)
            .to_rgb8();
        for q in THUMB_QUALITIES {
            let mut out = Vec::new();
            let enc = image::codecs::jpeg::JpegEncoder::new_with_quality(&mut out, q);
            if small.write_with_encoder(enc).is_err() {
                return Some(Thumbnail { bytes: None, w, h });
            }
            if out.len() <= MAX_THUMB_PLAINTEXT {
                return Some(Thumbnail {
                    bytes: Some(out),
                    w,
                    h,
                });
            }
        }
    }
    Some(Thumbnail { bytes: None, w, h })
}

/// core-port.ts `AttachmentDescriptor` (the L-CORE-30 element). It holds the file's key: never logged, never published,
/// zeroized on drop.
#[derive(Clone)]
pub struct Descriptor {
    pub(crate) blob_id: [u8; 32],
    pub(crate) key: Zeroizing<[u8; 32]>,
    pub(crate) nonce: [u8; 12],
    pub size: u64,
    pub mime: String,
    pub w: Option<u32>,
    pub h: Option<u32>,
    pub(crate) thumb: Option<Vec<u8>>,
    pub name: String,
}

impl std::fmt::Debug for Descriptor {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Descriptor")
            .field("size", &self.size)
            .field("mime", &self.mime)
            .field("name", &self.name)
            .finish_non_exhaustive()
    }
}

impl Descriptor {
    /// Whether the descriptor carries a sealed thumbnail.
    pub fn has_thumb(&self) -> bool {
        self.thumb.is_some()
    }

    /// `[blob_id, key, nonce, size, mime, w|null, h|null, thumb|null, name]` (core-port.ts `readDescriptor`).
    pub(crate) fn decode(raw: &[u8]) -> Result<Self, AttachmentError> {
        let mut tree =
            value::decode(raw).map_err(|_| AttachmentError::Other("E_CBOR_SHAPE".to_owned()))?;
        Self::from_tree(&mut tree)
    }

    /// [`Self::decode`] from the decoded tree, whose copy of the key is zeroed before this returns (on success or
    /// refusal).
    pub(crate) fn from_tree(tree: &mut Value) -> Result<Self, AttachmentError> {
        let out = Self::read_tree(tree);
        if let Value::Array(a) = tree
            && let Some(Value::Bytes(key)) = a.get_mut(1)
        {
            zeroize::Zeroize::zeroize(&mut key[..]);
        }
        out
    }

    fn read_tree(v: &Value) -> Result<Self, AttachmentError> {
        let bad = || AttachmentError::Other("E_CBOR_SHAPE".to_owned());
        let a = value::arr(v, 9).map_err(|_| bad())?;
        let fixed = |v: &Value, n: usize| -> Result<Vec<u8>, AttachmentError> {
            v.as_bytes()
                .filter(|b| b.len() == n)
                .map(<[u8]>::to_vec)
                .ok_or_else(bad)
        };
        let dim = |v: &Value| -> Result<Option<u32>, AttachmentError> {
            value::opt(v, value::u53)
                .map_err(|_| bad())?
                .map(|n| u32::try_from(n).map_err(|_| bad()))
                .transpose()
        };
        let mut key = Zeroizing::new([0u8; 32]);
        match a[1].as_bytes() {
            Some(k) if k.len() == 32 => key.copy_from_slice(k),
            _ => return Err(bad()),
        }
        Ok(Self {
            blob_id: fixed(&a[0], 32)?.try_into().map_err(|_| bad())?,
            key,
            nonce: fixed(&a[2], 12)?.try_into().map_err(|_| bad())?,
            size: value::u53(&a[3]).map_err(|_| bad())?,
            mime: value::text(&a[4]).map_err(|_| bad())?,
            w: dim(&a[5])?,
            h: dim(&a[6])?,
            thumb: value::opt(&a[7], |v| {
                v.as_bytes()
                    .map(<[u8]>::to_vec)
                    .ok_or(value::DecodeError("thumb"))
            })
            .map_err(|_| bad())?,
            name: value::text(&a[8]).map_err(|_| bad())?,
        })
    }
}

/// A CBOR head (major type and argument), in its shortest form, as `Value::encode` writes it.
fn cbor_head(out: &mut Vec<u8>, major: u8, n: u64) {
    let m = major << 5;
    match n {
        0..=23 => out.push(m | n as u8),
        24..=0xff => out.extend_from_slice(&[m | 24, n as u8]),
        0x100..=0xffff => {
            out.push(m | 25);
            out.extend_from_slice(&(n as u16).to_be_bytes());
        }
        0x1_0000..=0xffff_ffff => {
            out.push(m | 26);
            out.extend_from_slice(&(n as u32).to_be_bytes());
        }
        _ => {
            out.push(m | 27);
            out.extend_from_slice(&n.to_be_bytes());
        }
    }
}

fn cbor_bytes(out: &mut Vec<u8>, b: &[u8]) {
    cbor_head(out, 2, b.len() as u64);
    out.extend_from_slice(b);
}

fn cbor_text(out: &mut Vec<u8>, t: &str) {
    cbor_head(out, 3, t.len() as u64);
    out.extend_from_slice(t.as_bytes());
}

fn cbor_opt_uint(out: &mut Vec<u8>, n: Option<u32>) {
    match n {
        Some(n) => cbor_head(out, 0, u64::from(n)),
        None => out.push(0xf6),
    }
}

/// The core's send request `[0, reply_to, body, [attachment...], null]` (`send_prepare`; `reply_to` null without a reply),
/// encoded by hand into one
/// zeroizing buffer sized up front, so the attachments' keys exist only there (no `Value` tree, no grown buffer
/// leaving a copy behind). Answers the buffer and the capacity it was made with.
fn encode_send(
    body: &str,
    attachments: &[Descriptor],
    reply: Option<MessageId>,
) -> (Zeroizing<Vec<u8>>, usize) {
    // A head is at most 9 bytes; an element has 10 of them plus its payloads.
    let capacity = 64
        + 16
        + body.len()
        + attachments
            .iter()
            .map(|d| {
                10 * 9
                    + 32
                    + 32
                    + 12
                    + d.mime.len()
                    + d.name.len()
                    + d.thumb.as_ref().map_or(0, Vec::len)
            })
            .sum::<usize>();
    let mut out = Zeroizing::new(Vec::with_capacity(capacity));
    cbor_head(&mut out, 4, 5);
    cbor_head(&mut out, 0, 0);
    match reply {
        Some(to) => cbor_bytes(&mut out, &to.0),
        None => out.push(0xf6),
    }
    cbor_text(&mut out, body);
    cbor_head(&mut out, 4, attachments.len() as u64);
    for d in attachments {
        cbor_head(&mut out, 4, 9);
        cbor_bytes(&mut out, &d.blob_id);
        cbor_bytes(&mut out, &d.key[..]);
        cbor_bytes(&mut out, &d.nonce);
        cbor_head(&mut out, 0, d.size);
        cbor_text(&mut out, &d.mime);
        cbor_opt_uint(&mut out, d.w);
        cbor_opt_uint(&mut out, d.h);
        match &d.thumb {
            Some(t) => cbor_bytes(&mut out, t),
            None => out.push(0xf6),
        }
        cbor_text(&mut out, &d.name);
    }
    out.push(0xf6);
    (out, capacity)
}

/// download.ts `fetchAttachment`: refused over the cap before any request; the stored object fetched with a body
/// bound of its sealed size (`size + 16`), a longer one (`E_BODY_TOO_LARGE`) refused as E_BLOB_OPEN before the rest
/// is read; then [`open_stored`].
pub async fn fetch(
    routes: &dyn BlobRoutes,
    channel: ChannelId,
    d: &Descriptor,
) -> Result<Zeroizing<Vec<u8>>, AttachmentError> {
    if d.size > ATTACHMENT_CAP {
        return Err(AttachmentError::TooLarge);
    }
    let stored = routes
        .get(channel, d.blob_id, d.size + TAG_LEN as u64)
        .await
        .map_err(|e| {
            if e.code == "E_BODY_TOO_LARGE" {
                AttachmentError::BlobOpen(
                    "the stored object is larger than its descriptor".to_owned(),
                )
            } else {
                AttachmentError::from(e)
            }
        })?;
    open_stored(d, stored.as_deref())
}

/// download.ts after the fetch: a stored object longer than `size + 16` is refused before it is hashed or opened
/// (FACTS-SECURITY-13), then `open_blob` checks the hash, the AEAD and the size.
pub fn open_stored(
    d: &Descriptor,
    stored: Option<&[u8]>,
) -> Result<Zeroizing<Vec<u8>>, AttachmentError> {
    if d.size > ATTACHMENT_CAP {
        return Err(AttachmentError::TooLarge);
    }
    let stored = stored.ok_or(AttachmentError::Missing)?;
    if stored.len() as u64 > d.size + TAG_LEN as u64 {
        return Err(AttachmentError::BlobOpen(
            "the stored object is larger than its descriptor".to_owned(),
        ));
    }
    Ok(Zeroizing::new(open_blob(
        &d.key, &d.nonce, &d.blob_id, d.size, stored,
    )?))
}

/// download.ts `thumbnailBlob`: the sealed thumbnail opened, refused unless it is a WebP or a JPEG.
pub fn open_thumbnail(d: &Descriptor) -> Result<Zeroizing<Vec<u8>>, AttachmentError> {
    let thumb = d.thumb.as_deref().ok_or(AttachmentError::Missing)?;
    let plain = Zeroizing::new(open_thumb(&d.key, &d.nonce, thumb)?);
    if !is_thumbnail_bytes(&plain) {
        return Err(AttachmentError::BlobOpen(String::new()));
    }
    Ok(plain)
}

/// The blob routes a tray and a download use (`Pick<Routes, 'putBlob' | 'getBlob' | 'deleteBlob' | 'confirmBlob'>`): a
/// seam, so the tray's logic is tested without an instance.
#[async_trait::async_trait]
pub trait BlobRoutes: Send + Sync {
    async fn put(
        &self,
        channel: ChannelId,
        blob: [u8; 32],
        stored: Vec<u8>,
    ) -> Result<(), ApiError>;
    /// The stored bytes, at most `cap` of them (a longer body is `E_BODY_TOO_LARGE`, not read on); `None` on 404.
    async fn get(
        &self,
        channel: ChannelId,
        blob: [u8; 32],
        cap: u64,
    ) -> Result<Option<Vec<u8>>, ApiError>;
    async fn delete(&self, channel: ChannelId, blob: [u8; 32]) -> Result<(), ApiError>;
    async fn confirm(&self, channel: ChannelId, blob: [u8; 32]) -> Result<(), ApiError>;
}

/// [`BlobRoutes`] over the instance (HTTP rows of the route table).
struct InstanceBlobs(Arc<Api>);

#[async_trait::async_trait]
impl BlobRoutes for InstanceBlobs {
    async fn put(
        &self,
        channel: ChannelId,
        blob: [u8; 32],
        stored: Vec<u8>,
    ) -> Result<(), ApiError> {
        self.0
            .http_route::<r::PutBlob>(&[&channel.hex(), &hex(&blob)], Raw(stored), true)
            .await
            .map(|_| ())
    }
    async fn get(
        &self,
        channel: ChannelId,
        blob: [u8; 32],
        cap: u64,
    ) -> Result<Option<Vec<u8>>, ApiError> {
        let reply = self
            .0
            .http_route_capped::<r::GetBlob>(&[&channel.hex(), &hex(&blob)], NoBody, true, cap)
            .await?;
        Ok((reply.status == 200).then_some(reply.body))
    }
    async fn delete(&self, channel: ChannelId, blob: [u8; 32]) -> Result<(), ApiError> {
        self.0
            .http_route::<r::DeleteBlob>(&[&channel.hex(), &hex(&blob)], NoBody, true)
            .await
    }
    async fn confirm(&self, channel: ChannelId, blob: [u8; 32]) -> Result<(), ApiError> {
        self.0
            .http_route::<r::ConfirmBlob>(&[&channel.hex(), &hex(&blob)], NoBody, true)
            .await
    }
}

/// Reads a picked or dropped file, at most one byte past [`ATTACHMENT_CAP`] (`File::open(..).take(CAP + 1)`), into a
/// zeroizing buffer: a device that never ends (`/dev/zero`) or a file that grows while it is read stops there, and its
/// length says it is over the cap.
pub fn read_bounded(path: &Path) -> std::io::Result<Zeroizing<Vec<u8>>> {
    use std::io::Read;
    let mut out = Zeroizing::new(Vec::new());
    std::fs::File::open(path)?
        .take(ATTACHMENT_CAP + 1)
        .read_to_end(&mut out)?;
    Ok(out)
}

/// A file handed to the tray: picked or dropped from disk (read when its turn comes, never when it is over the cap),
/// or bytes already in memory (a pasted image, a picked GIF).
pub enum FileSource {
    Path(PathBuf),
    Bytes(Zeroizing<Vec<u8>>),
}

/// One file to attach.
pub struct NewFile {
    pub name: String,
    /// `None`: named by its extension ([`mime_of`]).
    pub mime: Option<String>,
    pub size: u64,
    pub source: FileSource,
}

impl NewFile {
    /// A file on disk, its size from its metadata.
    pub fn from_path(path: &Path) -> std::io::Result<Self> {
        let size = std::fs::metadata(path)?.len();
        let name = path
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_default();
        Ok(Self {
            name,
            mime: None,
            size,
            source: FileSource::Path(path.to_owned()),
        })
    }

    /// Bytes in memory.
    pub fn from_bytes(name: &str, mime: &str, bytes: Vec<u8>) -> Self {
        Self {
            name: name.to_owned(),
            mime: Some(mime.to_owned()),
            size: bytes.len() as u64,
            source: FileSource::Bytes(Zeroizing::new(bytes)),
        }
    }
}

struct Entry {
    item: TrayItem,
    source: Option<FileSource>,
    removed: bool,
    uploaded: bool,
    blob: Option<[u8; 32]>,
    descriptor: Option<Descriptor>,
}

#[derive(Default)]
struct TrayList {
    entries: Vec<Entry>,
    next: u64,
}

type Publish = Arc<dyn Fn(Vec<TrayItem>) + Send + Sync>;

struct TrayShared {
    channel: ChannelId,
    routes: Arc<dyn BlobRoutes>,
    publish: Publish,
    list: Mutex<TrayList>,
    /// tray.ts `chain`: one batch is processed after the other.
    chain: tokio::sync::Mutex<()>,
}

/// tray.ts `Tray`: the files waiting to be sent in one channel. Cheap to clone; every clone is the same tray.
#[derive(Clone)]
pub struct Tray {
    shared: Arc<TrayShared>,
}

fn lock(m: &Mutex<TrayList>) -> std::sync::MutexGuard<'_, TrayList> {
    m.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}

impl Tray {
    /// A tray over `routes`, publishing its entries through `publish` on every change.
    pub fn new(
        channel: ChannelId,
        routes: Arc<dyn BlobRoutes>,
        publish: impl Fn(Vec<TrayItem>) + Send + Sync + 'static,
    ) -> Self {
        Self {
            shared: Arc::new(TrayShared {
                channel,
                routes,
                publish: Arc::new(publish),
                list: Mutex::new(TrayList::default()),
                chain: tokio::sync::Mutex::new(()),
            }),
        }
    }

    pub fn channel(&self) -> ChannelId {
        self.shared.channel
    }

    /// The entries, without their descriptors.
    pub fn entries(&self) -> Vec<TrayItem> {
        lock(&self.shared.list)
            .entries
            .iter()
            .map(|e| e.item.clone())
            .collect()
    }

    fn changed(&self) {
        let items = self.entries();
        (self.shared.publish)(items);
    }

    /// tray.ts `add`: refuses a batch that would hold more than [`MAX_ATTACHMENTS`] before reading any; a file over the
    /// cap stays as a failed card and is never read; the rest are read, sealed, previewed and uploaded one by one.
    /// Answers the new entries' ids once the batch is processed.
    pub async fn add(&self, files: Vec<NewFile>) -> Result<Vec<String>, AttachmentError> {
        let ids = self.push(files)?;
        self.process_all(&ids).await;
        Ok(ids)
    }

    /// The first half of [`Self::add`], at once: the count check and the new entries (in their first phase, an
    /// oversize one failed), published; a view keys its previews by the answered ids. [`Self::process_all`] does the
    /// rest.
    pub fn push(&self, files: Vec<NewFile>) -> Result<Vec<String>, AttachmentError> {
        let ids = {
            let mut list = lock(&self.shared.list);
            if list.entries.len() + files.len() > MAX_ATTACHMENTS {
                return Err(AttachmentError::Count);
            }
            let mut ids = Vec::with_capacity(files.len());
            for file in files {
                list.next += 1;
                let id = list.next.to_string();
                let mime = file
                    .mime
                    .filter(|m| !m.is_empty() && m.len() <= 255)
                    .unwrap_or_else(|| mime_of(&file.name).to_owned());
                let over = file.size > ATTACHMENT_CAP;
                list.entries.push(Entry {
                    item: TrayItem {
                        id: id.clone(),
                        name: safe_name(&file.name, ""),
                        size: file.size,
                        image: !over && RENDERABLE_IMAGE.contains(&mime.as_str()),
                        mime,
                        phase: if over {
                            TrayPhase::Failed
                        } else {
                            TrayPhase::Reading
                        },
                        reason: if over {
                            "E_ATTACHMENT_TOO_LARGE".to_owned()
                        } else {
                            String::new()
                        },
                    },
                    source: (!over).then_some(file.source),
                    removed: false,
                    uploaded: false,
                    blob: None,
                    descriptor: None,
                });
                ids.push(id);
            }
            ids
        };
        self.changed();
        Ok(ids)
    }

    /// The second half of [`Self::add`]: reads, seals, previews and uploads the entries `ids`, after any batch before.
    pub async fn process_all(&self, ids: &[String]) {
        let _turn = self.shared.chain.lock().await;
        for id in ids {
            self.process(id).await;
        }
    }

    /// Runs `f` on the entry `id` unless it was removed; `None` when it is gone or removed.
    fn with_entry<T>(&self, id: &str, f: impl FnOnce(&mut Entry) -> T) -> Option<T> {
        let mut list = lock(&self.shared.list);
        let e = list.entries.iter_mut().find(|e| e.item.id == id)?;
        if e.removed {
            return None;
        }
        Some(f(e))
    }

    fn fail(&self, id: &str, code: String) {
        if self
            .with_entry(id, |e| {
                e.item.phase = TrayPhase::Failed;
                e.item.reason = code;
                e.descriptor = None;
            })
            .is_some()
        {
            self.changed();
        }
    }

    async fn process(&self, id: &str) {
        let Some(Some(source)) = self.with_entry(id, |e| {
            if e.item.phase == TrayPhase::Failed {
                None
            } else {
                e.source.take()
            }
        }) else {
            return;
        };
        let bytes = match source {
            FileSource::Bytes(b) => b,
            FileSource::Path(p) => {
                match tokio::task::spawn_blocking(move || read_bounded(&p)).await {
                    Ok(Ok(b)) => b,
                    _ => return self.fail(id, "E_ATTACHMENT_READ".to_owned()),
                }
            }
        };
        // A file that grew past the cap after it was picked is refused as one over it was.
        if bytes.len() as u64 > ATTACHMENT_CAP {
            return self.fail(id, "E_ATTACHMENT_TOO_LARGE".to_owned());
        }
        let Some(image) = self.with_entry(id, |e| {
            e.item.phase = TrayPhase::Preparing;
            e.item.size = bytes.len() as u64;
            e.item.image
        }) else {
            return;
        };
        self.changed();
        let mut key = Zeroizing::new([0u8; 32]);
        let mut nonce = [0u8; 12];
        if getrandom::fill(key.as_mut()).is_err() || getrandom::fill(&mut nonce).is_err() {
            return self.fail(id, "E_ATTACHMENT_FAILED".to_owned());
        }
        let size = bytes.len() as u64;
        let sealed = {
            let key = key.clone();
            tokio::task::spawn_blocking(move || {
                let stored = seal_blob(&key, &nonce, &bytes);
                let id = blob_id(&stored);
                let preview = if image { make_thumbnail(&bytes) } else { None };
                let thumb = preview
                    .as_ref()
                    .and_then(|p| p.bytes.as_deref())
                    .map(|b| seal_thumb(&key, &nonce, b));
                (stored, id, preview.map(|p| (p.w, p.h)), thumb)
            })
            .await
        };
        let Ok((stored, blob, dims, thumb)) = sealed else {
            return self.fail(id, "E_ATTACHMENT_FAILED".to_owned());
        };
        let thumb = match thumb.transpose() {
            Ok(t) => t,
            Err(e) => return self.fail(id, AttachmentError::from(e).code()),
        };
        if self
            .with_entry(id, |e| {
                e.item.phase = TrayPhase::Uploading;
                e.blob = Some(blob);
            })
            .is_none()
        {
            return;
        }
        self.changed();
        if let Err(e) = self
            .shared
            .routes
            .put(self.shared.channel, blob, stored)
            .await
        {
            return self.fail(id, e.code);
        }
        let still = {
            let mut list = lock(&self.shared.list);
            match list.entries.iter_mut().find(|e| e.item.id == id) {
                Some(e) if !e.removed => {
                    e.uploaded = true;
                    e.descriptor = Some(Descriptor {
                        blob_id: blob,
                        key,
                        nonce,
                        size,
                        mime: e.item.mime.clone(),
                        w: dims.map(|d| d.0),
                        h: dims.map(|d| d.1),
                        thumb,
                        name: e.item.name.clone(),
                    });
                    e.item.phase = TrayPhase::Ready;
                    true
                }
                _ => false,
            }
        };
        if still {
            self.changed();
        } else {
            // Removed while it uploaded: its reference goes; a failed delete is left to pending_ttl.
            let _ = self.shared.routes.delete(self.shared.channel, blob).await;
        }
    }

    /// tray.ts `discard`: the entry leaves the tray at once; an uploaded reference is deleted (a failure is left to the
    /// instance's pending_ttl), an upload in flight deletes its own when it ends.
    pub async fn discard(&self, id: &str) {
        let gone = {
            let mut list = lock(&self.shared.list);
            let Some(at) = list.entries.iter().position(|e| e.item.id == id) else {
                return;
            };
            let mut e = list.entries.remove(at);
            e.removed = true;
            (e.uploaded, e.blob)
        };
        self.changed();
        if let (true, Some(blob)) = gone {
            let _ = self.shared.routes.delete(self.shared.channel, blob).await;
        }
    }

    /// Whether every entry is ready (send refuses a tray still working: `shell.tray.notReady`).
    pub fn all_ready(&self) -> bool {
        lock(&self.shared.list)
            .entries
            .iter()
            .all(|e| e.item.phase == TrayPhase::Ready)
    }

    /// tray.ts `take`: the ready descriptors in the order asked, removed from the tray; a duplicate id or an entry not
    /// ready refuses the whole call and removes nothing.
    pub fn take(&self, ids: &[String]) -> Result<Vec<Descriptor>, AttachmentError> {
        let out = {
            let mut list = lock(&self.shared.list);
            let unique: std::collections::HashSet<&String> = ids.iter().collect();
            if unique.len() != ids.len() {
                return Err(AttachmentError::NotReady);
            }
            let mut out = Vec::with_capacity(ids.len());
            for id in ids {
                let e = list
                    .entries
                    .iter()
                    .find(|e| &e.item.id == id)
                    .ok_or(AttachmentError::NotReady)?;
                match (&e.item.phase, &e.descriptor) {
                    (TrayPhase::Ready, Some(d)) => out.push(d.clone()),
                    _ => return Err(AttachmentError::NotReady),
                }
            }
            list.entries.retain(|e| !ids.contains(&e.item.id));
            out
        };
        self.changed();
        Ok(out)
    }

    /// Every entry, as [`Self::take`] does: what a send carries.
    pub fn take_all(&self) -> Result<Vec<Descriptor>, AttachmentError> {
        let ids: Vec<String> = self.entries().into_iter().map(|e| e.id).collect();
        self.take(&ids)
    }
}

/// An opened attachment (the worker's `openAttachment` answer): the plaintext, which the caller holds only in memory.
pub struct Opened {
    pub bytes: Zeroizing<Vec<u8>>,
    pub name: String,
    pub mime: String,
}

/// The attachments area's handle for attachments: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Attachments {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Attachments`] handle.
    pub fn attachments(&self) -> Attachments {
        Attachments {
            inner: self.inner().clone(),
        }
    }
}

impl Attachments {
    fn routes(&self) -> Arc<dyn BlobRoutes> {
        Arc::new(InstanceBlobs(self.inner.api().clone()))
    }

    /// The text group of `channel` this device holds, active (the worker's `timelineGroup`).
    async fn group_of(inner: &Inner, channel: ChannelId) -> Result<[u8; 16], AttachmentError> {
        inner
            .engine()
            .groups()
            .await
            .into_iter()
            .find(|g| g.target_id == channel.0 && g.state == 2)
            .map(|g| g.group_id)
            .ok_or(AttachmentError::NoGroup)
    }

    /// A new, empty tray for `channel` that publishes its entries in `AppState::attachments.trays`. The caller keeps
    /// it (one per channel) as the worker keeps `trays`; dropping it drops its files without a request (pending_ttl).
    pub fn tray(&self, channel: ChannelId) -> Tray {
        let inner = self.inner.clone();
        Tray::new(channel, self.routes(), move |items| {
            inner.publish(move |s| {
                if items.is_empty() {
                    s.attachments.trays.remove(&channel);
                } else {
                    s.attachments.trays.insert(channel, items);
                }
            });
        })
    }

    /// The worker's `attachFiles`, from the UI thread: the entries are added at once (their ids answered, or the count
    /// refused) and processed on the client's runtime.
    pub fn attach(&self, tray: &Tray, files: Vec<NewFile>) -> Result<Vec<String>, AttachmentError> {
        let ids = tray.push(files)?;
        let (tray, run) = (tray.clone(), ids.clone());
        drop(self.inner.spawn(async move {
            tray.process_all(&run).await;
            Ok(())
        }));
        Ok(ids)
    }

    /// The worker's `discardAttachment`, from the UI thread.
    pub fn discard(&self, tray: &Tray, id: String) {
        let tray = tray.clone();
        drop(self.inner.spawn(async move {
            tray.discard(&id).await;
            Ok(())
        }));
    }

    /// The worker's `openAttachment`: the descriptor of attachment `index` of the stored message `seq`, then its
    /// thumbnail, or the stored object fetched and opened ([`open_stored`]).
    pub fn open(
        &self,
        channel: ChannelId,
        seq: u64,
        index: u32,
        thumb: bool,
    ) -> tokio::sync::oneshot::Receiver<Result<Opened, ClientError>> {
        let inner = self.inner.clone();
        let routes = self.routes();
        self.inner.spawn(async move {
            let g = Self::group_of(&inner, channel).await?;
            // The core's answer holds the file's key: it is wiped when dropped.
            let raw = Zeroizing::new(
                with_core(inner.engine().core(), move |c| {
                    c.attachment_get(&g, seq, index)
                })
                .await
                .map_err(|e| AttachmentError::Other(e.code.to_owned()))?,
            );
            let d = Descriptor::decode(&raw)?;
            drop(raw);
            let name = safe_name(&d.name, "");
            let mime = d.mime.clone();
            let bytes = if thumb {
                open_thumbnail(&d)?
            } else {
                fetch(&*routes, channel, &d).await?
            };
            Ok(Opened { bytes, name, mime })
        })
    }

    /// Writes an opened file to `path`, which the person chose (a save dialog): the only way plaintext reaches the
    /// disk, and only after their action.
    pub fn save(path: &Path, bytes: &[u8]) -> std::io::Result<()> {
        std::fs::write(path, bytes)
    }

    /// send.ts `sendMessage` with attachments: each reference is confirmed (the worker's `beforeSend`), then the post
    /// is queued in the group's outbox and drained. A reply with files carries `reply` in the same post (Shell.tsx's
    /// one `send` with `replyTo` and `attachments`).
    ///
    /// Deviation, until the sync engine runs `beforeSend` itself (sync/mod.rs, Task 2's file; see Task 12's report): the
    /// confirms run here, once, before the post is queued, not before every upload attempt.
    pub fn send(
        &self,
        channel: ChannelId,
        body: String,
        attachments: Vec<Descriptor>,
        reply: Option<MessageId>,
    ) -> tokio::sync::oneshot::Receiver<Result<MessageId, ClientError>> {
        let inner = self.inner.clone();
        let routes = self.routes();
        self.inner.spawn(async move {
            if attachments.len() > MAX_ATTACHMENTS {
                return Err(AttachmentError::Count.into());
            }
            let g = Self::group_of(&inner, channel).await?;
            for a in &attachments {
                routes.confirm(channel, a.blob_id).await.map_err(|e| {
                    if e.status == 404 {
                        AttachmentError::Missing
                    } else {
                        AttachmentError::from(e)
                    }
                })?;
            }
            let (request, _) = encode_send(&body, &attachments, reply);
            drop(attachments);
            // Queued behind the trust retry, as every send is (controller.ts `sendWithTrustRetry`): a post with files
            // right after a peer's new device joined is not refused for it.
            let msg = inner
                .engine()
                .send_with_trust_retry(g, request)
                .await
                .map_err(|e| ClientError::Sync(e.code()))?;
            Ok(MessageId(msg))
        })
    }
}

/// A parity fixture's picture: a PNG of `w` x `h` whose pixels follow from its name only, so the seeded file (the web
/// reference draws its thumbnail) and the GPUI side's are the same bytes on every run.
#[cfg(feature = "testing")]
pub fn fixture_picture(name: &str, w: u32, h: u32) -> Vec<u8> {
    let seed = dilla_core::identity::sha256(name.as_bytes());
    let img = image::RgbImage::from_fn(w.max(1), h.max(1), |x, y| {
        let band = ((x / 16 + y / 16) % 4) as usize;
        image::Rgb([
            seed[band] / 2 + 64,
            seed[band + 4] / 2 + 64,
            seed[band + 8] / 2 + 64,
        ])
    });
    let mut out = std::io::Cursor::new(Vec::new());
    img.write_to(&mut out, image::ImageFormat::Png)
        .expect("a PNG encodes into memory");
    out.into_inner()
}

/// The logic tests of attachments/{name,thumb,download,tray}.test.ts, same inputs and same outputs.
#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::sync::Mutex as StdMutex;

    #[test]
    fn safe_name_matches_name_test_ts() {
        for (input, want) in [
            ("report.pdf", "report.pdf"),
            ("../etc/passwd", ".._etc_passwd"),
            ("a\\b.txt", "a_b.txt"),
            ("\u{202e}gnp.exe", "gnp.exe"),
            ("\u{2066}x\u{2069}.png", "x.png"),
            ("invoice.pdf\u{200e}\u{200f}.exe", "invoice.pdf.exe"),
            ("photo.jpg\u{061c}.exe", "photo.jpg.exe"),
            ("report.pdf\u{206a}\u{206f}.exe", "report.pdf.exe"),
            ("tab\there", "tabhere"),
            ("\u{0085}x\u{007f}\u{009f}", "x"),
            ("  spaced  ", "spaced"),
        ] {
            assert_eq!(safe_name(input, "file"), want, "{input:?}");
        }
        assert_eq!(safe_name("", "file"), "file");
        assert_eq!(safe_name("\u{2066}\u{2069} \u{0000}", "file"), "file");
        assert_eq!(safe_name("", ""), "");
        assert_eq!(safe_name(&"a".repeat(300), "file"), "a".repeat(255));
        let e = safe_name(&"é".repeat(200), "file");
        assert_eq!(e, "é".repeat(127));
        assert_eq!(e.len(), 254);
        let smile = safe_name(&"😀".repeat(70), "file");
        assert_eq!(smile, "😀".repeat(63));
        assert_eq!(smile.len(), 252);
    }

    #[test]
    fn kinds_follow_derive_ts() {
        assert_eq!(attachment_kind("image/gif"), AttachmentKind::Image);
        assert_eq!(attachment_kind("image/svg+xml"), AttachmentKind::File);
        assert_eq!(attachment_kind("audio/opus"), AttachmentKind::Audio);
        assert_eq!(attachment_kind("video/webm"), AttachmentKind::Video);
        assert_eq!(attachment_kind("video/quicktime"), AttachmentKind::File);
        assert_eq!(
            RENDERABLE_IMAGE,
            ["image/png", "image/jpeg", "image/webp", "image/gif"]
        );
    }

    #[test]
    fn recognises_webp_and_jpeg_only() {
        assert!(is_thumbnail_bytes(b"RIFF\0\0\0\0WEBPVP8 "));
        assert!(is_thumbnail_bytes(&[0xff, 0xd8, 0xff]));
        assert!(!is_thumbnail_bytes(&[0xff, 0xd8]));
        assert!(!is_thumbnail_bytes(b"\x89PNG\r\n\x1a\n...."));
        assert!(!is_thumbnail_bytes(b"RIFF\0\0\0\0WAVE"));
    }

    fn png(w: u32, h: u32) -> Vec<u8> {
        let img = image::RgbImage::from_fn(w, h, |x, y| {
            image::Rgb([(x * 7) as u8, (y * 5) as u8, ((x ^ y) * 3) as u8])
        });
        let mut out = std::io::Cursor::new(Vec::new());
        img.write_to(&mut out, image::ImageFormat::Png).unwrap();
        out.into_inner()
    }

    #[test]
    fn a_thumbnail_scales_the_longest_side_to_320_and_fits_the_envelope() {
        let t = make_thumbnail(&png(640, 480)).expect("an image");
        assert_eq!((t.w, t.h), (640, 480), "the original's size");
        let bytes = t.bytes.expect("a preview that fits");
        assert!(bytes.len() <= MAX_THUMB_PLAINTEXT);
        assert!(is_thumbnail_bytes(&bytes));
        let small = image::load_from_memory(&bytes).unwrap();
        assert!(small.width() <= 320 && small.height() <= 240);
    }

    #[test]
    fn a_thumbnail_never_scales_up_and_keeps_a_portrait_within_320() {
        let t = make_thumbnail(&png(40, 30)).unwrap();
        let small = image::load_from_memory(&t.bytes.unwrap()).unwrap();
        assert_eq!((small.width(), small.height()), (40, 30));
        let t = make_thumbnail(&png(300, 900)).unwrap();
        let small = image::load_from_memory(&t.bytes.unwrap()).unwrap();
        assert!(small.height() <= 320 && small.width() <= 107);
    }

    /// A GIF of a few dozen bytes that declares a 65535 x 65535 screen and frame.
    fn gif_bomb(frames: usize) -> Vec<u8> {
        let mut g = b"GIF89a".to_vec();
        g.extend_from_slice(&[0xff, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00]);
        for _ in 0..frames {
            g.push(0x2c);
            g.extend_from_slice(&[0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0x00]);
            g.extend_from_slice(&[0x02, 0x02, 0x4c, 0x01, 0x00]);
        }
        g.push(0x3b);
        g
    }

    fn png_header(w: u32, h: u32) -> Vec<u8> {
        let mut p = b"\x89PNG\r\n\x1a\n".to_vec();
        p.extend_from_slice(&13u32.to_be_bytes());
        p.extend_from_slice(b"IHDR");
        p.extend_from_slice(&w.to_be_bytes());
        p.extend_from_slice(&h.to_be_bytes());
        p.extend_from_slice(&[8, 2, 0, 0, 0, 0, 0, 0, 0]);
        p
    }

    /// Security review (MEDIUM): a picture whose decoded frames would pass 256 MB is never decoded whole.
    #[test]
    fn a_decompression_bomb_is_over_the_decode_budget() {
        let bomb = gif_bomb(1);
        assert!(bomb.len() < 64, "a tiny file");
        assert!(decoded_size(&bomb).is_some_and(|n| n > DECODE_BUDGET));
        assert!(!fits_decode_budget(&bomb));
        assert!(!fits_decode_budget(&png_header(65_535, 65_535)));
        // Many small frames add up: 1000 frames of 200 x 200 x 4 is 160 MB, 2000 frames pass the budget.
        let small = |n: usize| {
            let mut g = b"GIF89a\xc8\x00\xc8\x00\x00\x00\x00".to_vec();
            for _ in 0..n {
                g.extend_from_slice(&[
                    0x2c, 0, 0, 0, 0, 0xc8, 0, 0xc8, 0, 0, 0x02, 0x02, 0x4c, 0x01, 0x00,
                ]);
            }
            g.push(0x3b);
            g
        };
        assert!(fits_decode_budget(&small(1000)));
        assert!(!fits_decode_budget(&small(2000)));
        assert!(fits_decode_budget(&png(320, 240)));
        assert!(
            !fits_decode_budget(b"GIF89a"),
            "a header without a size is refused"
        );
        assert!(!fits_decode_budget(b"not a picture"));
        // No preview is made from a bomb, and making none is quick.
        let t = std::time::Instant::now();
        assert_eq!(make_thumbnail(&bomb), None);
        assert!(t.elapsed() < std::time::Duration::from_secs(1));
    }

    #[test]
    fn a_thumbnail_is_none_for_bytes_that_are_not_an_image() {
        assert_eq!(make_thumbnail(b"not an image"), None);
    }

    fn descriptor(plain: &[u8]) -> (Descriptor, Vec<u8>) {
        let key = [0x11; 32];
        let nonce = [0x22; 12];
        let stored = seal_blob(&key, &nonce, plain);
        (
            Descriptor {
                blob_id: blob_id(&stored),
                key: Zeroizing::new(key),
                nonce,
                size: plain.len() as u64,
                mime: "image/png".to_owned(),
                w: None,
                h: None,
                thumb: Some(seal_thumb(&key, &nonce, &[0xff, 0xd8, 0xff, 0x00]).unwrap()),
                name: "a.png".to_owned(),
            },
            stored,
        )
    }

    #[test]
    fn download_opens_checks_the_hash_and_reports_missing() {
        let (d, stored) = descriptor(b"hello");
        assert_eq!(&**open_stored(&d, Some(&stored)).unwrap(), b"hello");
        assert_eq!(open_stored(&d, None).err(), Some(AttachmentError::Missing));
        let mut other = stored.clone();
        other[0] ^= 1;
        assert_eq!(
            open_stored(&d, Some(&other)).err(),
            Some(AttachmentError::BlobHash)
        );
    }

    #[test]
    fn download_refuses_a_descriptor_over_the_cap_before_any_request() {
        let (mut d, stored) = descriptor(b"hello");
        d.size = ATTACHMENT_CAP + 1;
        assert_eq!(
            open_stored(&d, Some(&stored)).err(),
            Some(AttachmentError::TooLarge)
        );
    }

    /// FACTS-SECURITY-13: a stored object longer than size + 16 is refused before it is hashed.
    #[test]
    fn download_refuses_a_stored_object_longer_than_its_descriptor_before_hashing() {
        let (d, mut stored) = descriptor(b"hello");
        stored.push(0);
        assert_eq!(
            open_stored(&d, Some(&stored)).err().map(|e| e.code()),
            Some("E_BLOB_OPEN".to_owned()),
            "not E_BLOB_HASH: the length is checked first"
        );
    }

    #[test]
    fn a_thumbnail_is_typed_by_its_signature_and_refused_otherwise() {
        let (d, _) = descriptor(b"x");
        assert_eq!(&**open_thumbnail(&d).unwrap(), &[0xff, 0xd8, 0xff, 0x00]);
        let mut png_thumb = d.clone();
        png_thumb.thumb = Some(seal_thumb(&d.key, &d.nonce, b"\x89PNG....").unwrap());
        assert_eq!(
            open_thumbnail(&png_thumb).err().map(|e| e.code()),
            Some("E_BLOB_OPEN".to_owned())
        );
        let mut none = d.clone();
        none.thumb = None;
        assert_eq!(open_thumbnail(&none).err(), Some(AttachmentError::Missing));
    }

    /// The core's descriptor element, as `Value` would encode it: the reference the hand encoding must equal.
    fn element(d: &Descriptor) -> Value {
        let opt_u = |n: Option<u32>| n.map_or(Value::Null, |n| Value::Uint(u64::from(n)));
        Value::Array(vec![
            Value::Bytes(d.blob_id.to_vec()),
            Value::Bytes(d.key.to_vec()),
            Value::Bytes(d.nonce.to_vec()),
            Value::Uint(d.size),
            Value::Text(d.mime.clone()),
            opt_u(d.w),
            opt_u(d.h),
            d.thumb.clone().map_or(Value::Null, Value::Bytes),
            Value::Text(d.name.clone()),
        ])
    }

    #[test]
    fn a_descriptor_round_trips_through_the_core_encoding() {
        let (d, _) = descriptor(b"abc");
        let raw = element(&d).encode();
        let back = Descriptor::decode(&raw).unwrap();
        assert_eq!(element(&back).encode(), raw);
        assert!(
            !format!("{back:?}").contains("17"),
            "Debug never shows the key"
        );
    }

    /// Fix round 1: the key's copy in the decoded tree is wiped once the descriptor holds it.
    #[test]
    fn decoding_a_descriptor_wipes_the_key_in_the_decoded_tree() {
        let (d, _) = descriptor(b"abc");
        let mut tree = value::decode(&element(&d).encode()).unwrap();
        let back = Descriptor::from_tree(&mut tree).unwrap();
        assert_eq!(*back.key, [0x11; 32]);
        let Value::Array(a) = &tree else {
            panic!("an array")
        };
        assert_eq!(
            a[1].as_bytes(),
            Some(&[0u8; 32][..]),
            "the tree's key is zeroed"
        );
    }

    /// Fix round 1: the send request is the core's encoding, built in one zeroizing buffer that never grows (a grown
    /// buffer leaves its old copy, the key with it, in freed memory).
    #[test]
    fn the_send_request_is_built_by_hand_in_a_zeroizing_buffer() {
        let (mut d, _) = descriptor(b"abc");
        d.w = Some(320);
        d.h = Some(70_000);
        let (e, _) = descriptor(&vec![1u8; 300]);
        let body = "two files ".repeat(40);
        let want = Value::Array(vec![
            Value::Uint(0),
            Value::Null,
            Value::Text(body.clone()),
            Value::Array(vec![element(&d), element(&e)]),
            Value::Null,
        ])
        .encode();
        let (got, capacity) = encode_send(&body, &[d, e], None);
        assert_eq!(*got, want);
        assert_eq!(got.capacity(), capacity, "sized once, never reallocated");
    }

    /// Task 28 item 8 (review M1): a reply with files carries its reply reference in the same request, as the web's one
    /// `send` with `replyTo` and `attachments` (Shell.tsx).
    #[test]
    fn a_reply_with_files_carries_its_reply_reference() {
        let (d, _) = descriptor(b"abc");
        let to = MessageId([0x5a; 16]);
        let want = Value::Array(vec![
            Value::Uint(0),
            Value::Bytes(to.0.to_vec()),
            Value::Text("re".into()),
            Value::Array(vec![element(&d)]),
            Value::Null,
        ])
        .encode();
        let (got, capacity) = encode_send("re", &[d], Some(to));
        assert_eq!(*got, want);
        assert_eq!(got.capacity(), capacity, "sized once, never reallocated");
    }

    /// download.ts / routes.ts `getBlob(.., size + 16)`: the fetch is bounded by the sealed size, and a longer object
    /// (E_BODY_TOO_LARGE) is E_BLOB_OPEN.
    #[tokio::test]
    async fn a_download_is_bounded_by_its_sealed_size() {
        let fake = Arc::new(FakeBlobs::default());
        let (d, stored) = descriptor(b"hello");
        fake.stored.lock().unwrap().insert(d.blob_id, stored);
        let routes: Arc<dyn BlobRoutes> = fake.clone();
        assert_eq!(
            &**fetch(&*routes, ChannelId([7; 16]), &d).await.unwrap(),
            b"hello"
        );
        assert_eq!(*fake.caps.lock().unwrap(), [5 + 16]);
        *fake.fail_get.lock().unwrap() = Some(ApiError::local(200, "E_BODY_TOO_LARGE", ""));
        assert_eq!(
            fetch(&*routes, ChannelId([7; 16]), &d)
                .await
                .err()
                .map(|e| e.code()),
            Some("E_BLOB_OPEN".to_owned())
        );
        let mut big = d.clone();
        big.size = ATTACHMENT_CAP + 1;
        assert_eq!(
            fetch(&*routes, ChannelId([7; 16]), &big).await.err(),
            Some(AttachmentError::TooLarge)
        );
        assert_eq!(
            fake.caps.lock().unwrap().len(),
            2,
            "no request for a file over the cap"
        );
    }

    #[derive(Default)]
    struct FakeBlobs {
        stored: StdMutex<HashMap<[u8; 32], Vec<u8>>>,
        log: StdMutex<Vec<String>>,
        fail_put: StdMutex<Option<ApiError>>,
        fail_get: StdMutex<Option<ApiError>>,
        caps: StdMutex<Vec<u64>>,
    }

    #[async_trait::async_trait]
    impl BlobRoutes for FakeBlobs {
        async fn put(
            &self,
            _c: ChannelId,
            blob: [u8; 32],
            stored: Vec<u8>,
        ) -> Result<(), ApiError> {
            self.log
                .lock()
                .unwrap()
                .push(format!("put {}", &hex(&blob)[..8]));
            if let Some(e) = self.fail_put.lock().unwrap().take() {
                return Err(e);
            }
            self.stored.lock().unwrap().insert(blob, stored);
            Ok(())
        }
        async fn get(
            &self,
            _c: ChannelId,
            blob: [u8; 32],
            cap: u64,
        ) -> Result<Option<Vec<u8>>, ApiError> {
            self.caps.lock().unwrap().push(cap);
            if let Some(e) = self.fail_get.lock().unwrap().take() {
                return Err(e);
            }
            Ok(self.stored.lock().unwrap().get(&blob).cloned())
        }
        async fn delete(&self, _c: ChannelId, blob: [u8; 32]) -> Result<(), ApiError> {
            self.log
                .lock()
                .unwrap()
                .push(format!("delete {}", &hex(&blob)[..8]));
            Err(ApiError::local(
                500,
                "E_INTERNAL",
                "a failed delete is swallowed",
            ))
        }
        async fn confirm(&self, _c: ChannelId, _blob: [u8; 32]) -> Result<(), ApiError> {
            Ok(())
        }
    }

    fn tray(fake: &Arc<FakeBlobs>) -> (Tray, Arc<StdMutex<Vec<Vec<TrayItem>>>>) {
        let seen = Arc::new(StdMutex::new(Vec::new()));
        let s = seen.clone();
        let t = Tray::new(ChannelId([7; 16]), fake.clone(), move |items| {
            s.lock().unwrap().push(items)
        });
        (t, seen)
    }

    fn file(name: &str, bytes: &[u8]) -> NewFile {
        NewFile {
            name: name.to_owned(),
            mime: None,
            size: bytes.len() as u64,
            source: FileSource::Bytes(Zeroizing::new(bytes.to_vec())),
        }
    }

    #[tokio::test]
    async fn a_file_goes_reading_preparing_uploading_ready_and_its_descriptor_opens_the_upload() {
        let fake = Arc::new(FakeBlobs::default());
        let (t, seen) = tray(&fake);
        let ids = t.add(vec![file("n.bin", b"some bytes")]).await.unwrap();
        let phases: Vec<TrayPhase> = seen.lock().unwrap().iter().map(|s| s[0].phase).collect();
        assert_eq!(
            phases,
            [
                TrayPhase::Reading,
                TrayPhase::Preparing,
                TrayPhase::Uploading,
                TrayPhase::Ready
            ]
        );
        let d = t.take(&ids).unwrap().remove(0);
        let stored = fake.stored.lock().unwrap().get(&d.blob_id).cloned();
        assert_eq!(
            &**open_stored(&d, stored.as_deref()).unwrap(),
            b"some bytes"
        );
        assert!(t.entries().is_empty());
    }

    /// Fix round 1: a picked file is read at most one byte past the cap, so `/dev/zero` or a file growing while it is
    /// read fails as too large instead of filling memory.
    #[cfg(unix)]
    #[tokio::test(flavor = "multi_thread")]
    async fn a_file_that_never_ends_is_read_at_most_one_byte_past_the_cap() {
        let fake = Arc::new(FakeBlobs::default());
        let (t, _) = tray(&fake);
        let endless = NewFile::from_path(Path::new("/dev/zero")).unwrap();
        tokio::time::timeout(std::time::Duration::from_secs(20), t.add(vec![endless]))
            .await
            .expect("the read is bounded")
            .unwrap();
        let e = &t.entries()[0];
        assert_eq!(
            (e.phase, e.reason.as_str()),
            (TrayPhase::Failed, "E_ATTACHMENT_TOO_LARGE")
        );
        assert!(fake.log.lock().unwrap().is_empty(), "nothing uploaded");
        assert_eq!(
            read_bounded(Path::new("/dev/zero")).unwrap().len() as u64,
            ATTACHMENT_CAP + 1
        );
    }

    #[tokio::test]
    async fn more_than_ten_files_are_refused_before_any_is_read() {
        let fake = Arc::new(FakeBlobs::default());
        let (t, seen) = tray(&fake);
        let files = (0..11).map(|i| file(&format!("{i}.bin"), b"x")).collect();
        assert_eq!(t.add(files).await, Err(AttachmentError::Count));
        assert!(seen.lock().unwrap().is_empty());
        assert!(fake.log.lock().unwrap().is_empty());
    }

    /// conversation.spec.ts "a 25 MB file … one byte more": the cap is inclusive and the file over it is never read
    /// nor uploaded.
    #[tokio::test]
    async fn an_oversize_file_stays_failed_unread_and_its_sibling_at_the_cap_uploads() {
        let fake = Arc::new(FakeBlobs::default());
        let (t, _) = tray(&fake);
        let over = NewFile {
            name: "over.bin".to_owned(),
            mime: None,
            size: ATTACHMENT_CAP + 1,
            source: FileSource::Path(PathBuf::from("/nonexistent/never-read")),
        };
        let at_cap = file("big.bin", &vec![7u8; ATTACHMENT_CAP as usize]);
        t.add(vec![over, at_cap]).await.unwrap();
        let e = t.entries();
        assert_eq!(
            (e[0].phase, e[0].reason.as_str()),
            (TrayPhase::Failed, "E_ATTACHMENT_TOO_LARGE")
        );
        assert!(!e[0].image);
        assert_eq!(e[1].phase, TrayPhase::Ready);
        assert_eq!(
            fake.log.lock().unwrap().len(),
            1,
            "one upload: the file at the cap"
        );
        assert_eq!(
            t.take(&[e[0].id.clone()]).err(),
            Some(AttachmentError::NotReady)
        );
    }

    #[tokio::test]
    async fn an_image_gets_a_sealed_thumbnail_and_its_size() {
        let fake = Arc::new(FakeBlobs::default());
        let (t, _) = tray(&fake);
        let mut f = file("drawn.png", &png(320, 240));
        f.mime = Some("image/png".to_owned());
        let ids = t.add(vec![f]).await.unwrap();
        let d = t.take(&ids).unwrap().remove(0);
        assert_eq!((d.w, d.h), (Some(320), Some(240)));
        assert!(is_thumbnail_bytes(&open_thumbnail(&d).unwrap()));
    }

    #[tokio::test]
    async fn a_failed_upload_is_marked_with_its_code_and_the_next_file_goes_on() {
        let fake = Arc::new(FakeBlobs::default());
        *fake.fail_put.lock().unwrap() = Some(ApiError::local(413, "E_TOO_LARGE", ""));
        let (t, _) = tray(&fake);
        t.add(vec![file("a.bin", b"a"), file("b.bin", b"b")])
            .await
            .unwrap();
        let e = t.entries();
        assert_eq!(
            (e[0].phase, e[0].reason.as_str()),
            (TrayPhase::Failed, "E_TOO_LARGE")
        );
        assert_eq!(e[1].phase, TrayPhase::Ready);
    }

    #[tokio::test]
    async fn an_untyped_file_is_octet_stream_and_its_name_is_sanitised() {
        let fake = Arc::new(FakeBlobs::default());
        let (t, _) = tray(&fake);
        t.add(vec![file("../x\u{202e}", b"a")]).await.unwrap();
        let e = &t.entries()[0];
        assert_eq!(
            (e.mime.as_str(), e.name.as_str()),
            ("application/octet-stream", ".._x")
        );
    }

    #[tokio::test]
    async fn discard_deletes_an_uploaded_reference_and_swallows_a_failed_delete() {
        let fake = Arc::new(FakeBlobs::default());
        let (t, _) = tray(&fake);
        let ids = t.add(vec![file("a.bin", b"a")]).await.unwrap();
        t.discard(&ids[0]).await;
        assert!(t.entries().is_empty());
        let log = fake.log.lock().unwrap().clone();
        assert_eq!(log.len(), 2);
        assert!(log[1].starts_with("delete"));
        t.discard("unknown").await;
    }

    #[tokio::test]
    async fn take_answers_in_the_order_asked_and_refuses_duplicates_or_unready_removing_nothing() {
        let fake = Arc::new(FakeBlobs::default());
        let (t, _) = tray(&fake);
        let ids = t
            .add(vec![file("a.bin", b"a"), file("b.bin", b"bb")])
            .await
            .unwrap();
        assert_eq!(
            t.take(&[ids[0].clone(), ids[0].clone()]).err(),
            Some(AttachmentError::NotReady)
        );
        assert_eq!(
            t.take(&[ids[0].clone(), "9".to_owned()]).err(),
            Some(AttachmentError::NotReady)
        );
        assert_eq!(t.entries().len(), 2, "nothing was removed");
        let got = t.take(&[ids[1].clone(), ids[0].clone()]).unwrap();
        assert_eq!(got.iter().map(|d| d.size).collect::<Vec<_>>(), [2, 1]);
    }
}
