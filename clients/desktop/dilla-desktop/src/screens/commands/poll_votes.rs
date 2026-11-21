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
    if poll.closed || action.busy {
        return;
    }
    let attempt = match &action.attempt {
        // A held vote is retried by its own answer only, with its request id.
        Some(
            held @ Attempt {
                kind: Kind::Vote { answer: a, .. },
                ..
            },
        ) if *a == answer => held.clone(),
        Some(_) => return,
        None => Attempt {
            kind: Kind::Vote {
                choices: choices_after(&action.mine(poll), answer, poll.multi),
                answer,
            },
            request: dilla_client::features::bot_admin::new_request_id(),
        },
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
    let request = attempt.request;
    let job = card.client.utilities().run(move |u| async move {
        let voted = u.vote(community, message, place, choices, request).await;
        if voted.is_err() {
            let _ = u.read_poll(community, channel, thread, &item).await;
        }
        Ok(voted)
    });
    cx.spawn(async move |cx| {
        let answer = job.await.and_then(|r| r);
        cx.update(|cx| {
            update(key, cx, |a| {
                a.busy = false;
                match &answer {
                    Ok(_) => *a = CardAction::default(),
                    Err(e) => {
                        let (line, keep) = after_error(e, false);
                        a.error = Some(line);
                        if !keep {
                            a.attempt = None;
                            a.selected = None;
                        }
                    }
                }
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
    if poll.closed || action.busy {
        return;
    }
    let attempt = match &action.attempt {
        Some(
            held @ Attempt {
                kind: Kind::Close, ..
            },
        ) => held.clone(),
        Some(_) => return,
        None => Attempt {
            kind: Kind::Close,
            request: dilla_client::features::bot_admin::new_request_id(),
        },
    };
    update(key, cx, |a| {
        a.busy = true;
        a.attempt = Some(attempt.clone());
        a.error = None;
    });
    let (community, channel, place) = (card.community, poll.channel, (poll.channel, thread));
    let (message, item) = (card.message, card.item.clone());
    let request = attempt.request;
    let job = card.client.utilities().run(move |u| async move {
        let closed = match u.close(community, message, place, request).await {
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
        if closed.is_err() {
            let _ = u.read_poll(community, channel, thread, &item).await;
        }
        Ok(closed)
    });
    let window_id = window.window_handle().window_id();
    cx.spawn(async move |cx| {
        let answer = job.await.and_then(|r| r);
        cx.update(|cx| {
            update(key, cx, |a| {
                a.busy = false;
                match &answer {
                    Ok(_) => *a = CardAction::default(),
                    Err(e) => {
                        let (line, keep) = after_error(e, true);
                        a.error = Some(line);
                        if !keep {
                            a.attempt = None;
                        }
                    }
                }
            });
            if answer.is_ok() {
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
            }),
            ..CardAction::default()
        };
        assert_eq!(vote.pending_answer(), Some(1));
        assert!(vote.can_vote());
        let close = CardAction {
            attempt: Some(Attempt {
                kind: Kind::Close,
                request: [1; 16],
            }),
            ..CardAction::default()
        };
        assert_eq!(close.pending_answer(), None);
        assert!(!close.can_vote());
        assert!(CardAction::default().can_vote());
    }
}
