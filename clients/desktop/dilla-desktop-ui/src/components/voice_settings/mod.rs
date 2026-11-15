//! Task 19's components (`components/voice_settings/`): the Voice section's range, sensitivity meter, controls and page
//! furniture (packages/ui `VoiceSettings`, `SensitivityMeter`). Every file under this directory is Task 19's.

pub mod controls;
pub mod layout;
pub mod sensitivity_meter;
pub mod slider;

pub use controls::{CheckButton, KeyCapture};
pub use layout::{Heard, StatusPill};
pub use sensitivity_meter::SensitivityMeter;
pub use slider::Slider;
