//! `profile`: Task 16's slice of [`super::AppState`]: the account's profile, its device list and the appearance settings.
//!
//! Ports: the account's `user` fields (`display`, `statusMsg`, `bio`) and the `devices` and `settings` slices of
//! `packages/client-core`. Nothing here holds key material: a device is listed by its id, tier, label and times, never by its
//! keys. [`Default`] is the empty slice.

use super::DeviceId;

/// `DeviceSummary` (client-core `state/types.ts`): one row of Settings -> Devices.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct DeviceEntry {
    pub id: DeviceId,
    /// 1 browser custody (the web client and the desktop app alike today), 0 native.
    pub tier: u8,
    pub signer_tier: u8,
    /// Unix seconds, 0 when never seen.
    pub last_seen: u64,
    /// Unix seconds the device was removed, `None` while it is live.
    pub revoked_at: Option<u64>,
    /// The name the server stores for it, `None` when it has none.
    pub label: Option<String>,
    /// Whether the account's signed device list names it (an unlisted live device signed in with the password only).
    pub listed: bool,
    /// This device.
    pub own: bool,
}

/// The `density` setting (`settings.ts`: only these two values are admitted; the stylesheet's `cozy` step is not a setting).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum DensitySetting {
    #[default]
    Regular,
    Compact,
}

impl DensitySetting {
    pub fn id(self) -> &'static str {
        match self {
            Self::Regular => "regular",
            Self::Compact => "compact",
        }
    }

    pub fn from_id(id: &str) -> Option<Self> {
        match id {
            "regular" => Some(Self::Regular),
            "compact" => Some(Self::Compact),
            _ => None,
        }
    }
}

/// The `reduceMotion` setting.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum MotionSetting {
    #[default]
    System,
    On,
    Off,
}

impl MotionSetting {
    pub fn id(self) -> &'static str {
        match self {
            Self::System => "system",
            Self::On => "on",
            Self::Off => "off",
        }
    }

    pub fn from_id(id: &str) -> Option<Self> {
        match id {
            "system" => Some(Self::System),
            "on" => Some(Self::On),
            "off" => Some(Self::Off),
            _ => None,
        }
    }
}

/// The settings key of each appearance setting in the store's `app_settings` (client-core `SETTINGS`).
pub const KEY_DENSITY: &str = "density";
pub const KEY_REDUCE_MOTION: &str = "reduceMotion";

#[derive(Clone, Debug, Default, PartialEq)]
pub struct ProfileState {
    /// The account's own profile, as the server stores it. `None` until the first read lands.
    pub me: Option<MyProfile>,
    /// Bumped by every saved profile, so a form can tell a save landing from a read (client-core `profileSeq`).
    pub saves: u64,
    /// The device list, in the order the server returned it: [`crate::features::devices::order_devices`] is the display order.
    pub devices: Vec<DeviceEntry>,
    /// Whether the list has been read once.
    pub devices_loaded: bool,
    /// The recovery data stored for the account is not this account's (`AccountState.rootMismatch`).
    pub root_mismatch: bool,
    pub density: DensitySetting,
    pub reduce_motion: MotionSetting,
    /// This device signed itself out and removed itself: the app wipes the store and shows the sign-in again.
    pub signed_out: bool,
}

/// `AccountState.user`'s profile fields.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct MyProfile {
    pub display: String,
    pub status_msg: String,
    pub bio: String,
}
