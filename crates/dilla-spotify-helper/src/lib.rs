use serde::{Deserialize, Serialize};
use std::{
    fmt,
    io::{self, Read, Write},
    process::Child,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
};
use zeroize::Zeroize;

pub const MAX_CONTROL_BYTES: usize = 4096;
pub const MAX_CREDENTIAL_BYTES: usize = 64 * 1024;
/// One 20 ms frame of interleaved signed 16-bit little-endian PCM at 48 kHz stereo.
pub const PCM_BYTES_PER_FRAME: usize = 48_000 * 2 * 2 / 50;
const PCM_MAGIC: &[u8; 4] = b"DPCM";
const PCM_VERSION: u8 = 1;

pub struct Secret(Vec<u8>);
impl Secret {
    pub fn expose(&self) -> &[u8] {
        &self.0
    }
}
impl fmt::Debug for Secret {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("Secret([REDACTED])")
    }
}
impl Drop for Secret {
    fn drop(&mut self) {
        self.0.zeroize();
    }
}

/// Reads credential bytes from an already-open inherited descriptor. The caller must
/// arrange descriptor inheritance; the secret is never sourced from argv or environment.
pub const CREDENTIAL_FD: i32 = 3;

/// Reads the credential channel from the supervisor-provided inherited descriptor 3.
/// The descriptor number is part of the process contract and is not supplied by argv/env.
pub fn read_inherited_credential() -> io::Result<Secret> {
    struct FdReader;
    impl Read for FdReader {
        fn read(&mut self, buffer: &mut [u8]) -> io::Result<usize> {
            nix::unistd::read(CREDENTIAL_FD, buffer).map_err(io::Error::from)
        }
    }
    read_credential(FdReader, MAX_CREDENTIAL_BYTES)
}

pub fn read_credential(mut input: impl Read, max_bytes: usize) -> io::Result<Secret> {
    if max_bytes == 0 || max_bytes > MAX_CREDENTIAL_BYTES {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "invalid credential bound",
        ));
    }
    let mut bytes = Vec::with_capacity(max_bytes.min(4096));
    if let Err(err) = input
        .by_ref()
        .take((max_bytes + 1) as u64)
        .read_to_end(&mut bytes)
    {
        bytes.zeroize();
        return Err(err);
    }
    if bytes.is_empty() || bytes.len() > max_bytes {
        bytes.zeroize();
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "credential unavailable",
        ));
    }
    Ok(Secret(bytes))
}

/// The runtime sends only the current OAuth access token through FD 3. Keep
/// provider refresh tokens outside the player process.
pub fn parse_access_token(input: &[u8]) -> io::Result<String> {
    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    struct AccessToken {
        access_token: String,
    }
    let value: AccessToken = serde_json::from_slice(input)
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "invalid credential"))?;
    if value.access_token.is_empty()
        || value.access_token.len() > 8192
        || !value
            .access_token
            .bytes()
            .all(|byte| (0x21..=0x7e).contains(&byte))
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "invalid credential",
        ));
    }
    Ok(value.access_token)
}

pub fn write_control(mut output: impl Write, json: &[u8]) -> io::Result<()> {
    if json.is_empty() || json.len() > MAX_CONTROL_BYTES {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "invalid control frame",
        ));
    }
    parse_control(json)
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "invalid control"))?;
    output.write_all(&(json.len() as u32).to_be_bytes())?;
    output.write_all(json)
}

pub fn read_control(mut input: impl Read) -> io::Result<Vec<u8>> {
    let mut length = [0; 4];
    input.read_exact(&mut length)?;
    let length = u32::from_be_bytes(length) as usize;
    if length == 0 || length > MAX_CONTROL_BYTES {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "invalid control frame",
        ));
    }
    let mut bytes = vec![0; length];
    input.read_exact(&mut bytes)?;
    parse_control(&bytes)
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "invalid control"))?;
    Ok(bytes)
}

#[derive(Debug, PartialEq, Eq)]
pub enum Control {
    Pause,
    Resume,
    Stop,
    Seek { position_ms: u32 },
    Volume { value: u8 },
}

pub fn parse_control(bytes: &[u8]) -> Result<Control, &'static str> {
    if bytes.is_empty() || bytes.len() > MAX_CONTROL_BYTES {
        return Err("invalid control");
    }
    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Wire {
        op: String,
        position_ms: Option<u32>,
        value: Option<u8>,
    }
    let wire: Wire = serde_json::from_slice(bytes).map_err(|_| "invalid control")?;
    let value: serde_json::Value = serde_json::from_slice(bytes).map_err(|_| "invalid control")?;
    let object = value.as_object().ok_or("invalid control")?;
    match (wire.op.as_str(), wire.position_ms, wire.value) {
        ("pause", None, None) if object.len() == 1 => Ok(Control::Pause),
        ("resume", None, None) if object.len() == 1 => Ok(Control::Resume),
        ("stop", None, None) if object.len() == 1 => Ok(Control::Stop),
        ("seek", Some(n), None)
            if object.len() == 2
                && object
                    .get("position_ms")
                    .is_some_and(serde_json::Value::is_u64) =>
        {
            Ok(Control::Seek { position_ms: n })
        }
        ("volume", None, Some(n))
            if n <= 100
                && object.len() == 2
                && object.get("value").is_some_and(serde_json::Value::is_u64) =>
        {
            Ok(Control::Volume { value: n })
        }
        _ => Err("invalid control"),
    }
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Phase {
    Startup,
    Authorization,
    Playback,
    Control,
    Shutdown,
}
#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ErrorCode {
    InvalidRequest,
    CredentialUnavailable,
    ProviderUnavailable,
    Cancelled,
    Internal,
}
#[derive(Clone, Debug, Deserialize, PartialEq, Eq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Status {
    pub ok: bool,
    pub phase: Phase,
    pub code: Option<ErrorCode>,
}
impl Status {
    pub fn success(phase: Phase) -> Self {
        Self {
            ok: true,
            phase,
            code: None,
        }
    }
    pub fn failure(phase: Phase, code: ErrorCode) -> Self {
        Self {
            ok: false,
            phase,
            code: Some(code),
        }
    }
    pub fn encode(&self) -> io::Result<Vec<u8>> {
        if self.ok == self.code.is_some() {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "invalid status",
            ));
        }
        let bytes = serde_json::to_vec(self).map_err(io::Error::other)?;
        if bytes.len() > 256 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "status too large",
            ));
        }
        let mut framed = Vec::with_capacity(bytes.len() + 4);
        framed.extend_from_slice(&(bytes.len() as u32).to_be_bytes());
        framed.extend_from_slice(&bytes);
        Ok(framed)
    }
    pub fn decode(frame: &[u8]) -> io::Result<Self> {
        if frame.len() < 4 {
            return Err(io::Error::new(io::ErrorKind::UnexpectedEof, "status frame"));
        }
        let n = u32::from_be_bytes(frame[..4].try_into().unwrap()) as usize;
        if n > 256 || n != frame.len() - 4 {
            return Err(io::Error::new(io::ErrorKind::InvalidData, "status frame"));
        }
        let status: Self = serde_json::from_slice(&frame[4..])
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "status frame"))?;
        if status.ok == status.code.is_some() {
            return Err(io::Error::new(io::ErrorKind::InvalidData, "status frame"));
        }
        Ok(status)
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
/// Raw 48 kHz stereo s16le samples, exactly 20 ms (960 samples per channel).
pub struct PcmFrame(Vec<u8>);
impl PcmFrame {
    pub fn new(bytes: Vec<u8>) -> io::Result<Self> {
        if bytes.len() != PCM_BYTES_PER_FRAME {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "invalid pcm frame",
            ));
        }
        Ok(Self(bytes))
    }
    pub fn as_bytes(&self) -> &[u8] {
        &self.0
    }
}
pub fn write_pcm(mut output: impl Write, frame: &PcmFrame) -> io::Result<()> {
    output.write_all(PCM_MAGIC)?;
    output.write_all(&[PCM_VERSION])?;
    output.write_all(&(frame.0.len() as u32).to_be_bytes())?;
    output.write_all(&frame.0)
}
pub fn read_pcm(mut input: impl Read) -> io::Result<PcmFrame> {
    let mut header = [0; 9];
    input.read_exact(&mut header)?;
    let n = u32::from_be_bytes(header[5..9].try_into().unwrap()) as usize;
    if &header[..4] != PCM_MAGIC || header[4] != PCM_VERSION || n != PCM_BYTES_PER_FRAME {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "invalid pcm frame",
        ));
    }
    let mut bytes = vec![0; n];
    input.read_exact(&mut bytes)?;
    PcmFrame::new(bytes)
}

#[derive(Clone, Default)]
pub struct Cancellation(Arc<AtomicBool>);
impl Cancellation {
    pub fn new() -> Self {
        Self::default()
    }
    pub fn cancel(&self) {
        self.0.store(true, Ordering::Release);
    }
    pub fn is_cancelled(&self) -> bool {
        self.0.load(Ordering::Acquire)
    }
}

/// Reaps the directly-owned child. Process-tree cleanup is the caller's
/// responsibility until the provider supervisor places children in a cgroup.
pub struct ChildGuard<'a> {
    child: &'a mut Child,
}
impl<'a> ChildGuard<'a> {
    pub fn new(child: &'a mut Child) -> Self {
        Self { child }
    }
    pub fn disarm(self) {
        std::mem::forget(self);
    }
}
impl Drop for ChildGuard<'_> {
    fn drop(&mut self) {
        if self.child.try_wait().ok().flatten().is_none() {
            let _ = self.child.kill();
        }
        let _ = self.child.wait();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Cursor;

    #[test]
    fn credential_input_is_bounded_and_secret_wrapper_does_not_debug_contents() {
        let secret = b"sensitive-token";
        let value = read_credential(Cursor::new(secret), 64).unwrap();
        assert_eq!(value.expose(), secret);
        assert!(!format!("{value:?}").contains("sensitive-token"));
        assert!(read_credential(Cursor::new(vec![b'x'; 65]), 64).is_err());
        assert!(read_credential(Cursor::new([]), 64).is_err());
    }

    #[test]
    fn access_token_credential_is_strict_and_carries_no_refresh_token() {
        assert_eq!(
            parse_access_token(br#"{"access_token":"BQD_abc-123"}"#).unwrap(),
            "BQD_abc-123"
        );
        for invalid in [
            br#"{"access_token":""}"#.as_slice(),
            br#"{"access_token":"a b"}"#,
            br#"{"access_token":"abc","refresh_token":"secret"}"#,
            br#"{"access_token":"abc"} {}"#,
        ] {
            assert!(parse_access_token(invalid).is_err());
        }
    }

    #[test]
    fn control_ipc_is_length_prefixed_bounded_and_rejects_trailing_json() {
        let mut frame = Vec::new();
        write_control(&mut frame, br#"{"op":"pause"}"#).unwrap();
        assert_eq!(
            read_control(&mut Cursor::new(frame)).unwrap(),
            br#"{"op":"pause"}"#
        );
        assert!(
            read_control(&mut Cursor::new(
                9u32.to_be_bytes()
                    .into_iter()
                    .chain(b"{}".iter().copied())
                    .collect::<Vec<_>>()
            ))
            .is_err()
        );
        assert!(parse_control(br#"{"op":"pause"} {}"#).is_err());
    }

    #[test]
    fn status_frames_are_deterministic_and_errors_are_redacted() {
        let status = Status::failure(Phase::Playback, ErrorCode::ProviderUnavailable);
        let bytes = status.encode().unwrap();
        assert_eq!(bytes, status.encode().unwrap());
        assert!(!String::from_utf8_lossy(&bytes).contains("token"));
        assert_eq!(Status::decode(&bytes).unwrap(), status);
        assert!(
            Status {
                ok: true,
                phase: Phase::Playback,
                code: Some(ErrorCode::Internal)
            }
            .encode()
            .is_err()
        );
        assert!(
            Status {
                ok: false,
                phase: Phase::Playback,
                code: None
            }
            .encode()
            .is_err()
        );
    }

    #[test]
    fn pcm_frame_requires_exactly_one_twenty_millisecond_stereo_chunk() {
        let frame = PcmFrame::new(vec![0u8; PCM_BYTES_PER_FRAME]).unwrap();
        let mut bytes = Vec::new();
        write_pcm(&mut bytes, &frame).unwrap();
        assert_eq!(read_pcm(&mut Cursor::new(bytes)).unwrap(), frame);
        assert!(PcmFrame::new(vec![0; PCM_BYTES_PER_FRAME - 2]).is_err());
    }

    #[test]
    fn pcm_writer_matches_shared_go_consumer_fixture() {
        let frame = PcmFrame::new(vec![0u8; PCM_BYTES_PER_FRAME]).unwrap();
        let fixture = include_bytes!("../testdata/pcm-v1.frame");
        let mut encoded = Vec::new();
        write_pcm(&mut encoded, &frame).unwrap();
        assert_eq!(encoded.as_slice(), fixture.as_slice());
        assert_eq!(read_pcm(&mut Cursor::new(fixture)).unwrap(), frame);
    }

    #[test]
    fn cancellation_is_observable_and_child_guard_kills_on_drop() {
        let token = Cancellation::new();
        token.cancel();
        assert!(token.is_cancelled());
        let mut child = std::process::Command::new("sh")
            .args(["-c", "sleep 30"])
            .spawn()
            .unwrap();
        {
            let _guard = ChildGuard::new(&mut child);
        }
        assert!(child.try_wait().unwrap().is_some());
    }
}
