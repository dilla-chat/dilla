use librespot::{
    core::{Session, SpotifyUri, authentication::Credentials, config::SessionConfig},
    playback::{
        audio_backend::{Sink, SinkError, SinkResult},
        config::PlayerConfig,
        convert::Converter,
        decoder::AudioPacket,
        mixer::VolumeGetter,
        player::{Player, PlayerEvent},
    },
};
use serde::{Deserialize, Serialize};
use std::{
    io::{Read, Write},
    os::fd::FromRawFd,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

const PCM: usize = 3528;
const MAX_CREDENTIAL: usize = 64 * 1024;
const MAX_CONTROL: usize = 4096;
// The queue admits tracks up to 24 hours. Leave room for startup/buffering;
// the parent still cancels promptly when the voice session ends.
const MAX_TRACK_WALL_TIME: Duration = Duration::from_secs(25 * 60 * 60);
const PCM_HEADER: &[u8; 9] = b"DPCM\x01\x00\x00\x0d\xc8";

#[derive(Serialize)]
struct Status {
    ok: bool,
    phase: &'static str,
    #[serde(skip_serializing_if = "Option::is_none")]
    code: Option<&'static str>,
}
fn status(ok: bool, phase: &'static str, code: Option<&'static str>) {
    if let Some(mut frame) = encode_status(Status { ok, phase, code }) {
        let mut stderr = std::io::stderr().lock();
        let _ = stderr.write_all(&frame);
        let _ = stderr.flush();
        frame.fill(0);
    }
}

fn encode_status(status: Status) -> Option<Vec<u8>> {
    let body = serde_json::to_vec(&status).ok()?;
    if body.is_empty() || body.len() > 256 {
        return None;
    }
    let mut frame = Vec::with_capacity(4 + body.len());
    frame.extend_from_slice(&(body.len() as u32).to_be_bytes());
    frame.extend_from_slice(&body);
    Some(frame)
}
fn fail(phase: &'static str, code: &'static str) {
    status(false, phase, Some(code));
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Control {
    op: String,
    #[serde(default)]
    position_ms: Option<u32>,
}
fn controls() -> std::sync::mpsc::Receiver<Result<Control, ()>> {
    let (tx, rx) = std::sync::mpsc::sync_channel(4);
    std::thread::spawn(move || {
        let stdin = std::io::stdin();
        let mut input = stdin.lock();
        loop {
            let mut h = [0; 4];
            if input.read_exact(&mut h).is_err() {
                let _ = tx.send(Err(()));
                break;
            }
            let n = u32::from_be_bytes(h) as usize;
            if n == 0 || n > MAX_CONTROL {
                let _ = tx.send(Err(()));
                break;
            }
            let mut b = vec![0; n];
            if input.read_exact(&mut b).is_err() {
                let _ = tx.send(Err(()));
                break;
            }
            let parsed = parse_control(&b);
            b.fill(0);
            let stop = parsed.as_ref().is_ok_and(|c| c.op == "stop");
            if tx.send(parsed).is_err() || stop {
                break;
            }
        }
    });
    rx
}

fn parse_control(bytes: &[u8]) -> Result<Control, ()> {
    if bytes.is_empty() || bytes.len() > MAX_CONTROL {
        return Err(());
    }
    let control: Control = serde_json::from_slice(bytes).map_err(|_| ())?;
    match (control.op.as_str(), control.position_ms) {
        ("pause" | "resume" | "stop", None) => Ok(control),
        ("seek", Some(_)) => Ok(control),
        _ => Err(()),
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AccessTokenFile {
    access_token: String,
}

fn parse_credential(bytes: &[u8]) -> Result<Credentials, ()> {
    let token: AccessTokenFile = serde_json::from_slice(bytes).map_err(|_| ())?;
    if token.access_token.is_empty()
        || token.access_token.len() > 8192
        || !token
            .access_token
            .bytes()
            .all(|c| (0x21..=0x7e).contains(&c))
    {
        return Err(());
    }
    Ok(Credentials::with_access_token(token.access_token))
}
fn credential() -> Result<Credentials, ()> {
    let f = unsafe { std::fs::File::from_raw_fd(3) };
    let mut bytes = Vec::new();
    f.take((MAX_CREDENTIAL + 1) as u64)
        .read_to_end(&mut bytes)
        .map_err(|_| ())?;
    if bytes.is_empty() || bytes.len() > MAX_CREDENTIAL {
        bytes.fill(0);
        return Err(());
    }
    let result = parse_credential(&bytes);
    bytes.fill(0);
    result
}
struct Vol;
impl VolumeGetter for Vol {
    fn attenuation_factor(&self) -> f64 {
        1.0
    }
}
struct Audio {
    frame: Vec<u8>,
    first: bool,
    cancelled: Arc<AtomicBool>,
}
impl Sink for Audio {
    fn write(&mut self, packet: AudioPacket, converter: &mut Converter) -> SinkResult<()> {
        let AudioPacket::Samples(samples) = packet else {
            return Err(SinkError::OnWrite("unsupported audio packet".into()));
        };
        let converted = converter.f64_to_s16(&samples);
        for s in converted {
            self.frame.extend_from_slice(&s.to_le_bytes());
            if self.frame.len() == PCM {
                self.flush_frame()?;
            }
        }
        Ok(())
    }
}
impl Audio {
    fn flush_frame(&mut self) -> SinkResult<()> {
        if self.cancelled.load(Ordering::Acquire) {
            return Err(SinkError::OnWrite("cancelled".into()));
        }
        let mut out = std::io::stdout().lock();
        let result = (|| -> std::io::Result<()> {
            out.write_all(PCM_HEADER)?;
            out.write_all(&self.frame)?;
            out.flush()
        })();
        if result.is_err() {
            self.cancelled.store(true, Ordering::Release);
            return Err(SinkError::OnWrite("output unavailable".into()));
        }
        self.frame.clear();
        if !self.first {
            self.first = true;
            status(true, "playback", None);
        }
        Ok(())
    }
}

// Only PlayerEvent::Unavailable is track-specific. Connection, credentials,
// build and timeout faults must leave the user's queued item available to retry.
#[derive(Clone, Copy)]
enum Failure {
    InvalidRequest,
    CredentialUnavailable,
    ProviderConnect,
    ProviderConnectTimeout,
    BuildConfiguration,
    TrackUnavailable,
    PlaybackTimeout,
    Cancelled,
}
impl Failure {
    fn status(self) -> Status {
        let (phase, code) = match self {
            Self::InvalidRequest => ("control", "invalid_request"),
            Self::CredentialUnavailable => ("authorization", "credential_unavailable"),
            Self::ProviderConnect => ("authorization", "provider_connect"),
            Self::ProviderConnectTimeout => ("authorization", "provider_connect_timeout"),
            Self::BuildConfiguration => ("startup", "build_configuration"),
            Self::TrackUnavailable => ("playback", "provider_unavailable"),
            Self::PlaybackTimeout => ("playback", "playback_timeout"),
            Self::Cancelled => ("shutdown", "cancelled"),
        };
        Status {
            ok: false,
            phase,
            code: Some(code),
        }
    }
}

async fn run() -> Result<(), Failure> {
    use std::{env, io::IsTerminal};
    if librespot::playback::SAMPLE_RATE != 44_100 || librespot::playback::NUM_CHANNELS != 2 {
        return Err(Failure::BuildConfiguration);
    }
    if env::args().len() != 2 || std::io::stdout().is_terminal() {
        return Err(Failure::InvalidRequest);
    }
    let arg = env::args().nth(1).ok_or(Failure::InvalidRequest)?;
    let id = arg
        .strip_prefix("spotify:track:")
        .ok_or(Failure::InvalidRequest)?;
    if id.len() != 22 || !id.bytes().all(|c| c.is_ascii_alphanumeric()) {
        return Err(Failure::InvalidRequest);
    }
    let creds = credential().map_err(|_| Failure::CredentialUnavailable)?;
    status(true, "startup", None);
    let session = Session::new(SessionConfig::default(), None);
    match tokio::time::timeout(Duration::from_secs(35), session.connect(creds, false)).await {
        Ok(Ok(())) => {}
        Ok(Err(_)) => {
            session.shutdown();
            return Err(Failure::ProviderConnect);
        }
        Err(_) => {
            session.shutdown();
            return Err(Failure::ProviderConnectTimeout);
        }
    }
    status(true, "authorization", None);
    let uri = SpotifyUri::from_uri(&arg).map_err(|_| Failure::InvalidRequest)?;
    let cancelled = Arc::new(AtomicBool::new(false));
    let sink_cancel = cancelled.clone();
    let player = Player::new(
        PlayerConfig::default(),
        session.clone(),
        Box::new(Vol),
        move || {
            Box::new(Audio {
                frame: Vec::with_capacity(PCM),
                first: false,
                cancelled: sink_cancel.clone(),
            }) as Box<dyn Sink>
        },
    );
    let mut events = player.get_player_event_channel();
    let input = controls();
    player.load(uri, true, 0);
    let end = tokio::time::Instant::now() + MAX_TRACK_WALL_TIME;
    let outcome = loop {
        tokio::select! {
            _=tokio::time::sleep_until(end)=>break Err(Failure::PlaybackTimeout),
            ev=events.recv()=>match ev { Some(PlayerEvent::EndOfTrack{..})=>break Ok(()), Some(PlayerEvent::Unavailable{..})=>break Err(Failure::TrackUnavailable), _=>{} },
            _=tokio::time::sleep(Duration::from_millis(10))=>match input.try_recv() {
                Ok(Ok(c))=>match (c.op.as_str(),c.position_ms) { ("pause",None)=>player.pause(),("resume",None)=>player.play(),("seek",Some(ms))=>player.seek(ms),("stop",None)=>{status(true,"control",None);break Ok(())}, _=>break Err(Failure::InvalidRequest) },
                Ok(Err(()))|Err(std::sync::mpsc::TryRecvError::Disconnected)=>{cancelled.store(true,Ordering::Release);break Err(Failure::Cancelled)}, _=>{}
            }
        }
    };
    cancelled.store(true, Ordering::Release);
    player.stop();
    drop(player);
    session.shutdown();
    outcome
}
#[tokio::main]
async fn main() {
    if let Err(failure) = run().await {
        let result = failure.status();
        fail(
            result.phase,
            result.code.expect("failure has a bounded code"),
        );
        std::process::exit(1);
    } else {
        status(true, "shutdown", None);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn status_frames_fit_adapter_limit_and_wire_fields() {
        let frame = encode_status(Status {
            ok: false,
            phase: "authorization",
            code: Some("credential_unavailable"),
        })
        .unwrap();
        let n = u32::from_be_bytes(frame[..4].try_into().unwrap()) as usize;
        assert_eq!(n, frame.len() - 4);
        assert!(n <= 256);
        let decoded: serde_json::Value = serde_json::from_slice(&frame[4..]).unwrap();
        assert_eq!(decoded["ok"], false);
        assert_eq!(decoded["phase"], "authorization");
        assert_eq!(decoded["code"], "credential_unavailable");
    }
    #[test]
    fn pcm_header_declares_exact_contract() {
        assert_eq!(&PCM_HEADER[..4], b"DPCM");
        assert_eq!(PCM_HEADER[4], 1);
        assert_eq!(
            u32::from_be_bytes(PCM_HEADER[5..9].try_into().unwrap()),
            PCM as u32
        );
        assert_eq!(PCM, 3528);
    }
    #[test]
    fn accepts_adapter_controls_and_rejects_malformed_frames() {
        assert_eq!(parse_control(br#"{"op":"pause"}"#).unwrap().op, "pause");
        assert_eq!(
            parse_control(br#"{"op":"seek","position_ms":1234}"#)
                .unwrap()
                .position_ms,
            Some(1234)
        );
        for bad in [
            br#"{"op":"pause","x":1}"#.as_slice(),
            br#"{"op":"seek"}"#,
            br#"{"op":"resume","position_ms":1}"#,
            br#"{}"#,
        ] {
            assert!(parse_control(bad).is_err());
        }
    }

    #[test]
    fn accepts_only_runtime_access_token_descriptor() {
        let credential = parse_credential(br#"{"access_token":"test-token"}"#).unwrap();
        assert_eq!(credential.auth_data, b"test-token");
        for bad in [
            br#"{"access_token":""}"#.as_slice(),
            br#"{"access_token":"test-token","refresh_token":"secret"}"#,
            br#"{"username":"somebody","auth_type":0,"auth_data":"AA=="}"#,
            br#"{"access_token":"bad\nvalue"}"#,
        ] {
            assert!(parse_credential(bad).is_err());
        }
    }
}

#[cfg(test)]
mod failure_tests {
    use super::*;

    #[test]
    fn only_track_unavailable_uses_permanent_playback_code() {
        for (failure, phase, code) in [
            (
                Failure::ProviderConnect,
                "authorization",
                "provider_connect",
            ),
            (
                Failure::ProviderConnectTimeout,
                "authorization",
                "provider_connect_timeout",
            ),
            (
                Failure::BuildConfiguration,
                "startup",
                "build_configuration",
            ),
            (Failure::PlaybackTimeout, "playback", "playback_timeout"),
            (
                Failure::TrackUnavailable,
                "playback",
                "provider_unavailable",
            ),
        ] {
            let frame = encode_status(failure.status()).unwrap();
            let decoded: serde_json::Value = serde_json::from_slice(&frame[4..]).unwrap();
            assert_eq!(decoded["ok"], false);
            assert_eq!(decoded["phase"], phase);
            assert_eq!(decoded["code"], code);
        }
    }
}
