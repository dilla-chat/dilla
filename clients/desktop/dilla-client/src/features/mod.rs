//! One module per feature of the screens plan's Wave 1 areas (clients/desktop/OWNERSHIP.md). Each module is one task's
//! file: its handle (`Client::<feature>()`, cheap to clone, no I/O on construction), its commands, and what it publishes
//! into its own [`crate::state`] slice through the client's publisher. The module list is Task 1's; no Wave 1 task adds
//! a line here.

pub mod attachments;
pub mod bot;
pub mod bot_admin;
pub mod browser_auth;
pub mod devices;
pub mod diagnostics;
pub mod dms;
pub mod enrol;
pub mod factors;
pub mod gifs;
pub mod members;
pub mod messages;
pub mod moderation;
pub mod music;
pub mod notify;
pub mod profile;
pub mod reactions;
pub mod ring;
pub mod role_panels;
pub mod roles;
pub mod search;
pub mod server_admin;
pub mod signup;
pub mod threads;
pub mod trust;
pub mod utilities;
pub mod voice;
pub mod voice_prefs;

/// The Wave 1 areas of the ownership map (clients/desktop/OWNERSHIP.md), one per owning task. The gateway's frames that
/// the sync engine does not consume are routed to one of these ([`crate::sync::route`]); the desktop's slots and
/// settings registry name them too.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub enum Area {
    Shell,
    Messages,
    Reactions,
    Threads,
    Attachments,
    Dms,
    Members,
    ServerSettings,
    UserSettings,
    Voice,
    Ring,
    VoiceSettings,
    Bot,
    BotAdmin,
    Notify,
    Search,
    Diagnostics,
    Onboarding,
    Factors,
    Instances,
}

impl Area {
    pub const ALL: [Area; 20] = [
        Area::Shell,
        Area::Messages,
        Area::Reactions,
        Area::Threads,
        Area::Attachments,
        Area::Dms,
        Area::Members,
        Area::ServerSettings,
        Area::UserSettings,
        Area::Voice,
        Area::Ring,
        Area::VoiceSettings,
        Area::Bot,
        Area::BotAdmin,
        Area::Notify,
        Area::Search,
        Area::Diagnostics,
        Area::Onboarding,
        Area::Factors,
        Area::Instances,
    ];

    /// The screens plan's task that owns the area.
    pub fn task(self) -> u8 {
        match self {
            Area::Shell => 8,
            Area::Messages => 9,
            Area::Reactions => 10,
            Area::Threads => 11,
            Area::Attachments => 12,
            Area::Dms => 13,
            Area::Members => 14,
            Area::ServerSettings => 15,
            Area::UserSettings => 16,
            Area::Voice => 17,
            Area::Ring => 18,
            Area::VoiceSettings => 19,
            Area::Bot => 20,
            Area::BotAdmin => 21,
            Area::Notify => 22,
            Area::Search => 23,
            Area::Diagnostics => 24,
            Area::Onboarding => 25,
            Area::Factors => 26,
            Area::Instances => 27,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::Area;

    #[test]
    fn every_wave_1_task_owns_one_area() {
        let tasks: Vec<u8> = Area::ALL.iter().map(|a| a.task()).collect();
        assert_eq!(tasks, (8..=27).collect::<Vec<u8>>());
    }
}
