//! The voice settings as the device keeps them (`<data_dir>/prefs.json`, the `voice.*` keys of OWNERSHIP.md), validated the
//! way client-core validates the web's `voice.*` settings: a value the key does not admit reads as its default, and a
//! setter refuses one.
//!
//! The web writes these as account settings; the desktop keeps them in the instance's prefs file (the device is the
//! unit: its microphone, its speaker, its key). The names are the web's, except the push-to-talk key, which is
//! `voice.pttChord` (a key whose words include `key` is refused by [`crate::prefs`]).

use crate::prefs::{Prefs, PrefsError};

use super::volume::{Volumes, is_percent, is_user_id, is_volume};

pub const INPUT: &str = "voice.input";
pub const OUTPUT: &str = "voice.output";
pub const MODE: &str = "voice.mode";
pub const PTT_CHORD: &str = "voice.pttChord";
pub const NOISE: &str = "voice.noise";
pub const INPUT_VOLUME: &str = "voice.inputVolume";
pub const OUTPUT_VOLUME: &str = "voice.outputVolume";
pub const SENSITIVITY_AUTO: &str = "voice.sensitivityAuto";
pub const SENSITIVITY: &str = "voice.sensitivity";
pub const PTT_RELEASE: &str = "voice.pttRelease";
pub const ECHO: &str = "voice.echo";
pub const AGC: &str = "voice.agc";
/// `voice.volume.<user>` of the web, as one object (user id to 0..200 in steps of 10).
pub const VOLUMES: &str = "voice.volumes";
/// `voice.streamVolume.<user>` of the web, as one object.
pub const STREAM_VOLUMES: &str = "voice.streamVolumes";

/// `voice.mode`: voice activity (the default) or push to talk.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum InputMode {
    #[default]
    Vad,
    Ptt,
}

impl InputMode {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Vad => "vad",
            Self::Ptt => "ptt",
        }
    }

    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "vad" => Some(Self::Vad),
            "ptt" => Some(Self::Ptt),
            _ => None,
        }
    }
}

/// `voice.noise`: what the person chose for noise suppression.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum NoisePreference {
    /// DeepFilterNet3 where this device can run it, else the lightweight suppressor (visibly).
    Automatic,
    /// libwebrtc's own suppressor (the default).
    #[default]
    Lightweight,
    Off,
}

impl NoisePreference {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Automatic => "automatic",
            Self::Lightweight => "lightweight",
            Self::Off => "off",
        }
    }

    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "automatic" => Some(Self::Automatic),
            "lightweight" | "on" => Some(Self::Lightweight),
            "off" => Some(Self::Off),
            _ => None,
        }
    }
}

/// Every voice setting of this device.
#[derive(Clone, Debug, PartialEq)]
pub struct VoiceValues {
    /// The chosen microphone's id; `None` is the system default.
    pub input: Option<String>,
    /// The chosen speaker's id; `None` is the system default.
    pub output: Option<String>,
    /// 0..200 %.
    pub input_volume: u32,
    pub output_volume: u32,
    pub sensitivity_auto: bool,
    /// The manual threshold, -100..0 dB.
    pub sensitivity_db: i32,
    pub mode: InputMode,
    /// The push-to-talk key as a `KeyboardEvent.code`; `None`: no key set (there is no default key).
    pub ptt_chord: Option<String>,
    /// Push to talk keeps sending this long after the key is let go: 0..1000 ms in steps of 20.
    pub ptt_release_ms: u32,
    pub noise: NoisePreference,
    pub echo: bool,
    pub agc: bool,
    /// Per person, by user id.
    pub volumes: Volumes,
    pub stream_volumes: Volumes,
}

impl Default for VoiceValues {
    fn default() -> Self {
        Self {
            input: None,
            output: None,
            input_volume: 100,
            output_volume: 100,
            sensitivity_auto: true,
            sensitivity_db: -50,
            mode: InputMode::Vad,
            ptt_chord: None,
            ptt_release_ms: 200,
            noise: NoisePreference::Lightweight,
            echo: true,
            agc: true,
            volumes: Volumes::new(),
            stream_volumes: Volumes::new(),
        }
    }
}

/// One change to one setting.
#[derive(Clone, Debug, PartialEq)]
pub enum Setting {
    Input(Option<String>),
    Output(Option<String>),
    InputVolume(u32),
    OutputVolume(u32),
    SensitivityAuto(bool),
    Sensitivity(i32),
    Mode(InputMode),
    PttChord(Option<String>),
    PttRelease(u32),
    Noise(NoisePreference),
    Echo(bool),
    Agc(bool),
    /// One person's voice volume (100 forgets the entry).
    UserVolume {
        user: String,
        percent: u32,
    },
    /// One person's shared-screen volume (100 forgets the entry; 0 is the stream mute).
    StreamVolume {
        user: String,
        percent: u32,
    },
}

/// Why a setting was not written.
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum VoiceError {
    /// The key does not admit the value.
    #[error("the voice setting does not admit this value")]
    Invalid,
    #[error("the voice settings could not be saved: {0}")]
    Prefs(String),
}

impl From<PrefsError> for VoiceError {
    fn from(e: PrefsError) -> Self {
        Self::Prefs(e.to_string())
    }
}

/// A device id as the host gave it (cpal's, any printable text up to 256 bytes): the id of a device that is gone is
/// kept, so the choice comes back when it is plugged in again.
pub fn is_device_id(id: &str) -> bool {
    !id.is_empty() && id.len() <= 256 && !id.chars().any(char::is_control)
}

impl VoiceValues {
    /// The values `prefs` holds; every one it does not hold, or holds in a form its key does not admit, is the default.
    pub fn read(prefs: &Prefs) -> Self {
        let d = Self::default();
        let device = |key: &str| prefs.get::<String>(key).filter(|v| is_device_id(v));
        let percent = |key: &str, default: u32| {
            prefs
                .get::<u32>(key)
                .filter(|v| is_percent(*v))
                .unwrap_or(default)
        };
        let volumes = |key: &str| -> Volumes {
            prefs
                .get::<Volumes>(key)
                .unwrap_or_default()
                .into_iter()
                .filter(|(user, v)| is_user_id(user) && is_volume(*v))
                .collect()
        };
        Self {
            input: device(INPUT),
            output: device(OUTPUT),
            input_volume: percent(INPUT_VOLUME, d.input_volume),
            output_volume: percent(OUTPUT_VOLUME, d.output_volume),
            sensitivity_auto: prefs
                .get::<bool>(SENSITIVITY_AUTO)
                .unwrap_or(d.sensitivity_auto),
            sensitivity_db: prefs
                .get::<i32>(SENSITIVITY)
                .filter(|v| (-100..=0).contains(v))
                .unwrap_or(d.sensitivity_db),
            mode: prefs
                .get::<String>(MODE)
                .and_then(|m| InputMode::parse(&m))
                .unwrap_or(d.mode),
            ptt_chord: prefs
                .get::<String>(PTT_CHORD)
                .filter(|c| super::ptt::is_push_to_talk_key(c)),
            ptt_release_ms: prefs
                .get::<u32>(PTT_RELEASE)
                .filter(|v| *v <= 1000 && v % 20 == 0)
                .unwrap_or(d.ptt_release_ms),
            noise: prefs
                .get::<String>(NOISE)
                .and_then(|n| NoisePreference::parse(&n))
                .unwrap_or(d.noise),
            echo: prefs.get::<bool>(ECHO).unwrap_or(d.echo),
            agc: prefs.get::<bool>(AGC).unwrap_or(d.agc),
            volumes: volumes(VOLUMES),
            stream_volumes: volumes(STREAM_VOLUMES),
        }
    }

    /// These values with `setting` applied, or why it is refused. Nothing is written.
    pub fn with(&self, setting: &Setting) -> Result<Self, VoiceError> {
        let mut next = self.clone();
        match setting {
            Setting::Input(id) | Setting::Output(id) => {
                if id.as_deref().is_some_and(|id| !is_device_id(id)) {
                    return Err(VoiceError::Invalid);
                }
                if matches!(setting, Setting::Input(_)) {
                    next.input = id.clone();
                } else {
                    next.output = id.clone();
                }
            }
            Setting::InputVolume(v) | Setting::OutputVolume(v) => {
                if !is_percent(*v) {
                    return Err(VoiceError::Invalid);
                }
                if matches!(setting, Setting::InputVolume(_)) {
                    next.input_volume = *v;
                } else {
                    next.output_volume = *v;
                }
            }
            Setting::SensitivityAuto(on) => next.sensitivity_auto = *on,
            Setting::Sensitivity(db) => {
                if !(-100..=0).contains(db) {
                    return Err(VoiceError::Invalid);
                }
                next.sensitivity_db = *db;
            }
            Setting::Mode(mode) => next.mode = *mode,
            Setting::PttChord(chord) => {
                if chord
                    .as_deref()
                    .is_some_and(|c| !super::ptt::is_push_to_talk_key(c))
                {
                    return Err(VoiceError::Invalid);
                }
                next.ptt_chord = chord.clone();
            }
            Setting::PttRelease(ms) => {
                if *ms > 1000 || ms % 20 != 0 {
                    return Err(VoiceError::Invalid);
                }
                next.ptt_release_ms = *ms;
            }
            Setting::Noise(n) => next.noise = *n,
            Setting::Echo(on) => next.echo = *on,
            Setting::Agc(on) => next.agc = *on,
            Setting::UserVolume { user, percent } | Setting::StreamVolume { user, percent } => {
                if !is_user_id(user) || !is_volume(*percent) {
                    return Err(VoiceError::Invalid);
                }
                let map = if matches!(setting, Setting::UserVolume { .. }) {
                    &mut next.volumes
                } else {
                    &mut next.stream_volumes
                };
                if *percent == 100 {
                    map.remove(user);
                } else {
                    map.insert(user.clone(), *percent);
                }
            }
        }
        Ok(next)
    }

    /// Checks `setting`, writes its one key to `prefs` (keeping every other) and returns the values after it.
    pub fn apply(prefs: &Prefs, setting: &Setting) -> Result<Self, VoiceError> {
        let next = Self::read(prefs).with(setting)?;
        let optional = |key: &str, v: &Option<String>| match v {
            Some(v) => prefs.set(key, v),
            None => prefs.remove(key),
        };
        match setting {
            Setting::Input(_) => optional(INPUT, &next.input)?,
            Setting::Output(_) => optional(OUTPUT, &next.output)?,
            Setting::InputVolume(_) => prefs.set(INPUT_VOLUME, &next.input_volume)?,
            Setting::OutputVolume(_) => prefs.set(OUTPUT_VOLUME, &next.output_volume)?,
            Setting::SensitivityAuto(_) => prefs.set(SENSITIVITY_AUTO, &next.sensitivity_auto)?,
            Setting::Sensitivity(_) => prefs.set(SENSITIVITY, &next.sensitivity_db)?,
            Setting::Mode(_) => prefs.set(MODE, &next.mode.as_str())?,
            Setting::PttChord(_) => optional(PTT_CHORD, &next.ptt_chord)?,
            Setting::PttRelease(_) => prefs.set(PTT_RELEASE, &next.ptt_release_ms)?,
            Setting::Noise(_) => prefs.set(NOISE, &next.noise.as_str())?,
            Setting::Echo(_) => prefs.set(ECHO, &next.echo)?,
            Setting::Agc(_) => prefs.set(AGC, &next.agc)?,
            Setting::UserVolume { .. } => prefs.set(VOLUMES, &next.volumes)?,
            Setting::StreamVolume { .. } => prefs.set(STREAM_VOLUMES, &next.stream_volumes)?,
        }
        Ok(Self::read(prefs))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn temp() -> (std::path::PathBuf, Prefs) {
        let dir = std::env::temp_dir().join(format!(
            "dilla-voice-prefs-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let prefs = Prefs::instance(&dir);
        (dir, prefs)
    }

    const ALICE: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";

    #[test]
    fn absent_keys_read_as_the_web_defaults() {
        let (dir, prefs) = temp();
        let v = VoiceValues::read(&prefs);
        assert_eq!(v, VoiceValues::default());
        assert_eq!((v.input_volume, v.output_volume), (100, 100));
        assert!(v.sensitivity_auto && v.echo && v.agc);
        assert_eq!((v.sensitivity_db, v.ptt_release_ms), (-50, 200));
        assert_eq!(v.mode, InputMode::Vad);
        assert_eq!(v.noise, NoisePreference::Lightweight);
        assert_eq!(v.ptt_chord, None);
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn a_value_the_key_does_not_admit_reads_as_its_default() {
        let (dir, prefs) = temp();
        prefs.set(INPUT_VOLUME, &201u32).unwrap();
        prefs.set(SENSITIVITY, &5i32).unwrap();
        prefs.set(PTT_RELEASE, &150u32).unwrap();
        prefs.set(PTT_CHORD, &"KeyA").unwrap();
        prefs.set(MODE, &"loud").unwrap();
        prefs
            .set(
                VOLUMES,
                &Volumes::from([(ALICE.into(), 55), ("x".into(), 50)]),
            )
            .unwrap();
        let v = VoiceValues::read(&prefs);
        assert_eq!(v, VoiceValues::default());
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn every_setting_persists_and_comes_back() {
        // sensitivity.spec.ts: the threshold and the levels persist (the web's across a reload, here across a restart).
        let (dir, prefs) = temp();
        for s in [
            Setting::SensitivityAuto(false),
            Setting::Sensitivity(-100),
            Setting::InputVolume(150),
            Setting::OutputVolume(0),
            Setting::Mode(InputMode::Ptt),
            Setting::PttChord(Some("Insert".into())),
            Setting::PttRelease(400),
            Setting::Noise(NoisePreference::Automatic),
            Setting::Echo(false),
            Setting::Agc(false),
            Setting::Input(Some("alsa:front".into())),
            Setting::Output(Some("alsa:hdmi".into())),
            Setting::UserVolume {
                user: ALICE.into(),
                percent: 50,
            },
        ] {
            VoiceValues::apply(&prefs, &s).unwrap();
        }
        let reopened = Prefs::instance(&dir);
        let v = VoiceValues::read(&reopened);
        assert!(!v.sensitivity_auto && !v.echo && !v.agc);
        assert_eq!(
            (v.sensitivity_db, v.input_volume, v.output_volume),
            (-100, 150, 0)
        );
        assert_eq!(v.mode, InputMode::Ptt);
        assert_eq!(v.ptt_chord.as_deref(), Some("Insert"));
        assert_eq!(v.ptt_release_ms, 400);
        assert_eq!(v.noise, NoisePreference::Automatic);
        assert_eq!(v.input.as_deref(), Some("alsa:front"));
        assert_eq!(v.output.as_deref(), Some("alsa:hdmi"));
        assert_eq!(v.volumes.get(ALICE), Some(&50));
        // Choosing the system default forgets the device; 100 % forgets the person.
        let v = VoiceValues::apply(&reopened, &Setting::Input(None)).unwrap();
        assert_eq!(v.input, None);
        let v = VoiceValues::apply(
            &reopened,
            &Setting::UserVolume {
                user: ALICE.into(),
                percent: 100,
            },
        )
        .unwrap();
        assert!(v.volumes.is_empty());
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn a_setter_refuses_what_the_key_does_not_admit_and_writes_nothing() {
        let (dir, prefs) = temp();
        for s in [
            Setting::InputVolume(201),
            Setting::Sensitivity(1),
            Setting::Sensitivity(-101),
            Setting::PttRelease(30),
            Setting::PttRelease(1020),
            Setting::PttChord(Some("KeyA".into())),
            Setting::Input(Some(String::new())),
            Setting::Output(Some("bad\nid".into())),
            Setting::UserVolume {
                user: ALICE.into(),
                percent: 55,
            },
            Setting::UserVolume {
                user: "nope".into(),
                percent: 50,
            },
        ] {
            assert_eq!(
                VoiceValues::apply(&prefs, &s),
                Err(VoiceError::Invalid),
                "{s:?}"
            );
        }
        assert_eq!(VoiceValues::read(&prefs), VoiceValues::default());
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn writing_one_key_keeps_every_other_key_of_the_file() {
        let (dir, prefs) = temp();
        prefs.set("layout.sidebarWidth", &280u32).unwrap();
        VoiceValues::apply(&prefs, &Setting::Echo(false)).unwrap();
        assert_eq!(prefs.get::<u32>("layout.sidebarWidth"), Some(280));
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn no_voice_key_names_a_secret() {
        for key in [
            INPUT,
            OUTPUT,
            MODE,
            PTT_CHORD,
            NOISE,
            INPUT_VOLUME,
            OUTPUT_VOLUME,
            SENSITIVITY_AUTO,
            SENSITIVITY,
            PTT_RELEASE,
            ECHO,
            AGC,
            VOLUMES,
            STREAM_VOLUMES,
        ] {
            assert!(!crate::prefs::names_a_secret(key), "{key}");
            assert!(key.starts_with("voice."));
        }
    }

    #[test]
    fn the_legacy_noise_value_on_reads_as_lightweight() {
        assert_eq!(
            NoisePreference::parse("on"),
            Some(NoisePreference::Lightweight)
        );
        assert_eq!(NoisePreference::parse("loud"), None);
    }
}
