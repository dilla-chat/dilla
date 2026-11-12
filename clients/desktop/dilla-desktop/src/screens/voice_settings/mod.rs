//! Area **voice settings** (Task 19): the Voice section, the microphone test and the synthesised sounds.
//!
//! The hooks below are where `crate::slots` draws this area inside another area's view. The section ([`section`]) draws
//! packages/web/src/screens/settings/Voice.tsx; the microphone test ([`mic_test`]) and the sounds ([`sounds`]) run on
//! dilla-media's device audio through one [`AudioHost`], the system's unless a test gives a fake one ([`set_audio_host`]).

pub mod devices;
pub mod keys;
pub mod mic_test;
pub mod section;
pub mod sounds;
#[cfg(any(test, feature = "automation"))]
pub mod testing;

use std::sync::{Arc, Mutex};

use dilla_media::devices::AudioHost;
use gpui::{AnyElement, App, Entity, IntoElement, Window};

use crate::slots::{SectionId, SlotCx};

/// The Voice section's view, made on first use and kept while the section is drawn.
pub fn section_view(
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> Entity<section::VoiceSection> {
    let view = window.use_keyed_state("voice-settings-section", cx, |window, cx| {
        section::VoiceSection::new(ctx.clone(), window, cx)
    });
    view.update(cx, |v, _| v.ctx = ctx.clone());
    view
}

/// Draws one of this area's settings sections (`slots::settings_sections` points here).
pub fn settings_section(
    _section: SectionId,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    section_view(ctx, window, cx).into_any_element()
}

/// Copy the web client has no counterpart of (it cannot lose a speaker: the browser plays on its own default): a speaker
/// that is gone with no default to take over (`MediaEvent::SpeakerLost`). The controller adds it to the generated strings
/// when the generator learns native-only copy (see the task report).
pub const SPEAKER_LOST: &str = "The speaker was disconnected and no other is available. Nothing is played until one is connected.";

static HOST: Mutex<Option<Arc<dyn AudioHost>>> = Mutex::new(None);

/// The audio host the section, the microphone test and the sounds use from now on (tests give a fake one).
pub fn set_audio_host(host: Arc<dyn AudioHost>) {
    *HOST
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(host);
}

/// Whether a host was given (the section watches the devices of a given host even without a client).
pub fn audio_host_is_set() -> bool {
    HOST.lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .is_some()
}

/// The audio host in use: the one given, else the system's.
pub fn audio_host() -> Arc<dyn AudioHost> {
    HOST.lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .clone()
        .unwrap_or_else(dilla_media::devices::system)
}
