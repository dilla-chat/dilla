//! Settings -> Devices, without any view: what each row says and which dialog each action opens. A port of the text and
//! decisions of `packages/web/src/screens/settings/{Devices,RenameDialog,KeyDialog,SignOut}.tsx`.
//!
//! A device is titled by what the client knows ("Desktop on Linux"), never by an opaque id. Any other device shows the label it
//! stored, and a nameless one "Unnamed device" (never a guess). The device's own id is on every row (a label is the server's to
//! change): "this device · active now · d3f9a1" for this device, "active 2 days ago · 8b20ce" for another.

use dilla_client::features::devices::{
    DEVICE_NAME_MAX, device_label, order_devices, own_platform_tag,
};
use dilla_client::state::profile::DeviceEntry;

use crate::strings::{t, tf};

/// `timeAgo` (SignInFactors.tsx): "3 weeks ago" for a unix time, in the largest unit that fits, as `Intl.RelativeTimeFormat`
/// with `numeric: 'auto'` says it in English.
pub fn time_ago(unix: u64, now: u64) -> String {
    let s = now.saturating_sub(unix);
    let steps: [(&str, &str, &str, u64); 6] = [
        ("year", "last year", "years", 31_536_000),
        ("month", "last month", "months", 2_592_000),
        ("week", "last week", "weeks", 604_800),
        ("day", "yesterday", "days", 86_400),
        ("hour", "", "hours", 3600),
        ("minute", "", "minutes", 60),
    ];
    for (unit, last, plural, size) in steps {
        if s >= size {
            let n = s / size;
            return match (n, last.is_empty()) {
                (1, false) => last.to_owned(),
                (1, true) => format!("1 {unit} ago"),
                _ => format!("{n} {plural} ago"),
            };
        }
    }
    "now".to_owned()
}

/// What this device calls itself when the server holds no name for it (the web's "Chrome on Linux" from the user agent; the
/// native app has no user agent, so it names itself and its system). `browser`: a drawn fixture's browser
/// (`ProfileState::fixture_browser`), which names this device instead.
pub fn suggested_name(browser: Option<&str>) -> String {
    let os = match dilla_client::features::devices::this_platform() {
        "darwin" => "macOS",
        "win32" => "Windows",
        "linux" => "Linux",
        other => other,
    };
    tf(
        "devices.renameSuggestion",
        &[("browser", browser.unwrap_or("Dilla")), ("os", os)],
    )
}

/// The title of a device: its stored label; for this device without one, the suggestion; else "Unnamed device".
pub fn title_of(d: &DeviceEntry, browser: Option<&str>) -> String {
    match &d.label {
        Some(l) => l.clone(),
        None if d.own => suggested_name(browser),
        None => t("devices.unnamed").to_owned(),
    }
}

/// The first characters of the id shown on every row.
pub fn short_id(d: &DeviceEntry, n: usize) -> String {
    d.id.hex().chars().take(n).collect()
}

/// The row's sub line: the state ("removed", "not yet in the device list") and the activity line.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Meta {
    pub state: Option<&'static str>,
    pub seen: String,
}

pub fn meta_of(d: &DeviceEntry, now: u64) -> Meta {
    let state = if d.revoked_at.is_some() {
        Some(t("devices.revoked"))
    } else if !d.listed {
        Some(t("devices.unlisted"))
    } else {
        None
    };
    let id6 = short_id(d, 6);
    let seen = if d.own {
        tf("devices.subOwn", &[("id", &id6)])
    } else if d.last_seen > 0 {
        tf(
            "devices.subOther",
            &[("when", &time_ago(d.last_seen, now)), ("id", &id6)],
        )
    } else {
        id6
    };
    Meta { state, seen }
}

/// What a row's button does.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RowAction {
    /// This device: Sign out.
    SignOut,
    /// A device the signed list names: removed with the recovery key.
    Revoke,
    /// A live device in no signed list: removed without a key.
    RemoveUnlisted,
}

/// The button a row draws: none for a removed device; this device signs out; every other live device is removed.
pub fn action_of(d: &DeviceEntry) -> Option<RowAction> {
    if d.revoked_at.is_some() {
        None
    } else if d.own {
        Some(RowAction::SignOut)
    } else if d.listed {
        Some(RowAction::Revoke)
    } else {
        Some(RowAction::RemoveUnlisted)
    }
}

/// Rename is in a live row's context menu.
pub fn can_rename(d: &DeviceEntry) -> bool {
    d.revoked_at.is_none()
}

/// The rows in display order.
pub fn ordered(devices: &[DeviceEntry]) -> Vec<DeviceEntry> {
    order_devices(devices)
}

/// This device's tag: "Desktop · Linux"; none in a browser (a drawn fixture's, `browser`), as the web shows none there.
pub fn own_tag(browser: Option<&str>) -> Option<String> {
    browser.is_none().then(own_platform_tag)
}

/// The text a rename dialog starts on: the current name; this device without one, the suggestion.
pub fn rename_start(d: &DeviceEntry, browser: Option<&str>) -> String {
    match &d.label {
        Some(l) => l.clone(),
        None if d.own => suggested_name(browser),
        None => String::new(),
    }
}

/// What a rename sends: `None` clears the name; `Err` when the text is not a name.
/// A name the server would refuse.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct BadName;

pub fn rename_label(typed: &str) -> Result<Option<String>, BadName> {
    let trimmed = typed.trim();
    if trimmed.is_empty() {
        return Ok(None);
    }
    device_label(typed).map(Some).ok_or(BadName)
}

/// The recovery-key dialog: the key is counted as it is typed and a submit before the count is 52 sends nothing.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct KeyEntry {
    /// The characters counted (separators and case do not count: `normaliseRecoveryKey`).
    pub counted: usize,
    pub field_error: Option<&'static str>,
    pub banner: Option<String>,
    pub busy: bool,
}

impl KeyEntry {
    pub fn count(typed: &str) -> usize {
        dilla_client::signin::normalise_recovery_key(typed)
            .chars()
            .count()
    }

    pub fn short(&self) -> bool {
        self.counted != dilla_client::features::devices::RECOVERY_KEY_LENGTH
    }

    pub fn typed(&mut self, typed: &str) {
        self.counted = Self::count(typed);
        self.field_error = None;
    }

    /// A submit: `true` when the key may be sent; a short key shows the length error instead.
    pub fn submit(&mut self) -> bool {
        if self.busy {
            return false;
        }
        if self.short() {
            self.field_error = Some("signin.error.keyLength");
            return false;
        }
        self.busy = true;
        self.field_error = None;
        self.banner = None;
        true
    }

    /// A refusal: a wrong key is a field error with the text kept; any other is the banner by its code.
    pub fn refused(&mut self, code: &str) {
        self.busy = false;
        if code == "E_RECOVERY_KEY" {
            self.field_error = Some("devices.error.wrongKey");
        } else {
            self.banner = Some(tf("settings.error.other", &[("code", code)]));
        }
    }

    pub fn hint(&self) -> String {
        tf("signin.key.hint", &[("n", &self.counted.to_string())])
    }
}

pub const NAME_MAX: usize = DEVICE_NAME_MAX;

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::DeviceId;

    fn device(
        id: u8,
        own: bool,
        last_seen: u64,
        revoked_at: Option<u64>,
        listed: bool,
    ) -> DeviceEntry {
        DeviceEntry {
            id: DeviceId([id; 16]),
            tier: 1,
            signer_tier: 1,
            last_seen,
            revoked_at,
            label: None,
            listed,
            own,
        }
    }

    #[test]
    fn time_ago_names_the_largest_unit_that_fits() {
        let now = 1_000_000_000;
        assert_eq!(time_ago(now, now), "now");
        assert_eq!(time_ago(now - 59, now), "now");
        assert_eq!(time_ago(now - 60, now), "1 minute ago");
        assert_eq!(time_ago(now - 7200, now), "2 hours ago");
        assert_eq!(time_ago(now - 86_400, now), "yesterday");
        assert_eq!(time_ago(now - 2 * 86_400, now), "2 days ago");
        assert_eq!(time_ago(now - 8 * 86_400, now), "last week");
        assert_eq!(time_ago(now - 3 * 604_800, now), "3 weeks ago");
        assert_eq!(time_ago(now - 40 * 86_400, now), "last month");
        assert_eq!(time_ago(now - 400 * 86_400, now), "last year");
    }

    #[test]
    fn a_row_says_what_it_is_and_its_id() {
        let own = device(0xd3, true, 5, None, true);
        let other = device(0x8b, false, 1_000_000_000 - 2 * 86_400, None, true);
        let m = meta_of(&own, 1_000_000_000);
        assert_eq!(m.seen, "this device · active now · d3d3d3");
        let m = meta_of(&other, 1_000_000_000);
        assert_eq!(m.seen, "active 2 days ago · 8b8b8b");
        assert_eq!(m.state, None);
        let never = device(0x11, false, 0, None, true);
        assert_eq!(meta_of(&never, 5).seen, "111111");
        assert_eq!(
            meta_of(&device(1, false, 1, Some(9), true), 5).state,
            Some("removed")
        );
        assert_eq!(
            meta_of(&device(1, false, 1, None, false), 5).state,
            Some("not yet in the device list")
        );
    }

    #[test]
    fn titles_never_show_an_id() {
        let mut d = device(1, false, 1, None, true);
        assert_eq!(title_of(&d, None), "Unnamed device");
        d.label = Some("Laptop".into());
        assert_eq!(title_of(&d, None), "Laptop");
        let own = device(2, true, 1, None, true);
        assert!(title_of(&own, None).starts_with("Dilla on "));
        assert!(own_tag(None).is_some());
    }

    /// A drawn fixture's own device is the shooter's browser: named from it, with no platform tag (the web's row).
    #[test]
    fn a_fixture_s_own_device_is_the_shooter_s_browser() {
        let own = device(2, true, 1, None, true);
        assert!(title_of(&own, Some("Chrome")).starts_with("Chrome on "));
        assert_eq!(own_tag(Some("Chrome")), None);
    }

    /// Devices.tsx: this device signs out, every other live device is removed (with the key when the signed list names it),
    /// and a removed device has no button and no rename.
    #[test]
    fn the_row_button_follows_the_device() {
        assert_eq!(
            action_of(&device(1, true, 1, None, true)),
            Some(RowAction::SignOut)
        );
        assert_eq!(
            action_of(&device(1, false, 1, None, true)),
            Some(RowAction::Revoke)
        );
        assert_eq!(
            action_of(&device(1, false, 1, None, false)),
            Some(RowAction::RemoveUnlisted)
        );
        assert_eq!(action_of(&device(1, false, 1, Some(3), true)), None);
        assert!(!can_rename(&device(1, false, 1, Some(3), true)));
        assert!(can_rename(&device(1, false, 1, None, true)));
    }

    #[test]
    fn a_rename_trims_and_an_empty_name_clears() {
        assert_eq!(rename_label("  Desk "), Ok(Some("Desk".into())));
        assert_eq!(rename_label("   "), Ok(None));
        assert_eq!(rename_label(&"a".repeat(65)), Err(BadName));
        assert_eq!(rename_label("a\tb"), Err(BadName));
    }

    /// KeyDialog.tsx: a submit before the count is 52 sends nothing; a wrong key is a field error; any other refusal is the
    /// banner by its code; the key text is kept (by the view) in every case.
    #[test]
    fn the_key_dialog_counts_refuses_and_reports() {
        let mut k = KeyEntry::default();
        k.typed("ABCD-EFGH");
        assert_eq!(k.counted, 8);
        assert_eq!(k.hint(), "8 of 52 characters");
        assert!(!k.submit());
        assert_eq!(k.field_error, Some("signin.error.keyLength"));
        k.typed(&"A".repeat(52));
        assert!(!k.short());
        assert!(k.submit() && k.busy);
        assert!(
            !k.submit(),
            "a second press while the command runs sends nothing"
        );
        k.refused("E_RECOVERY_KEY");
        assert!(!k.busy);
        assert_eq!(k.field_error, Some("devices.error.wrongKey"));
        assert!(k.submit());
        k.refused("E_LIST_RACE");
        assert_eq!(
            k.banner.as_deref(),
            Some("That did not work (E_LIST_RACE). Try again.")
        );
    }
}
