//! Task 19's client tests for the voice settings area (standard procedure S2).
//!
//! The pure rules (the voice-activity gate, the speaking detector, push to talk, per-person volume, the preference keys)
//! have unit tests beside their code (`src/features/voice_prefs/`), ported from the web's: `sensitivity.ts`,
//! `speaking.ts`, `ptt.ts`, `volume.ts`, `voice-prefs.ts`. This file tests what the area publishes through a running
//! client, against a real dillad: the values a person set are published into `AppState::voice_prefs`, survive the client's
//! restart, and a client that never touched them shows the defaults.
//!
//! Needs `target/dilla-testhost` and the WASI core (see `tests/sync.rs`); `#[ignore]`d so a plain `cargo test` needs no Go
//! toolchain.

use std::time::Duration;

use dilla_client::features::voice_prefs::{InputMode, NoisePreference, Setting, VoiceValues};
use dilla_client::state::voice_prefs::{AudioDevice, AudioDevices, AudioNotice, MicTestState};
use dilla_client::testing::host::{sign_up, start, start_host, wait_for};

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn voice_prefs_publish_and_survive_a_restart() {
    let host = tokio::task::spawn_blocking(|| start_host("voice-prefs", "127.0.0.1:0", None))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let alice = sign_up(&host, &invite, "alice", "alice", "voice-prefs").await;
    let client = start(&alice).await;

    // A client that never touched them: the defaults, published once loaded.
    let loaded = client.voice_prefs().load();
    assert_eq!(loaded, VoiceValues::default());
    wait_for(
        &client,
        Duration::from_secs(10),
        "the loaded defaults",
        |s| s.voice_prefs.values == VoiceValues::default(),
    )
    .await;

    // Each write is checked, stored and published.
    let voice = client.voice_prefs();
    voice.set(Setting::SensitivityAuto(false)).unwrap();
    voice.set(Setting::Sensitivity(-100)).unwrap();
    voice.set(Setting::InputVolume(150)).unwrap();
    voice.set(Setting::Mode(InputMode::Ptt)).unwrap();
    voice.set(Setting::PttChord(Some("Insert".into()))).unwrap();
    voice
        .set(Setting::Noise(NoisePreference::Automatic))
        .unwrap();
    assert!(voice.set(Setting::InputVolume(201)).is_err());
    assert!(voice.set(Setting::PttChord(Some("KeyA".into()))).is_err());
    let s = wait_for(
        &client,
        Duration::from_secs(10),
        "the written values",
        |s| {
            s.voice_prefs.values.ptt_chord.as_deref() == Some("Insert")
                && s.voice_prefs.values.noise == NoisePreference::Automatic
                && s.voice_prefs.values.input_volume == 150
        },
    )
    .await;
    let v = &s.voice_prefs.values;
    assert!(!v.sensitivity_auto);
    assert_eq!(v.sensitivity_db, -100);
    assert_eq!(v.mode, InputMode::Ptt);

    // What the device audio reports is published too: the devices, the test, the notices (shown once, then dismissed).
    voice.set_devices(AudioDevices {
        inputs: vec![AudioDevice {
            id: "usb".into(),
            name: "USB headset".into(),
        }],
        outputs: vec![],
    });
    voice.set_test(MicTestState {
        testing: true,
        level_db: -12.0,
        ..MicTestState::default()
    });
    voice.note(AudioNotice::MicLost);
    voice.note(AudioNotice::MicLost);
    let s = wait_for(&client, Duration::from_secs(10), "the audio reports", |s| {
        s.voice_prefs.test.testing && !s.voice_prefs.devices.inputs.is_empty()
    })
    .await;
    assert_eq!(s.voice_prefs.notices, [AudioNotice::MicLost]);
    voice.dismiss(AudioNotice::MicLost);
    wait_for(
        &client,
        Duration::from_secs(10),
        "the dismissed notice",
        |s| s.voice_prefs.notices.is_empty(),
    )
    .await;

    // The settings survive a restart of the client on the same data directory.
    tokio::task::spawn_blocking(move || client.stop())
        .await
        .unwrap();
    let again = start(&alice).await;
    let v = again.voice_prefs().values();
    assert!(!v.sensitivity_auto);
    assert_eq!((v.sensitivity_db, v.input_volume), (-100, 150));
    assert_eq!(v.mode, InputMode::Ptt);
    assert_eq!(v.ptt_chord.as_deref(), Some("Insert"));
    assert_eq!(v.noise, NoisePreference::Automatic);
}

/// The preference keys of the area are the documented ones, so another client of the same instance (or a newer build)
/// reads what this one wrote: the web's names, `voice.pttChord` for the push-to-talk key.
#[test]
fn the_voice_keys_are_the_web_names() {
    use dilla_client::features::voice_prefs::values::*;
    assert_eq!(
        [
            INPUT,
            OUTPUT,
            MODE,
            NOISE,
            INPUT_VOLUME,
            OUTPUT_VOLUME,
            SENSITIVITY_AUTO,
            SENSITIVITY,
            PTT_RELEASE,
            ECHO,
            AGC
        ],
        [
            "voice.input",
            "voice.output",
            "voice.mode",
            "voice.noise",
            "voice.inputVolume",
            "voice.outputVolume",
            "voice.sensitivityAuto",
            "voice.sensitivity",
            "voice.pttRelease",
            "voice.echo",
            "voice.agc"
        ]
    );
    assert_eq!(PTT_CHORD, "voice.pttChord");
}
