//! `trust`: Task 13's slice of [`super::AppState`]: contacts whose identity changed and the blocks they cause.
//!
//! The slice is a projection of the core's stored trust state (`trust_state`, `send_block_code`, `send_block_own`) and
//! nothing else: the core owns every trust decision (verified, changed, blocked), and the slice is read again from the
//! core whenever anything may have moved it (`crate::features::trust`). The UI never caches a verdict of its own: a
//! changed key never shows as verified, because `verified` is only ever the core's bit while nothing is blocked.
//!
//! The pure decisions the web draws from the slice are here too, so the desktop and the tests share them: the Privacy
//! row (settings/Privacy.tsx), the composer's block reason (Shell.tsx `blockReason`, `isChanged`) and the compare
//! dialog's approval rule (SafetyCompare.tsx). Nothing here holds key material: `umk` and `changed_umk` are public keys.

use std::collections::HashMap;

use super::{ChannelId, UserId};

/// core-port.ts `TrustBlock`: the stored verdict of a contact's leaves still in an active group.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum TrustBlock {
    UmkChanged,
    Credential,
    DeviceUnlisted,
    DeviceRevoked,
    TierMismatch,
    Binding,
}

impl TrustBlock {
    /// core-port.ts `trustBlock`: a code the port does not know reads as `E_CREDENTIAL` (fail closed: still a block).
    pub fn of(code: &str) -> Self {
        match code {
            "E_UMK_CHANGED" => Self::UmkChanged,
            "E_DEVICE_UNLISTED" => Self::DeviceUnlisted,
            "E_DEVICE_REVOKED" => Self::DeviceRevoked,
            "E_TIER_MISMATCH" => Self::TierMismatch,
            "E_BINDING" => Self::Binding,
            _ => Self::Credential,
        }
    }

    pub fn code(self) -> &'static str {
        match self {
            Self::UmkChanged => "E_UMK_CHANGED",
            Self::Credential => "E_CREDENTIAL",
            Self::DeviceUnlisted => "E_DEVICE_UNLISTED",
            Self::DeviceRevoked => "E_DEVICE_REVOKED",
            Self::TierMismatch => "E_TIER_MISMATCH",
            Self::Binding => "E_BINDING",
        }
    }
}

/// types.ts `TrustSummary`: one contact this device pinned, as the core reports it, with the name and the shared flag.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TrustSummary {
    pub user_id: UserId,
    /// The member's display name (else username) from the first loaded member list that has them, else the first 8 hex
    /// characters of the user id.
    pub name: String,
    /// A current group is shared with the contact: a community this device holds a group of, or a DM with them.
    pub shared: bool,
    /// The pinned public key, 64 lower-case hex digits.
    pub umk: String,
    pub first_seen: u64,
    /// The core's verified bit, and only while nothing is blocked (F4).
    pub verified: bool,
    /// The validated pending key that replaces `umk`, 64 lower-case hex digits.
    pub changed_umk: Option<String>,
    /// A failed leaf (not a changed key), a refusal the core stored nothing for, or history pending: the unlisted copy
    /// and no comparison.
    pub unlisted: bool,
    /// The channels (and DMs) whose validated Remove of the changed leaf cleared that group's send block while the key
    /// stays changed and unverified, sorted.
    pub removed_in: Vec<ChannelId>,
    pub blocked: Option<TrustBlock>,
}

impl TrustSummary {
    /// Shell.tsx `isChanged`: a validated pending key, or a leaf the core holds under a key no list presents.
    pub fn is_changed(&self) -> bool {
        self.changed_umk.is_some() || self.blocked == Some(TrustBlock::UmkChanged)
    }

    /// The key a comparison is for: the pending changed key, else the pinned one (the core's comparison key).
    pub fn comparison_key(&self) -> &str {
        self.changed_umk.as_deref().unwrap_or(&self.umk)
    }
}

/// types.ts `SendBlock`: the code the core refuses a send with in a conversation's group, and whether only own-user
/// leaves hold it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct SendBlock {
    pub code: TrustBlock,
    pub own: bool,
}

/// `AccountState.ownTrust`: an own-user leaf under another key, or an own device the list does not name.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum OwnTrust {
    Changed,
    Unlisted,
}

/// The slice.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct TrustState {
    /// The core has answered at least once since the client started; before that every conversation with a changed
    /// member is treated as blocked (Shell.tsx: fail closed until the first gate).
    pub loaded: bool,
    /// Every contact except the own account, by user id.
    pub contacts: Vec<TrustSummary>,
    /// `sendBlocks`: channel (or DM) → the block, only for blocked conversations.
    pub send_blocks: HashMap<ChannelId, SendBlock>,
    pub own: Option<OwnTrust>,
    /// The core could not read its trust state on the last pass: every contact reads as history pending (R3).
    pub failed: bool,
}

impl TrustState {
    pub fn contact(&self, user: UserId) -> Option<&TrustSummary> {
        self.contacts.iter().find(|c| c.user_id == user)
    }

    /// Shell.tsx `changedMembers`: the changed contacts among `audience` (the conversation's members) whose changed
    /// leaf was not removed from `channel` by a validated Remove.
    pub fn changed_in(&self, channel: ChannelId, audience: &[UserId]) -> Vec<&TrustSummary> {
        self.contacts
            .iter()
            .filter(|c| {
                c.is_changed() && audience.contains(&c.user_id) && !c.removed_in.contains(&channel)
            })
            .collect()
    }

    /// Shell.tsx `removedHere`: a changed contact's leaf left `channel` by a validated Remove.
    pub fn removed_here(&self, channel: ChannelId) -> bool {
        self.contacts
            .iter()
            .any(|c| c.is_changed() && c.removed_in.contains(&channel))
    }

    /// The composer's gate for `channel` (Shell.tsx `gate`): the core's block, else, before the core first answered, a
    /// changed member of the conversation blocks it (fail closed).
    pub fn gate(&self, channel: ChannelId, audience: &[UserId]) -> Option<SendBlock> {
        if !self.loaded {
            let changed = self.changed_in(channel, audience);
            return (!changed.is_empty()).then_some(SendBlock {
                code: TrustBlock::UmkChanged,
                own: false,
            });
        }
        self.send_blocks.get(&channel).copied()
    }

    /// The `strings.rs` key of the composer's block reason in `channel`, or `None` when sending is open.
    pub fn block_reason(&self, channel: ChannelId, audience: &[UserId]) -> Option<&'static str> {
        let block = self.gate(channel, audience)?;
        Some(block_reason(block, &self.changed_in(channel, audience)))
    }
}

/// Shell.tsx `blockReason`: a block only own-user leaves hold names the own account; a changed key asks for a comparison
/// only when a changed member of the conversation has a key to compare, else it says there is nothing to compare yet; any
/// other failed leaf is `privacy.leafFailed`.
pub fn block_reason(block: SendBlock, changed: &[&TrustSummary]) -> &'static str {
    if block.own {
        return if block.code == TrustBlock::UmkChanged {
            "privacy.ownChanged"
        } else {
            "privacy.ownUnlisted"
        };
    }
    if block.code != TrustBlock::UmkChanged {
        return "privacy.leafFailed";
    }
    if changed.iter().any(|c| c.changed_umk.is_some()) {
        "privacy.changed"
    } else {
        "privacy.changedNoKey"
    }
}

/// What the Privacy section draws for one contact (settings/Privacy.tsx), as plain decisions.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PrivacyRow {
    /// The changed state: an alert (`TrustAlert`) when a group is shared, else plain danger text. The copy key is
    /// `privacy.changed` with a key to compare, `privacy.changedNoKey` without one.
    pub changed: Option<&'static str>,
    /// The changed alert stands as an alert (a shared group): otherwise the state is plain text (M-04).
    pub alert: bool,
    /// The alert's Compare action (a key to compare).
    pub alert_compare: bool,
    /// The unlisted copy (`privacy.unlisted`), plain warning text.
    pub unlisted: bool,
    /// The verified label and the compared public key.
    pub verified_key: Option<String>,
    /// The first-seen label and text.
    pub first_seen: bool,
    /// `privacy.noShared` instead of a comparison.
    pub no_shared: bool,
    /// The ghost Compare button.
    pub compare: bool,
}

/// settings/Privacy.tsx, one row.
pub fn privacy_row(record: &TrustSummary) -> PrivacyRow {
    let changed = record.is_changed();
    let comparable = record.changed_umk.is_some();
    let unlisted = record.unlisted;
    let copy = if comparable {
        "privacy.changed"
    } else {
        "privacy.changedNoKey"
    };
    PrivacyRow {
        changed: changed.then_some(copy),
        alert: changed && record.shared,
        alert_compare: changed && record.shared && comparable,
        unlisted,
        verified_key: (!unlisted && !changed && record.verified).then(|| record.umk.clone()),
        first_seen: !unlisted && !changed && !record.verified,
        no_shared: !record.shared,
        compare: record.shared && !changed && !unlisted,
    }
}

/// SafetyCompare.tsx `validGroups`: twelve groups of five digits.
pub fn valid_groups(groups: &[String]) -> bool {
    groups.len() == 12
        && groups
            .iter()
            .all(|g| g.len() == 5 && g.bytes().all(|b| b.is_ascii_digit()))
}

/// The core's safety number (60 digits, possibly spaced) as the twelve groups of five the dialog shows; empty when it
/// is not exactly 60 digits.
pub fn safety_groups(number: &str) -> Vec<String> {
    let digits: String = number.chars().filter(|c| !c.is_whitespace()).collect();
    if digits.len() != 60 || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return Vec::new();
    }
    digits
        .as_bytes()
        .chunks(5)
        .map(|c| String::from_utf8_lossy(c).into_owned())
        .collect()
}

/// 64 lower-case hex digits (a public key as the dialog shows it).
pub fn is_key_hex(s: &str) -> bool {
    s.len() == 64 && s.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}

/// SafetyCompare.tsx `permitted`: All 12 blocks match approves only with a verifiable state, the exact public key shown,
/// twelve valid groups and the person's confirmation of exactly this comparison. Copying the number is not part of it.
pub fn compare_permitted(
    can_verify: bool,
    approved_umk: Option<&str>,
    groups: &[String],
    confirmed: bool,
) -> bool {
    can_verify && approved_umk.is_some_and(is_key_hex) && valid_groups(groups) && confirmed
}

/// SafetyCompare.tsx `qrValue`: `dilla-sn:1:<user id>:<groups joined by "-">`, only for twelve valid groups.
pub fn safety_qr_value(user: UserId, groups: &[String]) -> Option<String> {
    valid_groups(groups).then(|| format!("dilla-sn:1:{}:{}", user.hex(), groups.join("-")))
}

/// The dialog's state line: what the contact is to this device.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CompareState {
    FirstSeen,
    Changed,
    Verified,
    Unlisted,
}

impl CompareState {
    pub fn of(record: &TrustSummary) -> Self {
        if record.unlisted {
            Self::Unlisted
        } else if record.is_changed() {
            Self::Changed
        } else if record.verified {
            Self::Verified
        } else {
            Self::FirstSeen
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ada() -> TrustSummary {
        TrustSummary {
            user_id: UserId([0xaa; 16]),
            name: "Ada".into(),
            shared: true,
            umk: "bb".repeat(32),
            first_seen: 1,
            verified: false,
            changed_umk: Some("cc".repeat(32)),
            unlisted: false,
            removed_in: Vec::new(),
            blocked: None,
        }
    }

    // Privacy.test.tsx: "keeps changed contact visible and offers a named compare action".
    #[test]
    fn changed_contact_has_an_alert_with_compare() {
        let row = privacy_row(&ada());
        assert_eq!(row.changed, Some("privacy.changed"));
        assert!(row.alert && row.alert_compare);
        assert!(!row.compare, "the alert holds the one compare action");
    }

    // Privacy.test.tsx: "shows a changed identity without offering comparison when no group is shared".
    #[test]
    fn changed_without_a_shared_group_is_plain_text_and_no_compare() {
        let row = privacy_row(&TrustSummary {
            shared: false,
            ..ada()
        });
        assert!(
            !row.alert,
            "M-04: no alert beside a line saying they cannot compare"
        );
        assert_eq!(row.changed, Some("privacy.changed"));
        assert!(row.no_shared);
        assert!(!row.compare && !row.alert_compare);
    }

    // Privacy.test.tsx: "names an unlisted device and offers no comparison for it".
    #[test]
    fn unlisted_has_no_comparison_and_no_first_seen() {
        let row = privacy_row(&TrustSummary {
            changed_umk: None,
            unlisted: true,
            ..ada()
        });
        assert!(row.unlisted && !row.compare && !row.first_seen);
    }

    // Privacy.test.tsx: "shows a verified contact with the exact public key compared".
    #[test]
    fn verified_shows_the_key_and_compare() {
        let row = privacy_row(&TrustSummary {
            changed_umk: None,
            verified: true,
            ..ada()
        });
        assert_eq!(row.verified_key.as_deref(), Some("bb".repeat(32).as_str()));
        assert!(!row.first_seen && row.compare);
    }

    // Privacy.test.tsx: "shows a history-pending contact as unlisted, without its verified key or a comparison".
    #[test]
    fn history_pending_hides_the_verified_key() {
        let row = privacy_row(&TrustSummary {
            changed_umk: None,
            verified: true,
            unlisted: true,
            ..ada()
        });
        assert!(row.unlisted && row.verified_key.is_none() && !row.compare);
    }

    // Privacy.test.tsx: "shows a changed contact with no key to compare without Compare" (C1).
    #[test]
    fn changed_with_no_key_is_an_alert_without_compare() {
        let row = privacy_row(&TrustSummary {
            changed_umk: None,
            blocked: Some(TrustBlock::UmkChanged),
            ..ada()
        });
        assert_eq!(row.changed, Some("privacy.changedNoKey"));
        assert!(row.alert && !row.alert_compare && !row.compare && !row.first_seen);
    }

    // Privacy.test.tsx: a first-seen contact offers Compare.
    #[test]
    fn first_seen_offers_compare() {
        let row = privacy_row(&TrustSummary {
            changed_umk: None,
            ..ada()
        });
        assert!(row.first_seen && row.compare && row.verified_key.is_none());
    }

    #[test]
    fn a_changed_key_is_never_verified_in_the_dialog() {
        let r = TrustSummary {
            verified: true,
            ..ada()
        };
        assert_eq!(CompareState::of(&r), CompareState::Changed);
        assert_eq!(r.comparison_key(), "cc".repeat(32));
    }

    #[test]
    fn block_reasons_follow_shell() {
        let a = ada();
        let changed = [&a];
        let block = |code, own| SendBlock { code, own };
        assert_eq!(
            block_reason(block(TrustBlock::UmkChanged, true), &changed),
            "privacy.ownChanged"
        );
        assert_eq!(
            block_reason(block(TrustBlock::DeviceUnlisted, true), &changed),
            "privacy.ownUnlisted"
        );
        assert_eq!(
            block_reason(block(TrustBlock::Credential, false), &changed),
            "privacy.leafFailed"
        );
        assert_eq!(
            block_reason(block(TrustBlock::UmkChanged, false), &changed),
            "privacy.changed"
        );
        assert_eq!(
            block_reason(block(TrustBlock::UmkChanged, false), &[]),
            "privacy.changedNoKey"
        );
    }

    #[test]
    fn the_gate_fails_closed_before_the_core_answers() {
        let channel = ChannelId([1; 16]);
        let mut s = TrustState {
            contacts: vec![ada()],
            ..TrustState::default()
        };
        let audience = [UserId([0xaa; 16])];
        assert_eq!(
            s.block_reason(channel, &audience),
            Some("privacy.changed"),
            "a changed member blocks before the first gate"
        );
        assert_eq!(s.block_reason(channel, &[]), None);
        s.loaded = true;
        assert_eq!(
            s.block_reason(channel, &audience),
            None,
            "the core's gate decides"
        );
        s.send_blocks.insert(
            channel,
            SendBlock {
                code: TrustBlock::UmkChanged,
                own: false,
            },
        );
        assert_eq!(s.block_reason(channel, &audience), Some("privacy.changed"));
        s.contacts[0].removed_in.push(channel);
        assert!(s.removed_here(channel));
        assert!(s.changed_in(channel, &audience).is_empty());
    }

    // SafetyCompare.test.tsx: the approval needs the exact key, twelve groups and the confirmation; copy never approves.
    #[test]
    fn approval_needs_key_groups_and_confirmation() {
        let groups = safety_groups(&"0123456789".repeat(6));
        assert_eq!(groups.len(), 12);
        assert_eq!(groups[0], "01234");
        let key = "ab".repeat(32);
        assert!(compare_permitted(true, Some(&key), &groups, true));
        assert!(!compare_permitted(true, Some(&key), &groups, false));
        assert!(!compare_permitted(false, Some(&key), &groups, true));
        assert!(!compare_permitted(true, None, &groups, true));
        assert!(!compare_permitted(
            true,
            Some(&"AB".repeat(32)),
            &groups,
            true
        ));
        assert!(!compare_permitted(true, Some(&key), &groups[..11], true));
        let mut bad = groups.clone();
        bad[3] = "12a45".into();
        assert!(!compare_permitted(true, Some(&key), &bad, true));
        assert!(safety_groups("123").is_empty());
        assert_eq!(
            safety_groups(&"01234 ".repeat(12)).len(),
            12,
            "spacing is ignored"
        );
    }

    #[test]
    fn qr_value_only_for_twelve_groups() {
        let groups = safety_groups(&"9".repeat(60));
        let user = UserId([0xab; 16]);
        assert_eq!(
            safety_qr_value(user, &groups).unwrap(),
            format!("dilla-sn:1:{}:{}", "ab".repeat(16), ["99999"; 12].join("-"))
        );
        assert_eq!(safety_qr_value(user, &groups[..3]), None);
    }

    #[test]
    fn unknown_block_codes_fail_closed() {
        assert_eq!(TrustBlock::of("E_SOMETHING"), TrustBlock::Credential);
        for b in [
            TrustBlock::UmkChanged,
            TrustBlock::Credential,
            TrustBlock::DeviceUnlisted,
            TrustBlock::DeviceRevoked,
            TrustBlock::TierMismatch,
            TrustBlock::Binding,
        ] {
            assert_eq!(TrustBlock::of(b.code()), b);
        }
    }
}
