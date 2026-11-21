//! A poll card's vote and close (PollCard.tsx `vote`, `submit` and `close`), through the Bot: a click on an answer sends
//! this member's new choices (operation 73) and Close poll closes it (74), each for the card's own poll, in the card's own
//! channel and thread (`Utilities::vote`/`close` refuse a record held for another place). While one runs the card is busy.
//! A refusal for a changed poll (status 2, 3 or 4) says so and lets go of the attempt; an outcome that is not known keeps
//! the attempt and its request id, so the next click on the same answer (or on Close poll) is the same request, and says
//! "Your vote may not have counted"; the poll is read again after anything but success. The line under the card is the
//! web's `<p role="alert">`.
//!
//! The state is per client and card (the client's serial, the post, the thread), as the web's attempt map is per client and
//! poll scope; a released client's go with it ([`release`]).

use std::collections::HashMap;
use std::rc::Rc;

use dilla_client::Client;
use dilla_client::features::utilities::UtilityError;
use dilla_client::state::bot::PollView;
use dilla_client::state::{MessageId, TimelineItem};
use gpui::{App, Window};

use crate::slots::SlotCx;
use crate::strings::t;

/// What is under way, or held after an uncertain outcome, for one card.
#[derive(Clone, Debug, PartialEq, Eq)]
enum Kind {
    /// A vote for these choices, made by a click on `answer`.
    Vote {
        choices: Vec<usize>,
        answer: usize,
    },
    Close,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct Attempt {
    kind: Kind,
    /// The request id: kept while the outcome is not known, so a retry is the same request.
    request: [u8; 16],
    /// The revision the attempt was first sent at: a retry sends it again, not the re-read one, so the Bot's request
    /// fingerprint matches and a vote that landed is answered as a replay (PollCard.tsx `immutable.revision`).
    revision: u64,
}

/// Whether a re-read `poll` already shows the held attempt's effect: a close's poll is closed, a vote's `my_choices` are
/// its choices. The web's `reconcile` outcome, without the op read.
fn settled(attempt: &Attempt, poll: &PollView) -> bool {
    match &attempt.kind {
        Kind::Close => poll.closed,
        Kind::Vote { choices, .. } => {
            let mut want = choices.clone();
            want.sort_unstable();
            let mut have = poll.my_choices.clone().unwrap_or_default();
            have.sort_unstable();
            want == have
        }
    }
}

/// The attempt a click on `answer` (or on Close poll, `answer` `None`) makes of `held`: a held one is retried as it was
/// (same request id, same revision); no attempt starts a new one at `poll`'s revision. `None` when another attempt is held.
fn attempt_for(
    held: &Option<Attempt>,
    poll: &PollView,
    mine: &[usize],
    answer: Option<usize>,
    new_request: impl FnOnce() -> [u8; 16],
) -> Option<Attempt> {
    match (held, answer) {
        (
            Some(
                h @ Attempt {
                    kind: Kind::Vote { answer: a, .. },
                    ..
                },
            ),
            Some(answer),
        ) if *a == answer => Some(h.clone()),
        (
            Some(
                h @ Attempt {
                    kind: Kind::Close, ..
                },
            ),
            None,
        ) => Some(h.clone()),
        (Some(_), _) => None,
        (None, Some(answer)) => Some(Attempt {
            kind: Kind::Vote {
                choices: choices_after(mine, answer, poll.multi),
                answer,
            },
            request: new_request(),
            revision: poll.revision,
        }),
        (None, None) => Some(Attempt {
            kind: Kind::Close,
            request: new_request(),
            revision: poll.revision,
        }),
    }
}

/// What a finished vote or close leaves of the card's state. `error` is the failure (`None` for success) and `latest` the
/// poll read again after it. Success, or a re-read that shows the held attempt's effect, lets go of everything with no
/// line. Returns whether the action took effect.
fn finish(
    a: &mut CardAction,
    error: Option<&UtilityError>,
    latest: Option<&PollView>,
    close: bool,
) -> bool {
    a.busy = false;
    let Some(e) = error else {
        *a = CardAction::default();
        return true;
    };
    if let (Some(held), Some(poll)) = (&a.attempt, latest)
        && settled(held, poll)
    {
        *a = CardAction::default();
        return true;
    }
    let (line, keep) = after_error(e, close);
    a.error = Some(line);
    if !keep {
        a.attempt = None;
        a.selected = None;
    }
    false
}

/// One card's action state (PollCard.tsx's `busy`, `attempt`, `selected` and `error`).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct CardAction {
    pub busy: bool,
    attempt: Option<Attempt>,
    /// The answers shown as this member's while a vote runs or is uncertain (`selected`); `None` shows the record's.
    selected: Option<Vec<usize>>,
    /// The line under the card (`error`), a strings key.
    pub error: Option<&'static str>,
}

impl CardAction {
    /// The answers drawn as this member's: the vote under way or held, else the record's.
    pub fn mine(&self, poll: &PollView) -> Vec<usize> {
        self.selected
            .clone()
            .or_else(|| poll.my_choices.clone())
            .unwrap_or_default()
    }

    /// The answer of a held vote (`pendingAnswer`): only it may be clicked again.
    pub fn pending_answer(&self) -> Option<usize> {
        match &self.attempt {
            Some(Attempt {
                kind: Kind::Vote { answer, .. },
                ..
            }) => Some(*answer),
            _ => None,
        }
    }

    /// `canVote`: no attempt, or a vote's.
    pub fn can_vote(&self) -> bool {
        !matches!(
            self.attempt,
            Some(Attempt {
                kind: Kind::Close,
                ..
            })
        )
    }
}

type Key = (u64, MessageId, Option<MessageId>);

#[derive(Default)]
struct Actions(HashMap<Key, CardAction>);
impl gpui::Global for Actions {}

/// The action state of the card of `message` in `thread` for `client`.
pub fn action_of(
    client: u64,
    message: MessageId,
    thread: Option<MessageId>,
    cx: &App,
) -> CardAction {
    cx.try_global::<Actions>()
        .and_then(|a| a.0.get(&(client, message, thread)).cloned())
        .unwrap_or_default()
}

fn update(key: Key, cx: &mut App, f: impl FnOnce(&mut CardAction)) {
    let actions = &mut cx.default_global::<Actions>().0;
    let action = actions.entry(key).or_default();
    f(action);
    if *action == CardAction::default() {
        actions.remove(&key);
    }
}

/// A released client's cards keep nothing.
pub fn release(client: &Rc<Client>, cx: &mut App) {
    if cx.has_global::<Actions>() {
        let serial = client.serial();
        cx.global_mut::<Actions>()
            .0
            .retain(|(c, _, _), _| *c != serial);
    }
}

/// PollCard.tsx `vote`: the choices a click on `answer` makes of `mine`: a pick toggles off, a single-answer poll takes
/// the new one alone, a multi-answer poll adds it.
pub fn choices_after(mine: &[usize], answer: usize, multi: bool) -> Vec<usize> {
    if mine.contains(&answer) {
        mine.iter().copied().filter(|&c| c != answer).collect()
    } else if multi {
        let mut next = mine.to_vec();
        next.push(answer);
        next
    } else {
        vec![answer]
    }
}

/// What a vote's or a close's error leaves: the line's strings key, and whether the attempt (its request id) is kept.
fn after_error(error: &UtilityError, close: bool) -> (&'static str, bool) {
    match error {
        // A definite refusal of a changed poll has no effect (`definiteMutationError`).
        UtilityError::Status(2..=4) => (
            if close {
                "poll.closeConflict"
            } else {
                "poll.conflict"
            },
            false,
        ),
        // No record held for the card's place: nothing was sent.
        UtilityError::NoPoll | UtilityError::Payload => ("poll.readError", false),
        // Anything else may have taken effect: the same request settles it.
        _ => (
            if close {
                "poll.uncertain"
            } else {
                "poll.voteUncertain"
            },
            true,
        ),
    }
}

/// The parts of a card a vote or a close needs: the client, its community and the post.
struct Card {
    client: Rc<Client>,
    community: dilla_client::state::CommunityId,
    item: TimelineItem,
    message: MessageId,
    thread: Option<MessageId>,
}

impl Card {
    fn of(ctx: &SlotCx, item: &TimelineItem, thread: Option<MessageId>) -> Option<Self> {
        Some(Self {
            client: ctx.client.clone()?,
            community: ctx.community?,
            item: item.clone(),
            message: item.msg_id?,
            thread,
        })
    }

    fn key(&self) -> Key {
        (self.client.serial(), self.message, self.thread)
    }
}

/// A click on answer `answer` of the card of `item` (in `thread`) holding `poll`.
pub fn vote(
    ctx: &SlotCx,
    item: &TimelineItem,
    thread: Option<MessageId>,
    poll: &PollView,
    answer: usize,
    cx: &mut App,
) {
    let Some(card) = Card::of(ctx, item, thread) else {
        return;
    };
    let key = card.key();
    let action = action_of(key.0, key.1, key.2, cx);
    if action.busy {
        return;
    }
    // A re-read that already shows a held attempt's effect lets go of it, even on a closed card.
    if action.attempt.as_ref().is_some_and(|h| settled(h, poll)) {
        update(key, cx, |a| *a = CardAction::default());
        cx.refresh_windows();
        return;
    }
    if poll.closed {
        return;
    }
    let Some(attempt) = attempt_for(
        &action.attempt,
        poll,
        &action.mine(poll),
        Some(answer),
        dilla_client::features::bot_admin::new_request_id,
    ) else {
        return;
    };
    let Kind::Vote { choices, .. } = attempt.kind.clone() else {
        return;
    };
    update(key, cx, |a| {
        a.busy = true;
        a.attempt = Some(attempt.clone());
        a.selected = Some(choices.clone());
        a.error = None;
    });
    let (community, channel, place) = (card.community, poll.channel, (poll.channel, thread));
    let (message, item) = (card.message, card.item.clone());
    let (request, revision) = (attempt.request, attempt.revision);
    let job = card.client.utilities().run(move |u| async move {
        let voted = u
            .vote_at(community, message, place, choices, request, Some(revision))
            .await;
        let latest = if voted.is_err() {
            u.read_poll(community, channel, thread, &item).await.ok()
        } else {
            None
        };
        Ok((voted, latest))
    });
    cx.spawn(async move |cx| {
        let (voted, latest) = match job.await {
            Ok(done) => done,
            Err(e) => (Err(e), None),
        };
        cx.update(|cx| {
            update(key, cx, |a| {
                finish(a, voted.as_ref().err(), latest.as_ref(), false);
            });
            cx.refresh_windows();
        });
    })
    .detach();
}

/// Close poll on the card of `item` (in `thread`) holding `poll`. A refusal because the poll changed meanwhile (status 4)
/// reads it again and, while it is still open, closes it once more under a new request (PollCard.tsx `close`).
pub fn close(
    ctx: &SlotCx,
    item: &TimelineItem,
    thread: Option<MessageId>,
    poll: &PollView,
    window: &mut Window,
    cx: &mut App,
) {
    let Some(card) = Card::of(ctx, item, thread) else {
        return;
    };
    let key = card.key();
    let action = action_of(key.0, key.1, key.2, cx);
    if action.busy {
        return;
    }
    // A closed poll lets go of a held close, and says it closed, as a settled close does.
    if action.attempt.as_ref().is_some_and(|h| settled(h, poll)) {
        update(key, cx, |a| *a = CardAction::default());
        let window_id = window.window_handle().window_id();
        dilla_desktop_ui::components::toast::show_toast_in(Some(window_id), t("poll.closed"), cx);
        cx.refresh_windows();
        return;
    }
    if poll.closed {
        return;
    }
    let Some(attempt) = attempt_for(
        &action.attempt,
        poll,
        &[],
        None,
        dilla_client::features::bot_admin::new_request_id,
    ) else {
        return;
    };
    update(key, cx, |a| {
        a.busy = true;
        a.attempt = Some(attempt.clone());
        a.error = None;
    });
    let (community, channel, place) = (card.community, poll.channel, (poll.channel, thread));
    let (message, item) = (card.message, card.item.clone());
    let (request, revision) = (attempt.request, attempt.revision);
    let job = card.client.utilities().run(move |u| async move {
        let closed = match u
            .close_at(community, message, place, request, Some(revision))
            .await
        {
            Err(UtilityError::Status(4)) => {
                match u.read_poll(community, channel, thread, &item).await {
                    Ok(latest) if latest.closed => Ok(latest),
                    Ok(_) => {
                        let fresh = dilla_client::features::bot_admin::new_request_id();
                        u.close(community, message, place, fresh).await
                    }
                    Err(_) => Err(UtilityError::NoPoll),
                }
            }
            other => other,
        };
        let latest = if closed.is_err() {
            u.read_poll(community, channel, thread, &item).await.ok()
        } else {
            None
        };
        Ok((closed, latest))
    });
    let window_id = window.window_handle().window_id();
    cx.spawn(async move |cx| {
        let (closed, latest) = match job.await {
            Ok(done) => done,
            Err(e) => (Err(e), None),
        };
        cx.update(|cx| {
            let mut took_effect = false;
            update(key, cx, |a| {
                took_effect = finish(a, closed.as_ref().err(), latest.as_ref(), true);
            });
            if took_effect {
                dilla_desktop_ui::components::toast::show_toast_in(
                    Some(window_id),
                    t("poll.closed"),
                    cx,
                );
            }
            cx.refresh_windows();
        });
    })
    .detach();
}

#[cfg(test)]
mod tests {
    use super::*;

    /// PollCard.tsx `vote`: a pick toggles off; one answer replaces the other in a single-answer poll; a multi-answer poll
    /// adds it.
    #[test]
    fn a_click_toggles_replaces_or_adds_as_the_web_does() {
        assert_eq!(choices_after(&[], 1, false), [1]);
        assert_eq!(choices_after(&[0], 1, false), [1]);
        assert_eq!(choices_after(&[1], 1, false), [] as [usize; 0]);
        assert_eq!(choices_after(&[0], 1, true), [0, 1]);
        assert_eq!(choices_after(&[0, 1], 0, true), [1]);
    }

    /// A changed poll's refusal lets go of the attempt and says so; an outcome that is not known keeps it (the retry is
    /// the same request) and says the vote may not have counted.
    #[test]
    fn a_refusal_lets_go_and_an_unknown_outcome_keeps_the_attempt() {
        assert_eq!(
            after_error(&UtilityError::Status(4), false),
            ("poll.conflict", false)
        );
        assert_eq!(
            after_error(&UtilityError::Status(3), true),
            ("poll.closeConflict", false)
        );
        assert_eq!(
            after_error(
                &UtilityError::Uncertain {
                    request_id: [1; 16]
                },
                false
            ),
            ("poll.voteUncertain", true)
        );
        assert_eq!(
            after_error(&UtilityError::Unavailable("E_BOT_TIMEOUT".into()), true),
            ("poll.uncertain", true)
        );
        assert_eq!(
            after_error(&UtilityError::NoPoll, false),
            ("poll.readError", false)
        );
    }

    /// `canVote` and `pendingAnswer`: a held vote leaves only its answer to click; a held close takes the votes away.
    #[test]
    fn a_held_attempt_limits_the_clicks() {
        let vote = CardAction {
            attempt: Some(Attempt {
                kind: Kind::Vote {
                    choices: vec![1],
                    answer: 1,
                },
                request: [1; 16],
                revision: 1,
            }),
            ..CardAction::default()
        };
        assert_eq!(vote.pending_answer(), Some(1));
        assert!(vote.can_vote());
        let close = CardAction {
            attempt: Some(Attempt {
                kind: Kind::Close,
                request: [1; 16],
                revision: 1,
            }),
            ..CardAction::default()
        };
        assert_eq!(close.pending_answer(), None);
        assert!(!close.can_vote());
        assert!(CardAction::default().can_vote());
    }

    fn record(revision: u64, closed: bool, mine: Option<Vec<usize>>) -> PollView {
        PollView {
            id: [1; 16],
            channel: dilla_client::state::ChannelId([2; 16]),
            thread: None,
            creator: dilla_client::state::UserId([3; 16]),
            question: String::new(),
            answers: vec![String::new(), String::new()],
            multi: false,
            created_at: 0,
            due_at: 0,
            closed,
            counts: vec![0, 0],
            my_choices: mine,
            revision,
            previews: vec![],
            distinct: 0,
            capped: false,
        }
    }

    /// PollCard.tsx `immutable.revision`: a retry of an uncertain vote (or close) is the attempt as it was, at the
    /// revision it first went at, though the re-read record has moved on; a new attempt takes the record's.
    #[test]
    fn a_retry_carries_the_attempts_original_revision() {
        let first = attempt_for(&None, &record(3, false, None), &[], Some(1), || [9; 16]).unwrap();
        assert_eq!((first.request, first.revision), ([9; 16], 3));
        let reread = record(5, false, None);
        let retry = attempt_for(&Some(first), &reread, &[1], Some(1), || [7; 16]).unwrap();
        assert_eq!((retry.request, retry.revision), ([9; 16], 3));
        let close = attempt_for(&None, &record(3, false, None), &[], None, || [8; 16]).unwrap();
        let again = attempt_for(&Some(close), &reread, &[], None, || [6; 16]).unwrap();
        assert_eq!((again.request, again.revision), ([8; 16], 3));
        // Another answer on a held close is no retry.
        let held = Some(again);
        assert_eq!(attempt_for(&held, &reread, &[], Some(0), || [5; 16]), None);
    }

    fn uncertain_vote() -> CardAction {
        let mut a = CardAction {
            busy: true,
            attempt: Some(Attempt {
                kind: Kind::Vote {
                    choices: vec![1],
                    answer: 1,
                },
                request: [1; 16],
                revision: 3,
            }),
            selected: Some(vec![1]),
            error: None,
        };
        let e = UtilityError::Unavailable("E_BOT_TIMEOUT".into());
        assert!(!finish(
            &mut a,
            Some(&e),
            Some(&record(3, false, None)),
            false
        ));
        a
    }

    /// A re-read that shows the vote applied lets go of the attempt: no line, no conflict, no held answer.
    #[test]
    fn a_reread_showing_the_vote_landed_settles_it() {
        let held = uncertain_vote();
        assert_eq!(held.error, Some("poll.voteUncertain"));
        assert_eq!(held.pending_answer(), Some(1));
        let mut a = held.clone();
        a.busy = true;
        let e = UtilityError::Unavailable("E_BOT_TIMEOUT".into());
        assert!(finish(
            &mut a,
            Some(&e),
            Some(&record(4, false, Some(vec![1]))),
            false
        ));
        assert_eq!(a, CardAction::default());
        assert!(settled(
            held.attempt.as_ref().unwrap(),
            &record(4, false, Some(vec![1]))
        ));
        assert!(!settled(
            held.attempt.as_ref().unwrap(),
            &record(4, false, None)
        ));
    }

    /// A re-read that shows the poll closed lets go of an uncertain close and reports it closed.
    #[test]
    fn a_reread_showing_the_poll_closed_settles_an_uncertain_close() {
        let mut a = CardAction {
            busy: true,
            attempt: Some(Attempt {
                kind: Kind::Close,
                request: [1; 16],
                revision: 3,
            }),
            ..CardAction::default()
        };
        let e = UtilityError::Unavailable("E_BOT_TIMEOUT".into());
        assert!(!finish(
            &mut a,
            Some(&e),
            Some(&record(3, false, None)),
            true
        ));
        assert_eq!(a.error, Some("poll.uncertain"));
        a.busy = true;
        assert!(finish(&mut a, Some(&e), Some(&record(4, true, None)), true));
        assert_eq!(a, CardAction::default());
    }
}
