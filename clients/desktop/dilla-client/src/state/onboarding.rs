//! `onboarding`: Task 25's slice of [`super::AppState`]: a running client's join and create flows (sign-up itself runs
//! before a client exists). Nothing here holds key material: an invite link is a capability to join a server, never a
//! key, and it is held only in memory for the session (Create.tsx `madeInvites`).

use std::collections::HashMap;

use super::CommunityId;

/// The last invite made per server in this session: reused while more than a day of it is left, so neither an open of
/// the invite dialog nor a switch between servers mints another (Create.tsx WB m3).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct HeldInvite {
    pub url: String,
    pub code: String,
    /// Unix seconds; 0 never expires.
    pub expires: u64,
    /// When it was made, unix seconds (this device's clock).
    pub made_at: u64,
}

/// The onboarding area's slice.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct OnboardingState {
    /// The last invite made per server ([`crate::features::signup::Onboarding::create_invite`]).
    pub invites: HashMap<CommunityId, HeldInvite>,
}

impl OnboardingState {
    /// The invite to show again for `community` at `now` (unix seconds): one made within the last
    /// `ttl_days - 1` days, else `None` (a new one is made).
    pub fn reusable_invite(
        &self,
        community: CommunityId,
        now: u64,
        ttl_days: u64,
    ) -> Option<&HeldInvite> {
        self.invites
            .get(&community)
            .filter(|i| now.saturating_sub(i.made_at) < ttl_days.saturating_sub(1) * 24 * 3600)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_invite_is_reused_until_a_day_is_left() {
        let mut s = OnboardingState::default();
        let c = CommunityId([1; 16]);
        s.invites.insert(
            c,
            HeldInvite {
                url: "u".into(),
                code: "c".into(),
                expires: 0,
                made_at: 1000,
            },
        );
        assert!(s.reusable_invite(c, 1000 + 6 * 86400 - 1, 7).is_some());
        assert!(s.reusable_invite(c, 1000 + 6 * 86400, 7).is_none());
        assert!(s.reusable_invite(CommunityId([2; 16]), 1000, 7).is_none());
    }
}
