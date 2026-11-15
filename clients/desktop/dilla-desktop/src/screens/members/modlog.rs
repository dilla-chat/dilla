//! The moderation log's pages (integration 3, step 5: Task 14's section on Task 21's `BotAdmin::moderation_page`), as
//! packages/web/src/screens/admin/ModerationLog.tsx keeps them: each page is a sealed Bot read (automatic cases, manual
//! evidence) the Bot answers directly. [`Log`] is the pure part; the section draws it.

use dilla_client::ClientError;
use dilla_client::state::CommunityId;
use dilla_client::state::bot_admin::{ModerationCase, ModerationPage};

/// ModerationLog.tsx `MAX_DISPLAY_CASES`.
pub const MAX_DISPLAY_CASES: usize = 100;
/// The v1 status the Bot answers when the reader may not view the audit log.
const FORBIDDEN: u8 = 2;

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Phase {
    #[default]
    Idle,
    Sending,
    Ready,
    Denied,
    Unavailable,
    Error,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Source {
    Automatic,
    #[default]
    Manual,
}

impl Source {
    /// The Bot operation that reads this source's page.
    pub fn operation(self) -> u64 {
        match self {
            Source::Automatic => dilla_client::state::bot_admin::MODERATION_CASES,
            Source::Manual => dilla_client::state::bot_admin::MODERATION_EVIDENCE,
        }
    }
}

/// One community's log as the section shows it.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Log {
    pub community: Option<CommunityId>,
    pub phase: Phase,
    pub source: Source,
    pub cases: Vec<ModerationCase>,
    /// The manual evidence's next page.
    pub cursor: Option<String>,
    pub automatic_more: bool,
    generation: u64,
}

impl Log {
    /// The section shows `community`: another community's log starts over.
    pub fn show(&mut self, community: CommunityId) {
        if self.community != Some(community) {
            *self = Log {
                community: Some(community),
                generation: self.generation + 1,
                ..Log::default()
            };
        }
    }

    /// A load of `source` (after `after`, manual only) starts: its generation, or `None` while one is out.
    pub fn begin(
        &mut self,
        source: Source,
        after: Option<String>,
    ) -> Option<(u64, Option<String>)> {
        if self.phase == Phase::Sending {
            return None;
        }
        // Automatic cases have no cursor: every automatic load is a first page.
        let after = if source == Source::Manual {
            after
        } else {
            None
        };
        self.generation += 1;
        self.source = source;
        if after.is_none() {
            self.cases.clear();
        }
        self.phase = Phase::Sending;
        Some((self.generation, after))
    }

    /// The answer of the load `generation` landed; a later load or another community makes it stale.
    pub fn land(&mut self, generation: u64, answer: Result<ModerationPage, ClientError>) {
        if generation != self.generation {
            return;
        }
        let page = match answer {
            Ok(page) => page,
            Err(_) => {
                self.phase = Phase::Error;
                return;
            }
        };
        if page.status == FORBIDDEN {
            self.cases.clear();
            self.phase = Phase::Denied;
            return;
        }
        if page.status != 0 {
            self.phase = Phase::Unavailable;
            return;
        }
        for row in page.cases {
            if self.cases.len() < MAX_DISPLAY_CASES && !self.cases.iter().any(|c| c.key == row.key)
            {
                self.cases.push(row);
            }
        }
        self.cursor = if self.source == Source::Manual {
            page.next_cursor.clone()
        } else {
            None
        };
        self.automatic_more = self.source == Source::Automatic && page.next_cursor.is_some();
        self.phase = Phase::Ready;
    }

    /// "Load more" is offered: manual evidence with a next page, under the display cap.
    pub fn more(&self) -> Option<&str> {
        (self.phase == Phase::Ready
            && self.source == Source::Manual
            && self.cases.len() < MAX_DISPLAY_CASES)
            .then_some(self.cursor.as_deref())
            .flatten()
    }

    /// The ready answer had nothing to show and nothing more to read.
    pub fn empty(&self) -> bool {
        self.phase == Phase::Ready
            && self.cases.is_empty()
            && match self.source {
                Source::Manual => self.cursor.is_none(),
                Source::Automatic => !self.automatic_more,
            }
    }
}

/// One window's log, and the client (by the address of its `Rc<Client>`) whose sealed reads filled it (final review I4:
/// per window, and emptied when that client is released).
#[derive(Default)]
struct WindowLog {
    log: Log,
    reader: usize,
}

/// A copy of `window`'s log, as the section draws it.
pub fn log_of(window: gpui::WindowId, cx: &gpui::App) -> Log {
    crate::per_window::get::<WindowLog>(window, cx)
        .map(|w| w.log.clone())
        .unwrap_or_default()
}

/// Changes `window`'s log.
pub fn with_log<R>(window: gpui::WindowId, cx: &mut gpui::App, f: impl FnOnce(&mut Log) -> R) -> R {
    f(&mut crate::per_window::get_mut::<WindowLog>(window, cx).log)
}

/// The client whose reads fill `window`'s log now.
pub fn set_reader(window: gpui::WindowId, reader: usize, cx: &mut gpui::App) {
    crate::per_window::get_mut::<WindowLog>(window, cx).reader = reader;
}

/// The client `reader` is released (Forget, sign-out): every log it filled goes, decrypted excerpts and all.
pub fn release_reader(reader: usize, cx: &mut gpui::App) {
    crate::per_window::retain::<WindowLog>(cx, |_, w| w.reader != reader);
}

#[cfg(test)]
mod tests {
    use super::*;

    fn case(key: &str) -> ModerationCase {
        ModerationCase {
            key: key.to_owned(),
            channel_id: String::new(),
            actor_id: String::new(),
            message_id: String::new(),
            revision: String::new(),
            reason: "spam".into(),
            action: "delete".into(),
            excerpt: "buy now".into(),
            received_at: "1794481738".into(),
        }
    }

    fn page(status: u8, keys: &[&str], next: Option<&str>) -> ModerationPage {
        ModerationPage {
            status,
            cases: keys.iter().map(|k| case(k)).collect(),
            next_cursor: next.map(str::to_owned),
        }
    }

    /// Integration 3, final review I4: each window keeps its own log, and a release of the client that read it empties
    /// it (decrypted excerpts never outlive Forget or a sign-out); another window's log, of another client, stays.
    #[gpui::test]
    fn each_window_keeps_its_own_log_and_release_empties_it(cx: &mut gpui::TestAppContext) {
        let a = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let b = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        cx.update(|cx| {
            for (w, community, key, client) in [(a, 7u8, "a", 1usize), (b, 8, "b", 2)] {
                with_log(w, cx, |log| {
                    log.show(CommunityId([community; 16]));
                    let (g, _) = log.begin(Source::Manual, None).unwrap();
                    log.land(g, Ok(page(0, &[key], None)));
                });
                set_reader(w, client, cx);
            }
            assert_eq!(log_of(a, cx).cases[0].key, "a");
            assert_eq!(
                log_of(b, cx).cases[0].key,
                "b",
                "two windows never share a log"
            );
            release_reader(1, cx);
            let gone = log_of(a, cx);
            assert!(
                gone.cases.is_empty() && gone.phase == Phase::Idle,
                "released: empty"
            );
            assert_eq!(
                log_of(b, cx).cases[0].key,
                "b",
                "the other client's log stays"
            );
        });
    }

    /// ModerationLog.tsx: one load at a time; manual pages append (deduplicated, capped at 100) and offer "Load more"
    /// with the next cursor; an automatic page only says more exist; status 2 is the Bot's refusal, any other status
    /// "unavailable", a transport error "error"; a stale answer and another community's start over change nothing.
    #[test]
    fn the_log_reads_pages_as_the_web_does() {
        let mut log = Log::default();
        log.show(CommunityId([7; 16]));
        let (g, after) = log.begin(Source::Manual, None).unwrap();
        assert_eq!(after, None);
        assert!(
            log.begin(Source::Automatic, None).is_none(),
            "one load at a time"
        );
        log.land(g, Ok(page(0, &["a", "b"], Some("0a0b"))));
        assert_eq!(log.phase, Phase::Ready);
        assert_eq!(log.more(), Some("0a0b"));
        let (g, after) = log.begin(Source::Manual, Some("0a0b".into())).unwrap();
        assert_eq!(after.as_deref(), Some("0a0b"));
        log.land(g, Ok(page(0, &["b", "c"], None)));
        let keys: Vec<&str> = log.cases.iter().map(|c| c.key.as_str()).collect();
        assert_eq!(keys, ["a", "b", "c"], "appended without the repeat");
        assert_eq!(log.more(), None);
        // Automatic cases replace the list and only say more exist.
        let (g, after) = log.begin(Source::Automatic, Some("ff".into())).unwrap();
        assert_eq!(after, None, "automatic cases have no cursor");
        log.land(g, Ok(page(0, &[], Some("ff"))));
        assert!(log.cases.is_empty() && log.automatic_more && !log.empty());
        // Refusals.
        let (g, _) = log.begin(Source::Manual, None).unwrap();
        log.land(g, Ok(page(2, &["x"], None)));
        assert_eq!(log.phase, Phase::Denied);
        let (g, _) = log.begin(Source::Manual, None).unwrap();
        log.land(g, Ok(page(5, &[], None)));
        assert_eq!(log.phase, Phase::Unavailable);
        let (g, _) = log.begin(Source::Manual, None).unwrap();
        log.land(g, Err(ClientError::Sync("E_NETWORK".into())));
        assert_eq!(log.phase, Phase::Error);
        // A stale answer, and another community.
        let (g, _) = log.begin(Source::Manual, None).unwrap();
        log.show(CommunityId([8; 16]));
        log.land(g, Ok(page(0, &["late"], None)));
        assert_eq!(log.phase, Phase::Idle);
        assert!(log.cases.is_empty());
        // The cap.
        let (g, _) = log.begin(Source::Manual, None).unwrap();
        let many: Vec<String> = (0..120).map(|n| format!("k{n}")).collect();
        let keys: Vec<&str> = many.iter().map(String::as_str).collect();
        log.land(g, Ok(page(0, &keys, Some("aa"))));
        assert_eq!(log.cases.len(), MAX_DISPLAY_CASES);
        assert_eq!(log.more(), None, "no more past the cap");
    }
}
