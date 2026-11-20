//! `removal_notices`: the servers the community list stopped showing without this account leaving or deleting them
//! (controller.ts `removals`, 3073-3100): a kick, a ban or a server deleted by its owner. The web shows each as a warn
//! banner "You are no longer in {server}" until dismissed.
//!
//! [`SelfRemoved`] is the mark that tells this account's own leave or delete from a removal (admin/controller.ts
//! 197-214, controller.ts 3087 and 3155-3165).

use std::collections::{HashMap, HashSet};
use std::fmt;

use super::CommunityId;

/// How many notices are kept, the newest (controller.ts `REMOVALS_MAX`).
pub const REMOVALS_MAX: usize = 8;

/// One "You are no longer in {name}" notice. `id` grows with every notice of the client, so a dismissal by id never
/// hides a later notice of the same server.
#[derive(Clone, PartialEq, Eq)]
pub struct RemovalNotice {
    pub id: u64,
    pub community: CommunityId,
    pub name: String,
}

// The server's name is tied to a removal (R20/R21): Debug shows the ids and the name's length only
// (`a_notice_does_not_print_its_server_name`).
impl fmt::Debug for RemovalNotice {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("RemovalNotice")
            .field("id", &self.id)
            .field("community", &self.community)
            .field("name_len", &self.name.len())
            .finish()
    }
}

/// The servers this account left or deleted itself, or is leaving or deleting now.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SelfRemoved {
    removed: HashSet<CommunityId>,
    pending: HashSet<CommunityId>,
}

impl SelfRemoved {
    /// Set before a leave or delete request.
    pub fn begin(&mut self, c: CommunityId) {
        self.removed.insert(c);
        self.pending.insert(c);
    }

    /// The request ended: `status` is the refusal's HTTP status (`None` for success). A 4xx other than 404 means the
    /// server was not left, so the mark goes; a 404, a 5xx and status 0 (no answer) keep it, as the request may have
    /// landed.
    pub fn end(&mut self, c: CommunityId, status: Option<u16>) {
        self.pending.remove(&c);
        if status.is_some_and(|s| (400..500).contains(&s) && s != 404) {
            self.removed.remove(&c);
        }
    }

    /// The server is listed again with no leave or delete in flight: the mark is spent.
    pub fn listed(&mut self, c: CommunityId) {
        if !self.pending.contains(&c) {
            self.removed.remove(&c);
        }
    }

    /// The server is joined: the mark is spent, whatever is in flight.
    pub fn joined(&mut self, c: CommunityId) {
        self.removed.remove(&c);
        self.pending.remove(&c);
    }

    pub fn contains(&self, c: CommunityId) -> bool {
        self.removed.contains(&c)
    }
}

/// The notices for the servers `before` listed that `now` no longer does, minus the ones this account removed itself.
/// Each takes the next `seq`; the order is by server id, so one list read gives a stable order.
pub fn notices(
    before: &HashMap<CommunityId, String>,
    now: &HashSet<CommunityId>,
    mine: &SelfRemoved,
    seq: &mut u64,
) -> Vec<RemovalNotice> {
    let mut gone: Vec<(&CommunityId, &String)> = before
        .iter()
        .filter(|(c, _)| !now.contains(c) && !mine.contains(**c))
        .collect();
    gone.sort_by_key(|(c, _)| c.0);
    gone.into_iter()
        .map(|(c, name)| {
            *seq += 1;
            RemovalNotice {
                id: *seq,
                community: *c,
                name: name.clone(),
            }
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn id(n: u8) -> CommunityId {
        CommunityId([n; 16])
    }

    #[test]
    fn a_server_gone_from_the_list_is_told_unless_this_account_left_it() {
        let (a, b) = (id(1), id(2));
        let mut seq = 0;
        let before = HashMap::from([(a, "Midgard".to_string()), (b, "Asgard".to_string())]);
        let mut mine = SelfRemoved::default();
        mine.begin(b);
        let n = notices(&before, &HashSet::new(), &mine, &mut seq);
        assert_eq!(
            n,
            [RemovalNotice {
                id: 1,
                community: a,
                name: "Midgard".into()
            }]
        );
        mine.end(b, Some(409));
        assert!(!mine.contains(b));
        mine.begin(b);
        mine.end(b, Some(502));
        assert!(mine.contains(b));
        mine.end(b, Some(404));
        assert!(mine.contains(b));
        mine.end(b, None);
        mine.listed(b);
        assert!(!mine.contains(b));
    }

    #[test]
    fn a_listing_during_a_leave_keeps_the_mark_and_a_join_drops_it() {
        let b = id(2);
        let mut mine = SelfRemoved::default();
        mine.begin(b);
        mine.listed(b);
        assert!(mine.contains(b));
        mine.joined(b);
        assert!(!mine.contains(b));
    }

    #[test]
    fn a_notice_does_not_print_its_server_name() {
        let n = RemovalNotice {
            id: 3,
            community: id(1),
            name: "Secret Midgard".into(),
        };
        let shown = format!("{n:?}");
        assert!(!shown.contains("Midgard"), "{shown}");
        assert!(shown.contains("name_len: 14"), "{shown}");
    }
}
