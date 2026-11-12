//! One desktop call participant (`dilla_media::Call`), driven over stdin/stdout by the live tests
//! (tests/live.rs) and the browser interop spec (browser/desktop-call.spec.ts).
//!
//! ```text
//! call-peer --url WS --token JWT [--tone HZ] [--noise off|standard|enhanced]
//! ```
//!
//! It publishes a PCM microphone playing a sine at `--tone` (none: listen only) and plays nothing.
//!
//! stdin, one command per line:
//!   epoch <epoch> <base key hex> <own leaf> <leaf>:<device hex>,...   the call group's keys
//!   mute 0|1
//!   stats                                     prints {"event":"stats",...}
//!   quit
//! stdout: one JSON object per line ({"event": ...}).

use std::f64::consts::TAU;
use std::sync::Arc;
use std::time::Duration;

use dilla_media::noise::{FRAME_SAMPLES, NoiseMode, RATE};
use dilla_media::{Call, CallOptions, EpochFeed, LiveKitToken, MediaEpoch, Mic};
use serde_json::json;
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::sync::mpsc;
use zeroize::Zeroizing;

fn emit(v: serde_json::Value) {
    println!("{v}");
}

fn unhex<const N: usize>(s: &str) -> Option<[u8; N]> {
    if s.len() != 2 * N {
        return None;
    }
    let mut out = [0u8; N];
    for (i, b) in out.iter_mut().enumerate() {
        *b = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).ok()?;
    }
    Some(out)
}

fn parse_epoch(parts: &[&str]) -> Option<MediaEpoch> {
    let [epoch, key, own, roster] = parts else {
        return None;
    };
    let roster = roster
        .split(',')
        .map(|e| {
            let (l, d) = e.split_once(':')?;
            Some((l.parse().ok()?, unhex::<16>(d)?))
        })
        .collect::<Option<Vec<_>>>()?;
    Some(MediaEpoch {
        epoch: epoch.parse().ok()?,
        base_key: Zeroizing::new(unhex::<16>(key)?),
        roster,
        own_leaf: own.parse().ok()?,
    })
}

/// The power of `hz` in `samples` (Goertzel), normalised by the sample count.
fn power_at(samples: &[f32], hz: f64) -> f64 {
    let w = TAU * hz / f64::from(RATE);
    let coeff = 2.0 * w.cos();
    let (mut s1, mut s2) = (0.0f64, 0.0f64);
    for &x in samples {
        let s0 = f64::from(x) + coeff * s1 - s2;
        s2 = s1;
        s1 = s0;
    }
    (s1 * s1 + s2 * s2 - coeff * s1 * s2) / (samples.len().max(1) as f64).powi(2)
}

/// The frequency with the most power between 100 Hz and 4 kHz, in 10 Hz steps.
fn peak_hz(samples: &[i16]) -> f64 {
    let x: Vec<f32> = samples.iter().map(|s| f32::from(*s) / 32768.0).collect();
    let mut best = (0.0, f64::MIN);
    let mut hz = 100.0;
    while hz <= 4_000.0 {
        let p = power_at(&x, hz);
        if p > best.1 {
            best = (hz, p);
        }
        hz += 10.0;
    }
    best.0
}

#[tokio::main]
async fn main() {
    let mut url = String::new();
    let mut token = String::new();
    let mut tone: Option<f64> = None;
    let mut noise = NoiseMode::Off;
    let mut args = std::env::args().skip(1);
    while let Some(k) = args.next() {
        let v = args.next().unwrap_or_else(|| panic!("{k} needs a value"));
        match k.as_str() {
            "--url" => url = v,
            "--token" => token = v,
            "--tone" => tone = Some(v.parse().expect("--tone")),
            "--noise" => {
                noise = match v.as_str() {
                    "off" => NoiseMode::Off,
                    "standard" => NoiseMode::Standard,
                    "enhanced" => NoiseMode::Enhanced,
                    _ => panic!("--noise off|standard|enhanced"),
                }
            }
            _ => panic!("unknown argument {k}"),
        }
    }

    let feed = EpochFeed::new();
    let (lines_tx, mut lines) = mpsc::unbounded_channel::<String>();
    // Keys can arrive before the join returns: the join waits for the first one.
    {
        let feed = feed.clone();
        tokio::spawn(async move {
            let mut stdin = BufReader::new(tokio::io::stdin()).lines();
            while let Ok(Some(line)) = stdin.next_line().await {
                let parts: Vec<&str> = line.split_whitespace().collect();
                if parts.first() == Some(&"epoch") {
                    match parse_epoch(&parts[1..]) {
                        Some(e) => {
                            let n = e.epoch;
                            feed.publish(e);
                            emit(json!({"event": "epoch", "epoch": n}));
                        }
                        None => emit(json!({"event": "error", "line": line})),
                    }
                } else if lines_tx.send(line).is_err() {
                    break;
                }
            }
        });
    }

    let mic = match tone {
        Some(hz) => {
            let (tx, rx) = mpsc::channel::<Vec<i16>>(50);
            tokio::spawn(async move {
                let mut n: u64 = 0;
                let mut tick = tokio::time::interval(Duration::from_millis(10));
                loop {
                    tick.tick().await;
                    let frame: Vec<i16> = (0..FRAME_SAMPLES)
                        .map(|_| {
                            let t = n as f64 / f64::from(RATE);
                            n += 1;
                            ((TAU * hz * t).sin() * 0.5 * f64::from(i16::MAX)) as i16
                        })
                        .collect();
                    if tx.send(frame).await.is_err() {
                        break;
                    }
                }
            });
            Mic::Pcm { frames: rx, noise }
        }
        None => Mic::None,
    };
    let call = match Call::join_with(
        LiveKitToken {
            url,
            token: Zeroizing::new(token),
            ice_servers: Vec::new(),
        },
        feed.clone() as Arc<dyn dilla_media::EpochKeys>,
        CallOptions {
            mic,
            playout: false,
            key_timeout: Duration::from_secs(30),
        },
    )
    .await
    {
        Ok(call) => call,
        Err(e) => {
            emit(json!({"event": "failed", "error": e.to_string()}));
            std::process::exit(2);
        }
    };
    emit(json!({"event": "connected", "identity": call.identity()}));

    while let Some(line) = lines.recv().await {
        let parts: Vec<&str> = line.split_whitespace().collect();
        match parts.as_slice() {
            ["mute", on] => {
                call.set_muted(*on == "1");
                emit(json!({"event": "muted", "on": *on == "1"}));
            }
            ["stats"] => {
                let stats = call.stats();
                let levels = call.remote_audio_levels().borrow().clone();
                let peaks: serde_json::Map<String, serde_json::Value> = levels
                    .keys()
                    .filter_map(|d| {
                        let tail = call.remote_audio_tail(d)?;
                        Some((d.clone(), json!(peak_hz(&tail))))
                    })
                    .collect();
                emit(json!({"event": "stats", "media": stats, "levels": levels, "peaks": peaks}));
            }
            ["quit"] => break,
            _ => emit(json!({"event": "error", "line": line})),
        }
    }
    call.leave().await;
    emit(json!({"event": "closed"}));
}
