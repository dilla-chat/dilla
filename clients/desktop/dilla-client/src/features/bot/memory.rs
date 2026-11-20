//! The command memory (Task 30; W/screens/commands/CommandHost.tsx:24-51, :243-246, :285-298): per conversation, the
//! last command draft this client ran and its request id, and the attempt stores of the commands that write (builtin.ts
//! `uncertainPublications` and `timeoutAttempts`, UtilityCommandProvider.tsx's create attempts, bot-handlers.ts's
//! announcement and role panel attempts).
//!
//! A run takes the remembered id when it is the same invocation as the row's, else a new one ([`CommandMemory::attempt`]).
//! A definitive answer (success or failure) retires the id ([`CommandMemory::settled`]); an uncertain one keeps it and
//! the draft, to be reopened in the composer ([`CommandMemory::uncertain`], [`CommandMemory::take_reopen`]), so the
//! resend is the same request and the handler reconciles it instead of acting twice.
//!
//! One memory per client (the web's `memoryFor(client, selfId, accountScope)`): it lives beside the pins in
//! [`super::sealed::Sealed`] and goes with them at sign-out. A draft is a message body or a command with its member and
//! reason, so every type here gives lengths only in Debug (R20, R21).

use std::collections::{HashMap, VecDeque};

use crate::features::bot_admin::BotArg;
use crate::features::role_panels::PanelOption;
use crate::features::roles::request_id;
use crate::features::utilities::polls::PollCreate;
use crate::state::bot::Conversation;
use crate::state::{ChannelId, CommunityId, MessageId, Redacted, UserId, hex};

/// The most rows the memory and each attempt store keep (CommandHost.tsx:28; builtin.ts:57, :232; bot-handlers.ts:124).
pub const LIMIT: usize = 128;

/// One conversation's remembered draft (CommandHost.tsx `SavedDraft`). `reopen` is the web's `mode: 'active'` after an
/// uncertain answer: the composer shows `text` again the next time it is drawn for that conversation.
#[derive(Clone, PartialEq, Eq)]
pub struct Saved {
    pub text: String,
    pub request: [u8; 16],
    pub invocation: String,
    pub reopen: bool,
}

impl std::fmt::Debug for Saved {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Saved")
            .field("text", &Redacted(&self.text))
            .field("request", &self.request)
            .field("invocation", &Redacted(&self.invocation))
            .field("reopen", &self.reopen)
            .finish()
    }
}

/// The memory of one client: the rows, newest last, and the attempt stores.
#[derive(Default)]
pub struct CommandMemory {
    rows: VecDeque<(Conversation, Saved)>,
    pub publications: Publications,
    pub timeouts: TimeoutAttempts,
    pub polls: HashMap<[u8; 16], PollAttempt>,
    pub announcements: HashMap<([u8; 16], CommunityId), AnnouncementAttempt>,
    pub role_panels: HashMap<([u8; 16], CommunityId), RolePanelAttempt>,
    /// The insertion order of `polls`, `announcements` and `role_panels` (the web's `Map` order), to evict the oldest.
    poll_order: VecDeque<[u8; 16]>,
    announcement_order: VecDeque<([u8; 16], CommunityId)>,
    role_panel_order: VecDeque<([u8; 16], CommunityId)>,
}

impl std::fmt::Debug for CommandMemory {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CommandMemory")
            .field("rows", &self.rows.len())
            .field("publications", &self.publications.0.len())
            .field("timeouts", &self.timeouts.0.len())
            .field("polls", &self.polls.len())
            .field("announcements", &self.announcements.len())
            .field("role_panels", &self.role_panels.len())
            .finish()
    }
}

impl CommandMemory {
    fn row(&self, at: Conversation) -> Option<&Saved> {
        self.rows.iter().find(|(c, _)| *c == at).map(|(_, s)| s)
    }

    /// `remember` (:48-51): the row of `at` becomes `saved` in the newest place; past [`LIMIT`] the oldest goes.
    fn remember(&mut self, at: Conversation, saved: Saved) {
        self.rows.retain(|(c, _)| *c != at);
        self.rows.push_back((at, saved));
        while self.rows.len() > LIMIT {
            self.rows.pop_front();
        }
    }

    /// A run of `invocation` (its [`super::invocation_key`]) typed as `text` in `at`: the remembered request id when the
    /// row is the same invocation, else a new one (:243-246). The draft leaves the composer while it runs
    /// (`leaveAfterRun`, :198-205), so the row is not to be reopened.
    pub fn attempt(&mut self, at: Conversation, invocation: &str, text: &str) -> [u8; 16] {
        let request = self
            .row(at)
            .filter(|s| !s.invocation.is_empty() && s.invocation == invocation)
            .map_or_else(request_id, |s| s.request);
        self.remember(
            at,
            Saved {
                text: text.to_owned(),
                request,
                invocation: invocation.to_owned(),
                reopen: false,
            },
        );
        request
    }

    /// A definitive answer to the run of `request` (:285-288): when it is still the row's, the row takes a new id and no
    /// invocation, so the next run is a new request.
    pub fn settled(&mut self, at: Conversation, request: [u8; 16]) {
        let Some(saved) = self.row(at).filter(|s| s.request == request).cloned() else {
            return;
        };
        self.remember(
            at,
            Saved {
                request: request_id(),
                invocation: String::new(),
                reopen: false,
                ..saved
            },
        );
    }

    /// An uncertain answer (:289-292): the row keeps the request id and the invocation, and its draft is to be reopened.
    pub fn uncertain(&mut self, at: Conversation, text: &str, request: [u8; 16], invocation: &str) {
        self.remember(
            at,
            Saved {
                text: text.to_owned(),
                request,
                invocation: invocation.to_owned(),
                reopen: true,
            },
        );
    }

    /// The draft to reopen in `at`'s composer, once (:123-129); the id and the invocation stay.
    pub fn take_reopen(&mut self, at: Conversation) -> Option<String> {
        let saved = self
            .rows
            .iter_mut()
            .find(|(c, _)| *c == at)
            .map(|(_, s)| s)?;
        if !saved.reopen {
            return None;
        }
        saved.reopen = false;
        Some(saved.text.clone())
    }

    /// Keeps the poll create attempt of `request` (the oldest goes past [`LIMIT`]).
    pub fn keep_poll(&mut self, request: [u8; 16], attempt: PollAttempt) {
        if self.polls.insert(request, attempt).is_none() {
            self.poll_order.push_back(request);
        }
        evict(&mut self.polls, &mut self.poll_order);
    }

    /// Forgets the poll create attempt of `request`.
    pub fn drop_poll(&mut self, request: [u8; 16]) {
        self.polls.remove(&request);
        self.poll_order.retain(|r| *r != request);
    }

    /// Keeps the announcement attempt of the command `parent` in `community` (the oldest goes past [`LIMIT`]).
    pub fn keep_announcement(
        &mut self,
        key: ([u8; 16], CommunityId),
        attempt: AnnouncementAttempt,
    ) {
        if self.announcements.insert(key, attempt).is_none() {
            self.announcement_order.push_back(key);
        }
        evict(&mut self.announcements, &mut self.announcement_order);
    }

    /// Forgets the announcement attempt of `key`.
    pub fn drop_announcement(&mut self, key: ([u8; 16], CommunityId)) {
        self.announcements.remove(&key);
        self.announcement_order.retain(|k| *k != key);
    }

    /// Keeps the role panel attempt of `key` (the oldest goes past [`LIMIT`]).
    pub fn keep_role_panel(&mut self, key: ([u8; 16], CommunityId), attempt: RolePanelAttempt) {
        if self.role_panels.insert(key, attempt).is_none() {
            self.role_panel_order.push_back(key);
        }
        evict(&mut self.role_panels, &mut self.role_panel_order);
    }

    /// Forgets the role panel attempt of `key`.
    pub fn drop_role_panel(&mut self, key: ([u8; 16], CommunityId)) {
        self.role_panels.remove(&key);
        self.role_panel_order.retain(|k| *k != key);
    }
}

/// Drops `map`'s oldest entries (by `order`) past [`LIMIT`].
fn evict<K: std::hash::Hash + Eq + Copy, V>(map: &mut HashMap<K, V>, order: &mut VecDeque<K>) {
    order.retain(|k| map.contains_key(k));
    while map.len() > LIMIT {
        let Some(oldest) = order.pop_front() else {
            return;
        };
        map.remove(&oldest);
    }
}

/// builtin.ts `uncertainPublications` (:39-58): the sends whose answer was uncertain, by [`Publications::key`]. A run that
/// finds its key here sends nothing again.
#[derive(Default)]
pub struct Publications(VecDeque<String>);

impl Publications {
    /// builtin.ts:41: the request id, the channel, the thread, the message replied to and the text, joined by `\0` (the
    /// ids in hex, `""` for none).
    pub fn key(
        request: [u8; 16],
        channel: ChannelId,
        thread: Option<MessageId>,
        reply_to: Option<MessageId>,
        text: &str,
    ) -> String {
        let id = |m: Option<MessageId>| m.map(|m| m.hex()).unwrap_or_default();
        [
            hex(&request),
            channel.hex(),
            id(thread),
            id(reply_to),
            text.to_owned(),
        ]
        .join("\0")
    }

    pub fn contains(&self, key: &str) -> bool {
        self.0.iter().any(|k| k == key)
    }

    /// Remembers `key` as the newest; past [`LIMIT`] the oldest goes.
    pub fn insert(&mut self, key: String) {
        self.0.retain(|k| *k != key);
        self.0.push_back(key);
        while self.0.len() > LIMIT {
            self.0.pop_front();
        }
    }
}

/// A `/timeout` that was sent (builtin.ts:229-231): its member, the expiry it asked for, its length, and whether its
/// answer was confirmed. The timeout endpoint does not deduplicate request ids, so a sent one is never sent again.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TimeoutAttempt {
    pub community: CommunityId,
    pub user: UserId,
    pub expires: u64,
    pub seconds: u64,
    pub settled: bool,
}

/// builtin.ts `timeoutAttempts`, by request id.
#[derive(Default)]
pub struct TimeoutAttempts(HashMap<[u8; 16], TimeoutAttempt>);

impl TimeoutAttempts {
    /// The attempt `request` already belongs to, if any (builtin.ts:221-224): another member, community or length is an
    /// error.
    pub fn check(
        &self,
        request: [u8; 16],
        community: CommunityId,
        user: UserId,
        seconds: u64,
    ) -> Result<Option<&TimeoutAttempt>, &'static str> {
        match self.0.get(&request) {
            Some(a) if a.community != community || a.user != user || a.seconds != seconds => {
                Err("This timeout request ID already belongs to a different timeout.")
            }
            held => Ok(held),
        }
    }

    /// Keeps `attempt` as sent under `request` (:226-230). At [`LIMIT`] the settled ones go first; still full, nothing is
    /// sent.
    pub fn begin(
        &mut self,
        request: [u8; 16],
        attempt: TimeoutAttempt,
    ) -> Result<(), &'static str> {
        if self.0.len() >= LIMIT && !self.0.contains_key(&request) {
            self.0.retain(|_, a| !a.settled);
            if self.0.len() >= LIMIT {
                return Err("Reconcile a pending timeout before starting another timeout command.");
            }
        }
        self.0.insert(request, attempt);
        Ok(())
    }

    /// The answer to `request` was confirmed: the attempt is settled (and may go when the store is full).
    pub fn settle(&mut self, request: [u8; 16]) {
        if let Some(a) = self.0.get_mut(&request) {
            a.settled = true;
        }
    }

    /// A definitive refusal: the attempt goes, and a new run may send.
    pub fn remove(&mut self, request: [u8; 16]) {
        self.0.remove(&request);
    }
}

/// A poll create that went out (UtilityCommandProvider.tsx:385): where it was made, the Bot user that stands for the
/// web's `installationId` (the GPUI view holds no installation id, so the pinned Bot user of the community's catalog
/// stands for it), and the poll as sent. Its Debug gives no text ([`PollCreate`]'s).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PollAttempt {
    pub at: Conversation,
    pub community: CommunityId,
    pub bot_user: UserId,
    pub value: PollCreate,
}

/// A schedule mutation (bot-handlers.ts:78 `PendingAnnouncementMutation`): the operation (51 save, 52 delete), its own
/// request id, the arguments and list revision it was sent with, and whether it went out. [`BotArg`]'s Debug gives
/// lengths only.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AnnouncementAttempt {
    pub operation: u64,
    pub request: [u8; 16],
    pub args: Vec<BotArg>,
    pub revision: u64,
    pub sent: bool,
}

/// A role panel publication (bot-handlers.ts:466-540): its channel and roles, and whether it went out.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RolePanelAttempt {
    pub channel: ChannelId,
    pub options: Vec<PanelOption>,
    pub sent: bool,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn at(n: u8) -> Conversation {
        Conversation {
            channel: ChannelId([n; 16]),
            thread: None,
        }
    }

    /// CommandHost.tsx:243-246, :285-298: the same invocation keeps its id until a definitive answer retires it; an
    /// uncertain answer keeps it and reopens the draft once; another invocation is a new id; past 128 conversations the
    /// oldest row is gone.
    #[test]
    fn an_uncertain_attempt_keeps_its_request_and_reopens() {
        const K: &str = "timeout\0duration=10m\0user=a";
        const T: &str = "/timeout user:a duration:10m";
        let mut m = CommandMemory::default();
        let here = at(1);
        let r1 = m.attempt(here, K, T);
        assert_eq!(m.attempt(here, K, T), r1, "the same invocation");
        assert_eq!(m.take_reopen(here), None, "a running draft is not reopened");
        m.settled(here, r1);
        let r2 = m.attempt(here, K, T);
        assert_ne!(r2, r1, "a definitive answer retires the id");
        m.uncertain(here, T, r2, K);
        assert_eq!(m.attempt(here, K, T), r2, "an uncertain answer keeps it");
        m.uncertain(here, T, r2, K);
        assert_eq!(m.take_reopen(here), Some(T.into()));
        assert_eq!(m.take_reopen(here), None, "once");
        assert_eq!(m.attempt(here, K, T), r2, "the id stays after the reopen");
        assert_ne!(
            m.attempt(
                here,
                "timeout\0duration=10m\0user=b",
                "/timeout user:b duration:10m"
            ),
            r2,
            "another invocation"
        );
        // A settle of a request that is no longer the row's leaves the row alone.
        let r3 = m.attempt(here, K, T);
        m.settled(here, r2);
        assert_eq!(m.attempt(here, K, T), r3);
        m.uncertain(here, T, r3, K);
        for n in 2..=129 {
            m.attempt(at(n), K, T);
        }
        assert_eq!(m.take_reopen(here), None, "the oldest row is gone");
        assert_ne!(m.attempt(here, K, T), r3);
    }

    /// builtin.ts:221-235: a request id is bound to its member, community and length; a full store drops the settled ones
    /// first, and refuses while all are pending.
    #[test]
    fn timeout_attempts_bind_their_request() {
        let (c, bea, bob) = (CommunityId([7; 16]), UserId([2; 16]), UserId([3; 16]));
        let attempt = |user| TimeoutAttempt {
            community: c,
            user,
            expires: 1000,
            seconds: 600,
            settled: false,
        };
        let mut t = TimeoutAttempts::default();
        let r = [1; 16];
        assert_eq!(t.check(r, c, bea, 600), Ok(None));
        assert_eq!(t.begin(r, attempt(bea)), Ok(()));
        assert_eq!(
            t.check(r, c, bob, 600),
            Err("This timeout request ID already belongs to a different timeout.")
        );
        assert_eq!(
            t.check(r, c, bea, 60),
            Err("This timeout request ID already belongs to a different timeout.")
        );
        assert_eq!(t.check(r, c, bea, 600), Ok(Some(&attempt(bea))));
        let mut full = TimeoutAttempts::default();
        for n in 0..LIMIT as u8 {
            full.begin([n; 16], attempt(bea)).unwrap();
        }
        assert_eq!(
            full.begin([200; 16], attempt(bea)),
            Err("Reconcile a pending timeout before starting another timeout command.")
        );
        full.settle([5; 16]);
        assert_eq!(full.begin([200; 16], attempt(bea)), Ok(()));
        assert_eq!(
            full.check([5; 16], c, bea, 600),
            Ok(None),
            "the settled one went"
        );
        full.remove([200; 16]);
        assert_eq!(full.check([200; 16], c, bea, 600), Ok(None));
    }

    /// builtin.ts:41, :55-56: an uncertain send is remembered by its request, conversation, target and text; the 129th
    /// pushes the first out.
    #[test]
    fn a_publication_is_remembered_per_attempt() {
        let (ch, msg) = (ChannelId([4; 16]), MessageId([5; 16]));
        let key = |r: u8| Publications::key([r; 16], ch, None, Some(msg), "x");
        assert_eq!(
            key(1),
            format!("{}\0{}\0\0{}\0x", hex(&[1; 16]), ch.hex(), msg.hex())
        );
        let mut p = Publications::default();
        p.insert(key(1));
        assert!(p.contains(&key(1)));
        assert!(!p.contains(&key(2)));
        for r in 2..=129u8 {
            p.insert(key(r));
        }
        assert!(!p.contains(&key(1)), "the oldest is gone");
        assert!(p.contains(&key(129)));
    }

    /// Each attempt store keeps at most 128, the oldest going first.
    #[test]
    fn the_attempt_stores_evict_their_oldest() {
        let mut m = CommandMemory::default();
        let c = CommunityId([1; 16]);
        let attempt = AnnouncementAttempt {
            operation: 51,
            request: [0; 16],
            args: Vec::new(),
            revision: 1,
            sent: true,
        };
        for n in 0..=LIMIT as u8 {
            m.keep_announcement(([n; 16], c), attempt.clone());
        }
        assert_eq!(m.announcements.len(), LIMIT);
        assert!(!m.announcements.contains_key(&([0; 16], c)));
        m.drop_announcement(([1; 16], c));
        assert!(!m.announcements.contains_key(&([1; 16], c)));
    }

    /// R20/R21: a remembered draft, its invocation and the attempts' arguments never show in Debug.
    #[test]
    fn the_memory_keeps_drafts_out_of_debug() {
        let mut m = CommandMemory::default();
        m.uncertain(
            at(1),
            "/warn user:bea reason:secret",
            [1; 16],
            "warn\0reason=secret",
        );
        let saved = m.row(at(1)).unwrap().clone();
        let attempt = AnnouncementAttempt {
            operation: 51,
            request: [1; 16],
            args: vec![BotArg::Text("secret".into())],
            revision: 1,
            sent: true,
        };
        let poll = PollAttempt {
            at: at(1),
            community: CommunityId([1; 16]),
            bot_user: UserId([2; 16]),
            value: PollCreate {
                channel: [1; 16],
                thread: None,
                question: "secret".into(),
                answers: vec!["secret".into()],
                multi: false,
                duration: 3600,
            },
        };
        for shown in [
            format!("{m:?}"),
            format!("{saved:?}"),
            format!("{attempt:?}"),
            format!("{poll:?}"),
        ] {
            assert!(!shown.contains("secret"), "{shown}");
        }
    }
}
