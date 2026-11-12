//! Settings -> Voice (packages/web/src/screens/settings/Voice.tsx, packages/ui VoiceSettings; flow 06, README §5.9):
//! devices and their volumes, the Mic test (Let's check, the sensitivity meter and automatic sensitivity), the input mode
//! with push to talk's key and release delay, and processing (noise suppression, echo cancellation, automatic gain), each
//! written to the device's preferences ([`dilla_client::features::voice_prefs`]).
//!
//! The view keeps the section's state itself (`local`): it starts from what the app state holds (a fixture's world, or
//! the stored preferences of a running client), applies every change at once and mirrors it to the client, so a slider
//! never waits for the state to come back. Push to talk's key is captured by the set-key button: the next accepted key
//! is saved without typing anything, a key that types text or operates the window is refused with a reason, and Escape,
//! Enter, Space, Tab or leaving cancels.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use dilla_client::features::voice_prefs::ptt::{self, Capture, HeldModifiers};
use dilla_client::features::voice_prefs::values::NoisePreference as Noise;
use dilla_client::features::voice_prefs::{InputMode, Setting, VoiceError};
use dilla_client::state::AppState;
use dilla_client::state::voice_prefs::{
    AudioDevices, AudioNotice, MicTestState, Processor, ProcessorNote, VoicePrefsState,
};
use dilla_desktop_ui::components::banner::{Banner, BannerTone};
use dilla_desktop_ui::components::segmented::Segmented;
use dilla_desktop_ui::components::select::Select;
use dilla_desktop_ui::components::settings::frame::section_header;
use dilla_desktop_ui::components::toggle::Toggle;
use dilla_desktop_ui::components::voice_settings::layout::{
    self, Heard, RELEASE_VALUE_W, StatusPill, VOLUME_VALUE_W,
};
use dilla_desktop_ui::components::voice_settings::sensitivity_meter::format_db;
use dilla_desktop_ui::components::voice_settings::{
    CheckButton, KeyCapture, SensitivityMeter, Slider,
};
use gpui::{
    App, Context, Entity, FocusHandle, IntoElement, ParentElement, Render, Styled, Window, div,
};

use super::devices::{DeviceMonitor, POLL};
use super::mic_test::{MicTest, OnEvent, TestEvent, TestSettings};
use super::{SPEAKER_LOST, audio_host, audio_host_is_set, sounds};
use crate::slots::SlotCx;
use crate::strings::{t, tf};
use crate::tracked_box;

/// How often the section drains what the device threads reported.
const PUMP: Duration = Duration::from_millis(40);

type OnPick = Box<dyn Fn(Option<String>, &mut App)>;

/// What the device threads report to the view.
enum Inbound {
    Devices(AudioDevices),
    /// An event of the test started under this generation.
    Test(u64, TestEvent),
}

/// The focus handles of the controls that own one, so a test (and the automation `focused` query) can tell where the focus
/// is.
#[derive(Clone)]
pub struct Handles {
    pub input_volume: FocusHandle,
    pub output_volume: FocusHandle,
    pub check: FocusHandle,
    pub threshold: FocusHandle,
    pub release: FocusHandle,
    pub capture: FocusHandle,
}

/// The Voice section.
pub struct VoiceSection {
    pub ctx: SlotCx,
    /// What the section shows (see the module docs).
    pub local: VoicePrefsState,
    /// The set-key button listens for the next key.
    pub capturing: bool,
    /// The last captured key was refused (the prompt says why).
    refused: bool,
    /// A write was refused or failed: its E_ code.
    pub error: Option<&'static str>,
    test: Option<MicTest>,
    /// Which test is current: what a test that was stopped reports afterwards (its `Ended`) is not this one's.
    generation: u64,
    inbox: Arc<Mutex<Vec<Inbound>>>,
    _monitor: Option<DeviceMonitor>,
    pub handles: Handles,
}

fn code_of(e: &VoiceError) -> &'static str {
    match e {
        VoiceError::Invalid => "E_INVALID",
        VoiceError::Prefs(_) => "E_UNAVAILABLE",
    }
}

fn mode_str(m: InputMode) -> &'static str {
    m.as_str()
}

impl VoiceSection {
    pub fn new(ctx: SlotCx, _window: &mut Window, cx: &mut Context<Self>) -> Self {
        let mut local = ctx.state.voice_prefs.clone();
        let mut monitor = None;
        let inbox: Arc<Mutex<Vec<Inbound>>> = Arc::default();
        if let Some(client) = &ctx.client {
            // A running client: the stored preferences are the truth, and the device list is watched.
            local.values = client.voice_prefs().load();
        }
        if ctx.client.is_some() || audio_host_is_set() {
            let sink = inbox.clone();
            monitor = Some(DeviceMonitor::start(audio_host(), POLL, move |list| {
                sink.lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner)
                    .push(Inbound::Devices(list));
            }));
        }
        sounds::set_output_device(local.values.output.clone());
        // The device threads cannot wake the window: this task drains their reports on the foreground.
        cx.spawn(async move |this, cx| {
            loop {
                cx.background_executor().timer(PUMP).await;
                if this.update(cx, |view, cx| view.pump(cx)).is_err() {
                    break;
                }
            }
        })
        .detach();
        let handles = Handles {
            input_volume: cx.focus_handle().tab_stop(true),
            output_volume: cx.focus_handle().tab_stop(true),
            check: cx.focus_handle().tab_stop(true),
            threshold: cx.focus_handle().tab_stop(true),
            release: cx.focus_handle().tab_stop(true),
            capture: cx.focus_handle().tab_stop(true),
        };
        Self {
            ctx,
            local,
            capturing: false,
            refused: false,
            error: None,
            test: None,
            generation: 0,
            inbox,
            _monitor: monitor,
            handles,
        }
    }

    /// Whether a call holds the microphone, so Let's check cannot start. Task 17's `AppState::voice` has no call phase
    /// yet; it answers here when it does.
    fn in_call(_state: &AppState) -> bool {
        false
    }

    /// Drains what the device threads reported.
    fn pump(&mut self, cx: &mut Context<Self>) {
        let drained = std::mem::take(
            &mut *self
                .inbox
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner),
        );
        if drained.is_empty() {
            return;
        }
        for event in drained {
            match event {
                Inbound::Devices(list) => {
                    self.local.devices = list.clone();
                    if let Some(c) = &self.ctx.client {
                        c.voice_prefs().set_devices(list);
                    }
                }
                Inbound::Test(generation, event) => {
                    if generation == self.generation {
                        self.on_test_event(event);
                    }
                }
            }
        }
        cx.notify();
    }

    fn mirror_test(&self) {
        if let Some(c) = &self.ctx.client {
            c.voice_prefs().set_test(self.local.test.clone());
        }
    }

    fn note(&mut self, notice: AudioNotice) {
        if !self.local.notices.contains(&notice) {
            self.local.notices.push(notice);
        }
        if let Some(c) = &self.ctx.client {
            c.voice_prefs().note(notice);
        }
    }

    fn on_test_event(&mut self, event: TestEvent) {
        let test = &mut self.local.test;
        match event {
            TestEvent::Started(status) | TestEvent::Processor(status) => {
                test.testing = true;
                test.failed = false;
                test.processor = Some(status);
            }
            TestEvent::Level {
                db,
                threshold_db,
                open,
            } => {
                test.level_db = db;
                test.open = open;
                if self.local.values.sensitivity_auto {
                    test.auto_db = threshold_db;
                }
            }
            TestEvent::Failed => {
                *test = MicTestState {
                    failed: true,
                    ..MicTestState::default()
                };
                self.test = None;
            }
            TestEvent::Notice(notice) => {
                self.note(notice);
                return;
            }
            TestEvent::Ended => {
                *test = MicTestState {
                    failed: test.failed,
                    auto_db: test.auto_db,
                    ..MicTestState::default()
                };
                self.test = None;
            }
        }
        self.mirror_test();
    }

    /// Let's check / Stop testing.
    pub fn toggle_test(&mut self, cx: &mut Context<Self>) {
        if self.local.test.testing {
            self.stop_test();
            cx.notify();
            return;
        }
        if Self::in_call(&self.ctx.state) {
            return;
        }
        self.local
            .notices
            .retain(|n| *n == AudioNotice::SpeakerLost);
        self.local.test = MicTestState {
            testing: true,
            auto_db: self.local.test.auto_db,
            ..MicTestState::default()
        };
        self.mirror_test();
        self.generation += 1;
        let (inbox, generation) = (self.inbox.clone(), self.generation);
        let on_event: OnEvent = Arc::new(move |event| {
            inbox
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .push(Inbound::Test(generation, event));
        });
        self.test = Some(MicTest::start(
            audio_host(),
            Arc::new(dilla_media::noise::embedded_model),
            TestSettings::of(&self.local.values),
            on_event,
        ));
        cx.notify();
    }

    /// Ends the test at once and releases the devices.
    pub fn stop_test(&mut self) {
        self.generation += 1;
        // Dropping the test joins its threads: the microphone and the speaker are closed before this returns.
        self.test = None;
        let auto_db = self.local.test.auto_db;
        self.local.test = MicTestState {
            auto_db,
            ..MicTestState::default()
        };
        self.mirror_test();
    }

    /// Writes one setting; a refused or failed one shows its code.
    pub fn set(&mut self, setting: Setting, cx: &mut Context<Self>) {
        let result = match &self.ctx.client {
            Some(client) => client.voice_prefs().set(setting.clone()),
            None => self.local.values.with(&setting),
        };
        match result {
            Ok(values) => {
                self.error = None;
                let before = TestSettings::of(&self.local.values);
                self.local.values = values;
                let after = TestSettings::of(&self.local.values);
                if matches!(setting, Setting::Output(_)) {
                    sounds::set_output_device(self.local.values.output.clone());
                    self.local
                        .notices
                        .retain(|n| *n != AudioNotice::SpeakerLost);
                }
                // A change of device or processing ends the test (the web closes it); volume, threshold and mode apply at
                // once.
                if self.test.is_some() {
                    if before.same_chain(&after) {
                        if let Some(test) = &self.test {
                            test.update(after);
                        }
                    } else {
                        self.stop_test();
                    }
                }
            }
            Err(e) => self.error = Some(code_of(&e)),
        }
        cx.notify();
    }

    /// The set-key button: start the capture, or cancel it.
    pub fn toggle_capture(&mut self, cx: &mut Context<Self>) {
        self.capturing = !self.capturing;
        self.refused = false;
        cx.notify();
    }

    /// The next key of a capture: it binds, is refused (the capture goes on), or cancels.
    pub fn capture_key(&mut self, key: &str, modifiers: HeldModifiers, cx: &mut Context<Self>) {
        if !self.capturing {
            return;
        }
        match ptt::capture(key, modifiers) {
            Capture::Cancel => {
                self.capturing = false;
                self.refused = false;
            }
            Capture::Wait => {}
            Capture::Refused => self.refused = true,
            Capture::Accepted(code) => {
                self.capturing = false;
                self.refused = false;
                self.set(Setting::PttChord(Some(code.to_owned())), cx);
            }
        }
        cx.notify();
    }

    /// The capture ends without a key (Tab left the button).
    pub fn cancel_capture(&mut self, cx: &mut Context<Self>) {
        if self.capturing {
            self.capturing = false;
            self.refused = false;
            cx.notify();
        }
    }
}

/// `processorText` of Voice.tsx: what runs and why it fell back.
fn processor_line(test: &MicTestState) -> Option<String> {
    let status = test.processor?;
    let name = match status.effective {
        Processor::Off => t("settings.voice.processor.off").to_owned(),
        Processor::Standard => dilla_media::noise::NoiseMode::Standard
            .processor()
            .to_owned(),
        Processor::Enhanced => dilla_media::noise::NoiseMode::Enhanced
            .processor()
            .to_owned(),
    };
    let mut line = tf("settings.voice.processor", &[("processor", &name)]);
    if let Some(ProcessorNote::EnhancedUnavailable) = status.note {
        line.push(' ');
        line.push_str(t("settings.voice.fallback.enhanced-failed"));
    }
    Some(line)
}

/// What a notice says. The web's `fallback-failed` and `processor-error` copy says it for a microphone and a processor;
/// a lost speaker has native-only copy.
fn notice_text(notice: AudioNotice) -> &'static str {
    match notice {
        AudioNotice::MicLost => t("settings.voice.fallback.fallback-failed"),
        AudioNotice::EnhancedFailed => t("settings.voice.fallback.processor-error"),
        AudioNotice::SpeakerLost => SPEAKER_LOST,
    }
}

impl Render for VoiceSection {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let me: Entity<Self> = cx.entity();
        let v = self.local.values.clone();
        let devices = self.local.devices.clone();
        let test = self.local.test.clone();
        let in_call = Self::in_call(&self.ctx.state);
        let width = layout::group_inner();
        let column = layout::column_width();
        let handles = self.handles.clone();

        // ---- the page head and what went wrong
        let mut page = layout::head().child(section_header(
            t("settings.voice.section"),
            Some(t("signalSettings.voice.body")),
        ));
        if let Some(code) = self.error {
            page = page.child(Banner::new(
                "voice-error",
                BannerTone::Danger,
                tf("settings.error.other", &[("code", code)]),
            ));
        }

        // ---- Devices
        let device_options = |list: &[dilla_client::state::voice_prefs::AudioDevice]| {
            let mut options = vec![("".to_owned(), t("settings.voice.systemDefault").to_owned())];
            options.extend(list.iter().map(|d| (d.id.clone(), d.name.clone())));
            options
        };
        let select = |id: &'static str,
                      label: &str,
                      options: Vec<(String, String)>,
                      value: &str,
                      on_pick: OnPick| {
            let pairs: Vec<(&str, &str)> = options
                .iter()
                .map(|(v, l)| (v.as_str(), l.as_str()))
                .collect();
            Select::new(id, label.to_owned(), &pairs, value.to_owned()).on_change(
                move |picked, _, cx| {
                    on_pick((!picked.is_empty()).then(|| picked.to_owned()), cx);
                },
            )
        };
        let input_select = {
            let me = me.clone();
            select(
                "voice-input-select",
                t("settings.voice.input"),
                device_options(&devices.inputs),
                v.input.as_deref().unwrap_or(""),
                Box::new(move |id, cx| {
                    me.update(cx, |this, cx| this.set(Setting::Input(id), cx));
                }),
            )
        };
        let output_select = {
            let me = me.clone();
            select(
                "voice-output-select",
                t("settings.voice.output"),
                device_options(&devices.outputs),
                v.output.as_deref().unwrap_or(""),
                Box::new(move |id, cx| {
                    me.update(cx, |this, cx| this.set(Setting::Output(id), cx));
                }),
            )
        };
        let volume_slider = |id: &'static str, value: u32, handle: &FocusHandle, input: bool| {
            let me = me.clone();
            Slider::new(id, value as i32, 0, 200)
                .focus_handle(handle)
                .on_change(move |value, commit, _, cx| {
                    if commit {
                        me.update(cx, |this, cx| {
                            let setting = if input {
                                Setting::InputVolume(value as u32)
                            } else {
                                Setting::OutputVolume(value as u32)
                            };
                            this.set(setting, cx);
                        });
                    }
                })
        };
        let devices_group = layout::group()
            .child(layout::heading(t("signalSettings.voice.devices")))
            .child(
                layout::grid()
                    .child(
                        layout::field()
                            .child(tracked_box("voice-input-select", input_select))
                            .child(tracked_box(
                                "voice-input-volume",
                                layout::volume_row(
                                    t("signalSettings.voice.volume"),
                                    volume_slider(
                                        "voice-input-volume-range",
                                        v.input_volume,
                                        &handles.input_volume,
                                        true,
                                    ),
                                    &format!("{}%", v.input_volume),
                                    VOLUME_VALUE_W,
                                ),
                            )),
                    )
                    .child(
                        layout::field()
                            .child(tracked_box("voice-output-select", output_select))
                            .child(tracked_box(
                                "voice-output-volume",
                                layout::volume_row(
                                    t("signalSettings.voice.volume"),
                                    volume_slider(
                                        "voice-output-volume-range",
                                        v.output_volume,
                                        &handles.output_volume,
                                        false,
                                    ),
                                    &format!("{}%", v.output_volume),
                                    VOLUME_VALUE_W,
                                ),
                            )),
                    ),
            );

        // ---- Mic test
        let heard = if test.testing {
            if test.open {
                Heard::Heard
            } else {
                Heard::Quiet
            }
        } else {
            Heard::Idle
        };
        let pill_text = match heard {
            Heard::Heard => t("signalSettings.voice.heard"),
            Heard::Quiet => t("signalSettings.voice.tooQuiet"),
            Heard::Idle => t("signalSettings.voice.notTesting"),
        };
        let threshold_db = if v.sensitivity_auto {
            test.auto_db
        } else {
            v.sensitivity_db as f32
        };
        let level_text = tf(
            "signalSettings.voice.levelValue",
            &[(
                "value",
                &if test.testing {
                    format_db(test.level_db)
                } else {
                    t("signalSettings.voice.noLevel").to_owned()
                },
            )],
        );
        let threshold_text = tf(
            "signalSettings.voice.thresholdValue",
            &[("value", &format_db(threshold_db))],
        );
        let hint = if test.failed {
            t("settings.voice.testFailed")
        } else if in_call && !test.testing {
            t("settings.voice.testCall")
        } else {
            t("signalSettings.voice.testHint")
        };
        let check = {
            let me = me.clone();
            CheckButton::new(
                "voice-check-button",
                if test.testing {
                    t("signalSettings.voice.testStop")
                } else {
                    t("signalSettings.voice.testStart")
                },
                test.testing,
            )
            .focus_handle(&handles.check)
            .disabled(in_call && !test.testing)
            .on_click(move |_, _, cx| {
                me.update(cx, |this, cx| this.toggle_test(cx));
            })
        };
        let meter = {
            let me = me.clone();
            SensitivityMeter::new(
                "voice-threshold-meter",
                test.level_db,
                threshold_db,
                v.sensitivity_auto,
                test.testing,
            )
            .focus_handle(&handles.threshold)
            .on_threshold(move |db, commit, _, cx| {
                if commit {
                    me.update(cx, |this, cx| this.set(Setting::Sensitivity(db), cx));
                }
            })
        };
        let auto = {
            let me = me.clone();
            Toggle::new(
                "voice-auto-sensitivity",
                t("signalSettings.voice.auto"),
                v.sensitivity_auto,
            )
            .show_label(false)
            .on_change(move |on, _, cx| {
                me.update(cx, |this, cx| this.set(Setting::SensitivityAuto(on), cx));
            })
        };
        let mut test_group = layout::group()
            .child(
                layout::test_head()
                    .child(layout::heading(t("signalSettings.voice.test")))
                    .child(div().flex_1())
                    .child(tracked_box("voice-check", check)),
            )
            .child(layout::description(hint, width));
        if !v.echo {
            test_group = test_group.child(layout::description(
                t("signalSettings.voice.testEchoOff"),
                width,
            ));
        }
        test_group = test_group
            .child(tracked_box("voice-threshold", meter))
            .child(
                layout::readout()
                    .child(StatusPill::new("voice-pill", heard, pill_text))
                    .child(layout::stat(level_text))
                    .child(layout::stat(threshold_text)),
            )
            .child(layout::switch_row(
                t("signalSettings.voice.auto"),
                if v.sensitivity_auto {
                    t("signalSettings.voice.autoHintOn")
                } else {
                    t("signalSettings.voice.autoHintOff")
                },
                tracked_box("voice-auto", auto),
            ));

        // ---- Input mode
        let mode = {
            let me = me.clone();
            Segmented::new(
                "voice-mode",
                &[
                    ("vad", t("settings.voice.vad")),
                    ("ptt", t("settings.voice.ptt")),
                ],
                mode_str(v.mode),
            )
            .on_change(move |picked, _, cx| {
                let mode = InputMode::parse(picked).unwrap_or_default();
                me.update(cx, |this, cx| this.set(Setting::Mode(mode), cx));
            })
        };
        let mut mode_group = layout::group()
            .child(layout::heading(t("signalSettings.voice.inputMode")))
            .child(div().flex().child(tracked_box("voice-mode", mode)));
        if v.mode == InputMode::Ptt {
            let chord = ptt::stored_chord(v.ptt_chord.as_deref());
            let key_text = if self.capturing {
                t("signalSettings.voice.pttListening")
            } else {
                chord.unwrap_or_else(|| t("settings.voice.pttNone"))
            };
            let capture = {
                let (me_click, me_key, me_tab) = (me.clone(), me.clone(), me.clone());
                KeyCapture::new(
                    "voice-ptt-key",
                    key_text,
                    if self.capturing {
                        t("signalSettings.voice.pttCancel")
                    } else {
                        t("settings.voice.pttSet")
                    },
                )
                .focus_handle(&handles.capture)
                .empty(chord.is_none())
                .listening(self.capturing)
                .on_click(move |_, _, cx| {
                    me_click.update(cx, |this, cx| this.toggle_capture(cx));
                })
                .on_key(move |event, _, cx| {
                    let m = event.keystroke.modifiers;
                    let held = HeldModifiers {
                        ctrl: m.control,
                        alt: m.alt,
                        meta: m.platform,
                        shift: m.shift,
                    };
                    me_key.update(cx, |this, cx| {
                        this.capture_key(&event.keystroke.key, held, cx);
                    });
                })
                .on_blur(move |_, cx| {
                    me_tab.update(cx, |this, cx| this.cancel_capture(cx));
                })
            };
            let release = {
                let me = me.clone();
                Slider::new("voice-release-range", v.ptt_release_ms as i32, 0, 1000)
                    .step(20)
                    .page(100)
                    .focus_handle(&handles.release)
                    .on_change(move |value, commit, _, cx| {
                        if commit {
                            me.update(cx, |this, cx| {
                                this.set(Setting::PttRelease(value as u32), cx);
                            });
                        }
                    })
            };
            mode_group = mode_group.child(
                layout::grid()
                    .child(
                        layout::field()
                            .child(layout::label(t("settings.voice.pttKey")))
                            .child(tracked_box("voice-ptt", capture))
                            .child(layout::note(t("signalSettings.voice.pttHint"), column)),
                    )
                    .child(
                        layout::field()
                            .child(layout::label(t("signalSettings.voice.release")))
                            .child(tracked_box(
                                "voice-release",
                                layout::volume_row_bare(
                                    release,
                                    &format!("{} ms", v.ptt_release_ms),
                                    RELEASE_VALUE_W,
                                ),
                            ))
                            .child(layout::note(t("signalSettings.voice.releaseHint"), column)),
                    ),
            );
        }
        let prompt = (v.mode == InputMode::Ptt && self.capturing).then(|| {
            if self.refused {
                t("settings.voice.pttRefused")
            } else {
                t("settings.voice.pttPrompt")
            }
        });
        mode_group = mode_group.child(layout::prompt(prompt, width));

        // ---- Processing
        let noise = {
            let me = me.clone();
            Segmented::new(
                "voice-noise",
                &[
                    ("automatic", t("settings.voice.automatic")),
                    ("lightweight", t("settings.voice.lightweight")),
                    ("off", t("settings.voice.off")),
                ],
                v.noise.as_str(),
            )
            .on_change(move |picked, _, cx| {
                let noise = Noise::parse(picked).unwrap_or_default();
                me.update(cx, |this, cx| this.set(Setting::Noise(noise), cx));
            })
        };
        let noise_hint = match v.noise {
            Noise::Automatic => t("signalSettings.voice.noiseHint.automatic"),
            Noise::Lightweight => t("signalSettings.voice.noiseHint.lightweight"),
            Noise::Off => t("signalSettings.voice.noiseHint.off"),
        };
        let seg_w = layout::segmented_width(&[
            t("settings.voice.automatic"),
            t("settings.voice.lightweight"),
            t("settings.voice.off"),
        ]);
        let echo = {
            let me = me.clone();
            Toggle::new("voice-echo-switch", t("signalSettings.voice.echo"), v.echo)
                .show_label(false)
                .on_change(move |on, _, cx| {
                    me.update(cx, |this, cx| this.set(Setting::Echo(on), cx));
                })
        };
        let agc = {
            let me = me.clone();
            Toggle::new("voice-agc-switch", t("signalSettings.voice.agc"), v.agc)
                .show_label(false)
                .on_change(move |on, _, cx| {
                    me.update(cx, |this, cx| this.set(Setting::Agc(on), cx));
                })
        };
        let processing_group = layout::group()
            .child(layout::heading(t("signalSettings.voice.processing")))
            .child(layout::control_row(
                div()
                    .flex()
                    .flex_col()
                    .flex_1()
                    .min_w_0()
                    .child(layout::row_label(t("settings.voice.noise"), None))
                    .child(layout::row_hint(noise_hint, width - 14.0 - seg_w)),
                tracked_box("voice-noise", noise),
            ))
            .child(layout::switch_row(
                t("signalSettings.voice.echo"),
                t("signalSettings.voice.echoHint"),
                tracked_box("voice-echo", echo),
            ))
            .child(layout::switch_row(
                t("signalSettings.voice.agc"),
                t("signalSettings.voice.agcHint"),
                tracked_box("voice-agc", agc),
            ));

        let mut section = layout::page().child(page).child(
            layout::panel()
                .child(devices_group)
                .child(test_group)
                .child(mode_group)
                .child(processing_group),
        );
        // The status line under the groups: what the microphone test runs on and why it fell back.
        if let Some(line) = processor_line(&test) {
            section = section.child(tracked_box(
                "voice-processor",
                layout::description(&line, layout::group_inner() + 34.0),
            ));
        }
        // What the audio devices did (never swallowed): a lost microphone or speaker, a processor that failed.
        for (i, notice) in self.local.notices.iter().enumerate() {
            section = section.child(tracked_box(
                match i {
                    0 => "voice-notice-0",
                    1 => "voice-notice-1",
                    _ => "voice-notice-2",
                },
                Banner::new(("voice-notice", i), BannerTone::Warn, notice_text(*notice)),
            ));
        }
        section
    }
}
