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
    /// The browser this device is in a drawn fixture ("Chrome"): the web reference's viewer is the shooter's Chromium,
    /// so the fixture's own device is that browser, named from it and with no platform tag. Set only by the automation
    /// build's fixture (`dilla-desktop/src/fixture/devices.rs`); `None` in the app, which names itself.
    pub fixture_browser: Option<String>,
    /// The account-wide presence choice (`AccountState.presenceChoice`): the user panel's status menu. `None` until the
    /// first read of `GET /v1/accounts/me` lands (the panel then shows `online`, as the web's `?? 'online'`).
    pub presence_choice: Option<PresenceChoice>,
    /// Bumped when a presence choice is sent, so a read that began before it does not undo it (client-core
    /// `presenceChoiceWriteSeq`).
    pub presence_writes: u64,
}

/// client-core `PresenceChoice`: what the account asks others to see, on the wire 1 online, 2 idle, 3 dnd, 4 invisible
/// (`PATCH /v1/accounts/me/presence`, `GET /v1/accounts/me` index 6).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum PresenceChoice {
    #[default]
    Online,
    Idle,
    Dnd,
    Invisible,
}

impl PresenceChoice {
    /// The status menu's order (UserPanel.tsx `CHOICES`).
    pub const ALL: [PresenceChoice; 4] = [Self::Online, Self::Idle, Self::Dnd, Self::Invisible];

    /// The wire value (1..=4).
    pub fn wire(self) -> u8 {
        match self {
            Self::Online => 1,
            Self::Idle => 2,
            Self::Dnd => 3,
            Self::Invisible => 4,
        }
    }

    /// controller.ts `PRESENCE_CHOICES[(choice ?? 1) - 1] ?? 'online'`: an unknown value reads as online.
    pub fn of_wire(v: u64) -> Self {
        match v {
            2 => Self::Idle,
            3 => Self::Dnd,
            4 => Self::Invisible,
            _ => Self::Online,
        }
    }

    /// The id the strings and the menu use (`presence.<id>`).
    pub fn id(self) -> &'static str {
        match self {
            Self::Online => "online",
            Self::Idle => "idle",
            Self::Dnd => "dnd",
            Self::Invisible => "invisible",
        }
    }

    pub fn from_id(id: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|c| c.id() == id)
    }
}

/// `AccountState.user`'s profile fields.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct MyProfile {
    pub display: String,
    pub status_msg: String,
    pub bio: String,
}
