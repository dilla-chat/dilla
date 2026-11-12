//! The machine's microphones and speakers, through libwebrtc's platform audio device module
//! (LiveKit's `PlatformAudio`). Listing them opens no device and creates no PeerConnection.

use livekit::{PlatformAudio, PlayoutDeviceInfo, RecordingDeviceInfo};

use crate::MediaError;

/// One audio device: `id` is the platform's stable id (what [`crate::Mic::Device`] takes), `name`
/// what the settings show.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Device {
    pub id: String,
    pub name: String,
}

impl From<RecordingDeviceInfo> for Device {
    fn from(d: RecordingDeviceInfo) -> Self {
        Self {
            id: d.id.as_str().to_owned(),
            name: d.name,
        }
    }
}

impl From<PlayoutDeviceInfo> for Device {
    fn from(d: PlayoutDeviceInfo) -> Self {
        Self {
            id: d.id.as_str().to_owned(),
            name: d.name,
        }
    }
}

fn platform() -> Result<PlatformAudio, MediaError> {
    PlatformAudio::new().map_err(|e| MediaError::Audio(e.to_string()))
}

/// The microphones. Empty on a machine without any (a headless CI runner).
pub fn inputs() -> Result<Vec<Device>, MediaError> {
    Ok(platform()?.recording_devices().map(Device::from).collect())
}

/// The speakers and headsets.
pub fn outputs() -> Result<Vec<Device>, MediaError> {
    Ok(platform()?.playout_devices().map(Device::from).collect())
}
