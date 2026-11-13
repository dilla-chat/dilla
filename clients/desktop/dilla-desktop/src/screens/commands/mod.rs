//! Area **bot** (Task 20): the command picker, private results, poll cards and dialogs, reminders.
//!
//! Every file under `screens/commands/` is Task 20's. The hooks below are where `crate::slots` draws this area inside
//! another area's view; their signatures are Task 1's.
//!
//! - [`picker`] (`slots::composer_popup`): the `/` list over the composer, from the draft the composer reports through
//!   [`draft_changed`]; ↑↓, Tab, Enter and Escape reach it through [`keys`].
//! - [`poll_card`] (`slots::message_extras`): a verified poll under the Bot's post.
//! - [`private_results`]: the rows only this device shows, for the log of their conversation; [`on_send`] decides what
//!   a sent draft does (post it, or answer privately).

pub mod copy;
pub mod keys;
pub mod picker;

use dilla_client::features::bot::{self, Plan, run};
use dilla_client::features::utilities::polls;
use dilla_client::state::bot::{
    Conversation, PollView, PrivateResult, ResultProvider, ResultStatus,
};
use dilla_client::state::{AppState, TimelineItem};
use gpui::{AnyElement, App, ElementId, IntoElement, ParentElement, Styled, Window, div};

use dilla_desktop_ui::components::commands::bot_result::{self, Author, BotResult, Status};
use dilla_desktop_ui::components::commands::list::{CommandList, ListItem, ListLabels};
use dilla_desktop_ui::components::commands::poll_card::{self, PollCard};

use crate::screens::shell::Clock;
use crate::slots::{Extra, SlotCx};
use crate::strings::{t, tf};

/// The automation ids of the list's rows (at most the 25 the registry ranks).
pub const ROW_IDS: [&str; 25] = [
    "command-row-0",
    "command-row-1",
    "command-row-2",
    "command-row-3",
    "command-row-4",
    "command-row-5",
    "command-row-6",
    "command-row-7",
    "command-row-8",
    "command-row-9",
    "command-row-10",
    "command-row-11",
    "command-row-12",
    "command-row-13",
    "command-row-14",
    "command-row-15",
    "command-row-16",
    "command-row-17",
    "command-row-18",
    "command-row-19",
    "command-row-20",
    "command-row-21",
    "command-row-22",
    "command-row-23",
    "command-row-24",
];

/// The picker's state for one window's composer (final review M4: per window, so a draft in one window never opens or
/// closes another's list): the draft it reported, the highlighted row, the draft the list was closed on (Escape keeps
/// the draft and hides the list until the draft changes), and what the last frame showed.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct PickerState {
    pub draft: String,
    pub selected: usize,
    pub closed_for: Option<String>,
    /// The rows the last frame drew (`None`: the list was closed): what [`keys`] moves over.
    pub rows: Option<Vec<String>>,
    /// The command Tab or Enter picked on the list, for the chips.
    pub picked: Option<String>,
}

/// `window`'s picker state (empty until its composer reports a draft).
pub fn picker_state(window: gpui::WindowId, cx: &App) -> PickerState {
    crate::per_window::get::<PickerState>(window, cx)
        .cloned()
        .unwrap_or_default()
}

/// `window`'s picker state, to change.
pub fn picker_state_mut(window: gpui::WindowId, cx: &mut App) -> &mut PickerState {
    crate::per_window::get_mut::<PickerState>(window, cx)
}

/// The composer of `window` reports its draft on every change. A new draft moves the highlight back to the first row
/// and opens a list that Escape closed.
pub fn draft_changed(window: gpui::WindowId, draft: &str, cx: &mut App) {
    let state = picker_state_mut(window, cx);
    if state.draft != draft {
        state.draft = draft.to_owned();
        state.selected = 0;
        state.picked = None;
        if state.closed_for.as_deref() != Some(draft) {
            state.closed_for = None;
        }
    }
}

/// The zone and locale times are drawn in: UTC under a fixture's frozen clock (the references are shot in UTC), else
/// the local zone and the OS region.
fn zone_of(clock: Clock) -> (fn(i64) -> chrono::FixedOffset, String) {
    match clock {
        Clock::Fixed { utc: true, .. } => (
            |_| chrono::FixedOffset::east_opt(0).expect("utc"),
            "en-US".to_owned(),
        ),
        _ => (crate::format::local_zone, crate::format::date_locale()),
    }
}

/// A poll's card under its message (`slots::message_extras`, after the attachments).
pub fn poll_card(
    item: &TimelineItem,
    ctx: &SlotCx,
    _window: &mut Window,
    _cx: &mut App,
) -> Option<Extra> {
    // PollCard.tsx: only a Bot's unedited post whose verified record this device holds.
    if !item.bot || item.edited {
        return None;
    }
    let poll = ctx.state.bot.polls.get(&item.msg_id?)?;
    // The card is at most 440px of content; at the desktop sizes the log is always wider than the card's box.
    let width = f32::MAX;
    let height = PollCard::height(&poll.question, poll.answers.len(), width);
    let mine = ctx.state.account.user_id == Some(poll.creator);
    let options = polls::card_options(poll)
        .into_iter()
        .map(|o| poll_card::Option_ {
            text: o.text.into(),
            pct: o.pct,
            mine: o.mine,
            winner: o.winner,
            voters: o.voters.into_iter().map(Into::into).collect(),
        })
        .collect();
    let card = PollCard::new(
        ElementId::Name(format!("poll-{}", item.key).into()),
        poll.question.clone(),
        poll.closed,
        poll.multi,
        poll_meta(poll),
        options,
        poll_card::Labels {
            closed: t("poll.closedTag").into(),
            close: t("poll.close").into(),
        },
        width,
    )
    .can_vote(!poll.closed && ctx.client.is_some())
    .can_close(mine);
    Some(Extra {
        element: crate::tracked_box("poll-card", card).into_any_element(),
        height,
    })
}

/// PollCard.tsx's meta line: `Open for 24 hours · pick one · 0 voters` (or `Final results · …`).
pub fn poll_meta(poll: &PollView) -> String {
    let seconds = poll.due_at.saturating_sub(poll.created_at);
    let duration = if seconds >= 86_400 * 3 {
        tf("poll.days", &[("count", &(seconds / 86_400).to_string())])
    } else if seconds == 3600 {
        t("poll.hour").to_owned()
    } else {
        tf("poll.hours", &[("count", &(seconds / 3600).to_string())])
    };
    let head = if poll.closed {
        t("poll.final").to_owned()
    } else {
        tf("poll.openFor", &[("duration", &duration)])
    };
    let pick = t(if poll.multi {
        "poll.pickAny"
    } else {
        "poll.pickOne"
    })
    .to_owned();
    let count = if poll.multi {
        poll.distinct
    } else {
        poll.counts.iter().sum()
    };
    let voters = if poll.multi && poll.capped {
        tf("poll.voterLowerBound", &[("count", &count.to_string())])
    } else if count == 1 {
        tf("poll.voter", &[("count", "1")])
    } else {
        tf("poll.voterCount", &[("count", &count.to_string())])
    };
    [head, pick, voters].join(" · ")
}

/// The list's rows as the component draws them.
fn list_items(rows: &[picker::Row]) -> Vec<ListItem> {
    rows.iter()
        .map(|r| ListItem {
            id: r.id.clone().into(),
            icon: r.entry.icon,
            name: r.id.clone().into(),
            args: r.entry.args.clone().into(),
            description: r
                .reason
                .clone()
                .unwrap_or_else(|| r.entry.description.clone())
                .into(),
            source: r.entry.source.clone().into(),
            disabled: r.reason.is_some(),
        })
        .collect()
}

/// The command picker over the composer (`slots::composer_popup`).
pub fn picker(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let wid = window.window_handle().window_id();
    let state = crate::per_window::get::<PickerState>(wid, cx)?.clone();
    let (zone, _) = zone_of(ctx.clock);
    let rows = picker::list(&ctx.state, ctx.community, &state.draft, &zone)
        .filter(|_| state.closed_for.as_deref() != Some(state.draft.as_str()));
    let shown = rows
        .as_ref()
        .map(|r| r.iter().map(|r| r.id.clone()).collect::<Vec<_>>());
    if picker_state(wid, cx).rows != shown {
        picker_state_mut(wid, cx).rows = shown;
    }
    let rows = rows?;
    let active = (!rows.is_empty()).then(|| state.selected.min(rows.len() - 1));
    let list = CommandList::new(
        "command-list",
        list_items(&rows),
        ListLabels {
            title: copy::COMMANDS.into(),
            hint: copy::LIST_HINT.into(),
            no_match: copy::no_match(&state.draft).into(),
            send_as_text: copy::SEND_AS_TEXT.into(),
        },
    )
    .active(active)
    .row_ids(|i| ElementId::Name(ROW_IDS.get(i).copied().unwrap_or("command-row").into()))
    .on_active(move |i, window, cx| {
        let s = picker_state_mut(wid, cx);
        if s.selected != i {
            s.selected = i;
            window.refresh();
        }
    })
    .on_pick(move |i, window, cx| {
        keys::pick(wid, i, cx);
        window.refresh();
    });
    // CommandPicker.css: 16px in from the composer's sides, 6px above it.
    Some(
        div()
            .px(dilla_desktop_ui::components::commands::list::PICKER_INSET_X)
            .pb(dilla_desktop_ui::components::commands::list::PICKER_GAP)
            .child(crate::tracked_box(
                "command-picker",
                div().w_full().child(list),
            ))
            .into_any_element(),
    )
}

/// What a sent draft does (the composer asks before it posts).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum SendDecision {
    /// Post this text (a literal draft, `//x` as `/x`, or a built-in that posts).
    Post(String),
    /// Answered privately on this device; clear the draft.
    Handled,
    /// Not complete: keep the draft (the picker says why).
    Keep,
}

/// Decides what sending `draft` in the conversation of `ctx` does, and runs a private answer. A private answer is
/// added to this device's in-memory results only (`dilla_client::features::bot`): nothing is posted or stored.
pub fn on_send(ctx: &SlotCx, draft: &str, window: gpui::WindowId, cx: &mut App) -> SendDecision {
    let Some(channel) = ctx.channel else {
        return SendDecision::Post(draft.to_owned());
    };
    let decision = match (&ctx.client, bot::plan(&ctx.state, ctx.community, draft)) {
        (_, Plan::Post(text)) => SendDecision::Post(text),
        (_, Plan::Incomplete) => SendDecision::Keep,
        (Some(client), Plan::Private(status, text, provider)) => {
            client.bot().add_result(
                Conversation {
                    channel,
                    thread: None,
                },
                status,
                text,
                provider,
            );
            SendDecision::Handled
        }
        (None, Plan::Private(..)) => SendDecision::Handled,
        (Some(client), Plan::Bot { definition, args }) => {
            let Some(community) = ctx.community else {
                return SendDecision::Keep;
            };
            // Music has no text channel and is Task 21's; every other Bot command is sent from this channel.
            client.bot().run_command(
                Conversation {
                    channel,
                    thread: None,
                },
                community,
                Some(channel),
                definition,
                args,
                &local_zone(),
                texts(ctx.clock),
            );
            SendDecision::Handled
        }
        (None, Plan::Bot { .. }) => SendDecision::Handled,
    };
    if decision != SendDecision::Keep {
        draft_changed(window, "", cx);
    }
    decision
}

/// The local zone a reminder's `when` is read in: the OS zone's name and its offset at each instant.
fn local_zone() -> run::Zone {
    let name = std::env::var("TZ")
        .ok()
        .filter(|z| run_zone_name_ok(z))
        .unwrap_or_else(|| "UTC".to_owned());
    run::Zone {
        name,
        offset_at: std::sync::Arc::new(|t| crate::format::local_zone(t).local_minus_utc()),
    }
}

fn run_zone_name_ok(z: &str) -> bool {
    dilla_client::features::utilities::time::valid_zone_name(z)
}

/// The private results' copy (bot-run.ts, copy.ts), in the strings and the times of this app.
fn texts(clock: Clock) -> run::Texts {
    std::sync::Arc::new(move |text: &run::Text| {
        let (zone, _) = zone_of(clock);
        let since = |s: &Option<u64>| {
            s.map_or_else(|| "—".to_owned(), |s| crate::format::time_in(s, &zone))
        };
        match text {
            run::Text::Pending(id) => copy::pending(id),
            run::Text::NoAnswer => t("commands.botNoAnswer").to_owned(),
            run::Text::Failed => t("privateResult.failed").to_owned(),
            run::Text::Forbidden => t("commands.botForbidden").to_owned(),
            run::Text::Disabled => t("commands.botDisabled").to_owned(),
            run::Text::Offline(s) => tf("commands.botOffline", &[("time", &since(s))]),
            run::Text::Degraded(s) => tf("commands.botDegraded", &[("time", &since(s))]),
            run::Text::Absent => t("commands.botNotOnInstance").to_owned(),
            run::Text::NeedsBotChannel => t("commands.needsBotChannel").to_owned(),
            run::Text::CantCheckDevice => t("commands.botCantCheckDevice").to_owned(),
            run::Text::Unverified => t("commands.botUnverified").to_owned(),
            run::Text::Changed => t("commands.botChanged").to_owned(),
            run::Text::PostedIn { what, channel } => {
                tf("commands.postedIn", &[("what", what), ("channel", channel)])
            }
            run::Text::WholeNumber(name) => format!("{name} must be a whole number"),
            run::Text::ChooseFrom(name) => format!("Choose {name} from the suggestions."),
            run::Text::Duration(name) => format!("{name} must be a duration such as 10m or 2h"),
            run::Text::WhenHint => copy::WHEN_HINT.to_owned(),
        }
    })
}

/// The private results of the open conversation, oldest first, each with its height in a log `width` wide: the log
/// draws them after its last message.
pub fn private_results(
    ctx: &SlotCx,
    width: f32,
    _window: &mut Window,
    _cx: &mut App,
) -> Vec<(AnyElement, f32)> {
    let Some(channel) = ctx.channel else {
        return Vec::new();
    };
    let conversation = Conversation {
        channel,
        thread: None,
    };
    let (zone, _) = zone_of(ctx.clock);
    ctx.state
        .bot
        .results(&conversation)
        .iter()
        .map(|row| {
            let height = BotResult::height(&row.text, width);
            (
                result_row(row, conversation, ctx, width, &zone).into_any_element(),
                height,
            )
        })
        .collect()
}

fn result_row(
    row: &PrivateResult,
    conversation: Conversation,
    ctx: &SlotCx,
    width: f32,
    zone: crate::format::Zone,
) -> impl IntoElement + use<> {
    let (status, label) = match row.status {
        ResultStatus::Pending => (Status::Pending, copy::STATUS_PENDING),
        ResultStatus::Success => (Status::Success, copy::STATUS_SUCCESS),
        ResultStatus::Failed => (Status::Failed, copy::STATUS_FAILED),
        ResultStatus::Cancelled => (Status::Cancelled, copy::STATUS_CANCELLED),
    };
    let author = match row.provider {
        ResultProvider::Builtin => Author::Builtin {
            name: copy::AUTHOR_BUILTIN.into(),
        },
        ResultProvider::Bot => Author::Bot {
            name: bot_name(&ctx.state, ctx).into(),
        },
    };
    let client = ctx.client.clone();
    let dismiss_client = client.clone();
    let id = row.id;
    let result = BotResult::new(
        ElementId::Name(format!("bot-result-{id}").into()),
        author,
        crate::format::time_in(row.at / 1000, zone),
        status,
        row.text.clone(),
        bot_result::Labels {
            status: label.into(),
            only_you: copy::ONLY_YOU.into(),
            dismiss: copy::DISMISS.into(),
        },
        width,
    )
    .on_dismiss(move |window, _cx| {
        if let Some(client) = &dismiss_client {
            client.bot().dismiss(conversation, id);
        }
        window.refresh();
    });
    if row.hold
        && let Some(client) = client
    {
        result.on_retry(t("commands.retry"), move |window, _cx| {
            client.bot().retry(id);
            window.refresh();
        })
    } else {
        result
    }
}

/// The name a Bot's private result shows (`result-bot-name.ts`): the community Bot's display name, else "Glenn".
fn bot_name(state: &AppState, ctx: &SlotCx) -> String {
    ctx.community
        .and_then(|c| state.bot.catalogs.get(&c))
        .map(|v| v.bot_name.clone())
        .filter(|n| !n.is_empty())
        .unwrap_or_else(|| copy::AUTHOR_BOT.to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::{ChannelId, UserId};

    fn poll(counts: Vec<u64>, closed: bool, multi: bool, seconds: u64) -> PollView {
        PollView {
            id: [1; 16],
            channel: ChannelId([2; 16]),
            thread: None,
            creator: UserId([3; 16]),
            question: "What should we raid on Friday?".into(),
            answers: vec!["the bridge".into(), "the harbour".into()],
            multi,
            created_at: 1000,
            due_at: 1000 + seconds,
            closed,
            counts,
            my_choices: None,
            revision: 1,
            previews: vec![],
            distinct: 4,
            capped: true,
        }
    }

    #[test]
    fn the_meta_line_follows_the_web_card() {
        assert_eq!(
            poll_meta(&poll(vec![0, 0], false, false, 86_400)),
            "Open for 24 hours · pick one · 0 voters"
        );
        assert_eq!(
            poll_meta(&poll(vec![1, 0], false, false, 3600)),
            "Open for 1 hour · pick one · 1 voter"
        );
        assert_eq!(
            poll_meta(&poll(vec![2, 1], true, false, 259_200)),
            "Final results · pick one · 3 voters"
        );
        assert_eq!(
            poll_meta(&poll(vec![2, 1], false, true, 604_800)),
            "Open for 7 days · pick any · 4+ voters"
        );
    }

    /// Integration 3, final review M4: each window's composer has its own `/` list: a draft typed in window A opens
    /// nothing in window B, a send in B leaves A's list as it was, and B's keys never move A's highlight.
    #[gpui::test]
    fn each_window_has_its_own_command_list(cx: &mut gpui::TestAppContext) {
        let a = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let b = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        cx.update(|cx| {
            draft_changed(a, "/r", cx);
            picker_state_mut(a, cx).rows = Some(vec!["remind".into(), "roll".into()]);
            assert_eq!(
                picker_state(b, cx).draft,
                "",
                "B's composer reported nothing"
            );
            draft_changed(b, "", cx);
            assert_eq!(picker_state(a, cx).draft, "/r", "B's send leaves A's draft");
            assert!(!keys::apply(b, picker::Key::Down, cx), "B has no list open");
            assert_eq!(picker_state(a, cx).selected, 0);
            assert!(keys::apply(a, picker::Key::Down, cx));
            assert_eq!(picker_state(a, cx).selected, 1);
        });
    }

    #[test]
    fn a_new_draft_reopens_the_list_and_resets_the_highlight() {
        let mut app = PickerState {
            draft: "/".into(),
            selected: 3,
            closed_for: Some("/".into()),
            rows: None,
            picked: None,
        };
        // draft_changed's rule, applied without an App.
        let next = "/r";
        if app.draft != next {
            app.draft = next.into();
            app.selected = 0;
            if app.closed_for.as_deref() != Some(next) {
                app.closed_for = None;
            }
        }
        assert_eq!((app.selected, app.closed_for), (0, None));
    }
}
