//! The reminders popover under the top bar's bell (Signal §6.7; packages/ui RemindersPopover, fed as
//! UtilityCommandProvider.tsx `fetchInbox` and `ReminderPanel` feed it), one per window.
//!
//! - The bell ([`bell_state`], [`toggle`]) opens and closes it; its badge counts the due reminders.
//! - The list is this member's reminders in every server they are in, as the Bot answered them (`state.bot.reminders`),
//!   scheduled and due ones only, soonest first. It is read at the window's first frame with a client and again each time
//!   the popover opens; a server whose Bot cannot answer drops out of the list.
//! - Due lists the due ones with Dismiss, Upcoming the scheduled ones with Cancel (by the reminder's state, as the web
//!   splits them). The tab follows the list (Due while anything is due) until one is picked.
//! - The picked tab takes the focus when it opens; ←→ Home End move between the tabs, Escape closes it and gives the bell
//!   the focus back, a press outside it (but not on the bell) closes it.

use std::rc::Rc;

use dilla_client::Client;
use dilla_client::features::utilities::UtilityError;
use dilla_client::state::bot::ReminderStatus;
use dilla_client::state::{AppState, CommunityId};
use gpui::{
    AnyElement, App, Bounds, FocusHandle, InteractiveElement, IntoElement, KeyDownEvent,
    MouseButton, MouseDownEvent, ParentElement, Pixels, SharedString, Styled, Window, WindowId,
    deferred, div, px,
};

use dilla_desktop_ui::components::commands::reminders::{Item, Labels, RemindersPopover, Tab};

use crate::screens::shell::Clock;
use crate::slots::SlotCx;
use crate::strings::t;

/// `.d-reminders`: 36px under the bell's top, its right edge 6px past the bell's.
const TOP: f32 = 36.0;
const RIGHT: f32 = -6.0;
/// A Cancel or Dismiss that failed otherwise (`ReminderPanel.mutate` holds it as a literal; `strings.rs` has no key).
const UPDATE_FAILED: &str = "Could not update this reminder. Refresh and try again.";
/// The Bot's account kind (bot-member.ts `BOT_ACCOUNT_KIND`).
const BOT_KIND: u8 = 1;

/// One window's popover.
#[derive(Default)]
struct Inbox {
    open: bool,
    /// The picked tab; `None` follows the list (Due while anything is due).
    tab: Option<Tab>,
    /// A read is out and the list is not known yet (the popover says so and draws no rows).
    loading: bool,
    /// The last read failed somewhere ("Some reminders could not be loaded").
    error: bool,
    /// A Cancel or Dismiss is out: every action waits.
    busy: bool,
    /// Why the last Cancel or Dismiss did not go through.
    action_error: Option<String>,
    /// The window's first read was asked for.
    started: bool,
    /// A read the next frame starts (the popover opened).
    read_pending: bool,
    /// The newest read: an older one's answer is dropped.
    read: u64,
    /// The picked tab takes the focus on the next frame.
    focus_pending: bool,
    /// Escape gives the bell the focus back.
    bell_focus: Option<FocusHandle>,
    tabs: Option<(FocusHandle, FocusHandle)>,
    /// The bell's bounds: a press there is the bell's toggle, not a press outside.
    bell: Option<Bounds<Pixels>>,
}

fn inbox(window: WindowId, cx: &mut App) -> &mut Inbox {
    crate::per_window::get_mut::<Inbox>(window, cx)
}

/// One reminder of the list: where it is kept and its row.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Row {
    pub community: CommunityId,
    pub reminder: dilla_client::state::bot::ReminderView,
}

/// The reminders the popover lists (`currentRecords`): of the servers this account is in, scheduled or due, soonest first.
pub fn rows(state: &AppState) -> Vec<Row> {
    let mut rows: Vec<Row> = state
        .communities
        .iter()
        .flat_map(|c| {
            state
                .bot
                .reminders
                .get(&c.id)
                .into_iter()
                .flatten()
                .filter(|r| matches!(r.state, ReminderStatus::Scheduled | ReminderStatus::Due))
                .map(|r| Row {
                    community: c.id,
                    reminder: r.clone(),
                })
        })
        .collect();
    rows.sort_by_key(|r| (r.reminder.due, r.reminder.id));
    rows
}

/// The due ones (the bell's badge).
pub fn due_count(state: &AppState) -> usize {
    rows(state)
        .iter()
        .filter(|r| r.reminder.state == ReminderStatus::Due)
        .count()
}

/// What the bell draws: its badge's count and whether the popover is open.
pub fn bell_state(window: WindowId, state: &AppState, cx: &App) -> (usize, bool) {
    let open = crate::per_window::get::<Inbox>(window, cx).is_some_and(|i| i.open);
    (due_count(state), open)
}

/// The bell was pressed: the popover opens (and reads the list again) or closes.
pub fn toggle(window: WindowId, cx: &mut App) {
    let i = inbox(window, cx);
    if i.open {
        i.open = false;
    } else {
        i.open = true;
        i.tab = None;
        i.action_error = None;
        i.read_pending = true;
        i.focus_pending = true;
    }
    cx.refresh_windows();
}

/// Records the bell's bounds and its focus (the top bar draws the bell).
pub fn note_bell(window: WindowId, bounds: Bounds<Pixels>, cx: &mut App) {
    inbox(window, cx).bell = Some(bounds);
}

/// The bell's focus handle (Escape gives it the focus back).
pub fn bell_focus(window: WindowId, cx: &mut App) -> FocusHandle {
    if let Some(f) = &inbox(window, cx).bell_focus {
        return f.clone();
    }
    let f = cx.focus_handle().tab_stop(true);
    inbox(window, cx).bell_focus = Some(f.clone());
    f
}

fn close(window: WindowId, cx: &mut App) {
    inbox(window, cx).open = false;
    cx.refresh_windows();
}

/// `fetchInbox`'s wait before server `index` (w2-bot-review M-D2): `min(index·120, 900)` ms, none before the first, so
/// a member of many servers does not burst their Bots.
fn stagger(index: usize) -> Option<std::time::Duration> {
    (index > 0).then(|| {
        std::time::Duration::from_millis(
            u64::try_from(index)
                .unwrap_or(u64::MAX)
                .saturating_mul(120)
                .min(900),
        )
    })
}

/// What one server's read does to the list (`fetchInbox`): `(forget its reminders, the read failed)`. A server whose
/// Bot is disabled or inaccessible, and one whose read failed (w2-bot-review M-D1), drops out of the list: its rows never
/// stay on screen, stale, under "Some reminders could not be loaded".
fn outcome(answer: &Result<(), UtilityError>) -> (bool, bool) {
    match answer {
        Ok(()) => (false, false),
        Err(UtilityError::Unavailable(_) | UtilityError::Status(2 | 3)) => (true, false),
        Err(_) => (true, true),
    }
}

/// Reads every server's reminders again (`fetchInbox`), one server after another; a server whose Bot cannot answer
/// drops out of the list. `show_loading`: the list is hidden behind the loading line until the read lands.
fn read(window: WindowId, client: &Rc<Client>, state: &AppState, show_loading: bool, cx: &mut App) {
    let communities: Vec<CommunityId> = state.communities.iter().map(|c| c.id).collect();
    let i = inbox(window, cx);
    i.read += 1;
    let token = i.read;
    if show_loading {
        i.loading = true;
    }
    let job = client.utilities().run(move |u| async move {
        let mut failed = false;
        for (index, community) in communities.into_iter().enumerate() {
            if let Some(wait) = stagger(index) {
                tokio::time::sleep(wait).await;
            }
            let answer = u.refresh_reminders(community).await.map(|_| ());
            let (forget, fails) = outcome(&answer);
            if forget {
                u.hold_reminders(community, Vec::new());
            }
            failed |= fails;
        }
        Ok(failed)
    });
    cx.spawn(async move |cx| {
        let failed = job.await.unwrap_or(true);
        cx.update(|cx| {
            let i = inbox(window, cx);
            if i.read == token {
                i.loading = false;
                i.error = failed;
            }
            cx.refresh_windows();
        });
    })
    .detach();
}

/// Cancels an upcoming reminder or dismisses a due one (`ReminderPanel.mutate`), then reads the list again.
fn settle(window: WindowId, client: &Rc<Client>, row: &Row, cx: &mut App) {
    let i = inbox(window, cx);
    if i.busy {
        return;
    }
    i.busy = true;
    i.action_error = None;
    let (community, id) = (row.community, row.reminder.id);
    let dismiss = row.reminder.state == ReminderStatus::Due;
    let job = client
        .utilities()
        .run(move |u| async move { u.settle_reminder(community, id, dismiss).await.map(|_| ()) });
    let client = client.clone();
    cx.spawn(async move |cx| {
        let answer = job.await;
        cx.update(|cx| {
            let i = inbox(window, cx);
            i.busy = false;
            i.action_error = match answer {
                Ok(()) => None,
                Err(UtilityError::Status(4)) => Some(t("reminder.conflict").to_owned()),
                Err(UtilityError::Uncertain { .. }) => {
                    Some(t("reminder.uncertainMutation").to_owned())
                }
                Err(UtilityError::Unavailable(_)) => Some(t("reminder.unavailable").to_owned()),
                Err(_) => Some(UPDATE_FAILED.to_owned()),
            };
            let state = client.state().borrow().clone();
            read(window, &client, &state, false, cx);
        });
    })
    .detach();
}

/// The popover's tab: the picked one, else Due while anything is due, else Upcoming.
fn tab_of(picked: Option<Tab>, due: usize) -> Tab {
    picked.unwrap_or(if due > 0 { Tab::Due } else { Tab::Upcoming })
}

/// The popover under the bell while it is open, else nothing. Also starts the window's first read (the badge's count).
pub fn popover(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let wid = window.window_handle().window_id();
    if let Some(client) = &ctx.client {
        let i = inbox(wid, cx);
        let first = !i.started;
        let again = std::mem::take(&mut i.read_pending);
        i.started = true;
        if first || again {
            read(wid, client, &ctx.state, first, cx);
        }
    }
    let i = inbox(wid, cx);
    if !i.open {
        return None;
    }
    if i.tabs.is_none() {
        let tabs = (cx.focus_handle(), cx.focus_handle());
        inbox(wid, cx).tabs = Some(tabs);
    }
    let i = inbox(wid, cx);
    let (due_tab, upcoming_tab) = i.tabs.clone().expect("made above");
    let (picked, loading, error, busy) = (i.tab, i.loading, i.error, i.busy);
    let action_error = i.action_error.clone();
    let focus_now = std::mem::take(&mut i.focus_pending);

    let state = ctx.state.clone();
    let listed = rows(&state);
    let due = listed
        .iter()
        .filter(|r| r.reminder.state == ReminderStatus::Due)
        .count();
    let tab = tab_of(picked, due);
    if focus_now {
        window.focus(
            if tab == Tab::Due {
                &due_tab
            } else {
                &upcoming_tab
            },
            cx,
        );
    }
    let now = ctx.clock.now();
    let items: Vec<Item> = if loading {
        Vec::new()
    } else {
        listed
            .iter()
            .map(|r| item(&state, ctx.clock, now, r, busy))
            .collect()
    };
    let status = if loading {
        Some(t("reminder.loading"))
    } else if error {
        Some(t("reminder.loadError"))
    } else {
        None
    }
    .map(|s| SharedString::from(s.to_owned()))
    .or(action_error.map(SharedString::from));
    // The prototype's header tile ("GL"): the initials of the server's Bot, the account that keeps the reminders.
    let initials = ctx
        .community
        .or_else(|| listed.first().map(|r| r.community))
        .and_then(|c| state.members.get(&c))
        .and_then(|ms| ms.iter().find(|m| m.kind == BOT_KIND))
        .map(|m| {
            let name = if m.display.is_empty() {
                &m.username
            } else {
                &m.display
            };
            name.trim()
                .chars()
                .take(2)
                .collect::<String>()
                .to_uppercase()
        })
        .filter(|s| !s.is_empty());

    let labels = Labels {
        title: t("reminder.inbox").into(),
        due: t("reminder.due").into(),
        upcoming: t("reminder.upcoming").into(),
        dismiss: t("reminder.dismiss").into(),
        cancel: t("reminder.cancel").into(),
        empty_due: t("reminder.emptyDue").into(),
        empty_upcoming: t("reminder.emptyUpcoming").into(),
        footer: t("reminder.footer").into(),
    };
    let client = ctx.client.clone();
    #[cfg(feature = "automation")]
    let probe_tabs = (due_tab.clone(), upcoming_tab.clone());
    let mut panel = RemindersPopover::new("reminders", items, tab, labels)
        .status(status)
        .loading(loading)
        .tab_focus(due_tab.clone(), upcoming_tab.clone())
        .on_tab(move |next, window, cx| {
            inbox(wid, cx).tab = Some(next);
            window.focus(
                if next == Tab::Due {
                    &due_tab
                } else {
                    &upcoming_tab
                },
                cx,
            );
            cx.refresh_windows();
        })
        .on_action(move |index, _, cx| {
            if let (Some(client), Some(row)) = (&client, listed.get(index)) {
                settle(wid, client, row, cx);
            }
        });
    if let Some(initials) = initials {
        panel = panel.bot_initials(initials);
    }
    #[cfg(feature = "automation")]
    {
        panel = panel.probe(Rc::new(|id: &str| {
            let id = track_id(id);
            div()
                .absolute()
                .size_full()
                .debug_selector(move || id.to_owned())
                .child(crate::automation::bounds_probe(id))
                .into_any_element()
        }));
    }
    let root = div()
        .key_context("RemindersPopover")
        .on_key_down(move |e: &KeyDownEvent, window, cx| {
            if e.keystroke.key == "escape" {
                cx.stop_propagation();
                close(wid, cx);
                let bell = bell_focus(wid, cx);
                window.focus(&bell, cx);
            }
        })
        .on_mouse_down_out(move |e: &MouseDownEvent, _, cx| {
            if e.button != MouseButton::Left && e.button != MouseButton::Right {
                return;
            }
            let on_bell = inbox(wid, cx).bell.is_some_and(|b| b.contains(&e.position));
            if !on_bell {
                close(wid, cx);
            }
        })
        .child(panel);
    #[cfg(feature = "automation")]
    let root = root
        .relative()
        .child(crate::automation::bounds_probe("reminders-popover"))
        .child(crate::automation::focus_probe(Tab::Due.id(), &probe_tabs.0))
        .child(crate::automation::focus_probe(
            Tab::Upcoming.id(),
            &probe_tabs.1,
        ));
    Some(
        deferred(
            div()
                .absolute()
                .top(px(TOP))
                .right(px(RIGHT))
                .occlude()
                .child(root),
        )
        .with_priority(4)
        .into_any_element(),
    )
}

/// A reminder's row (`ReminderItem`): its text, when it is due and the channel it was set in.
fn item(state: &AppState, clock: Clock, now: u64, row: &Row, busy: bool) -> Item {
    let r = &row.reminder;
    let days = clock.day(r.due) - clock.day(now);
    let when = clock.format(r.due, |at| crate::format::reminder_when(at, days));
    // `sourceWhere`: "#general" when this device knows the channel.
    let place = r
        .source_channel
        .and_then(|c| state.channel(c))
        .map(|c| format!("#{}", c.name))
        .unwrap_or_default();
    Item {
        id: hex(&r.id).into(),
        text: r.text.clone().into(),
        when: when.into(),
        place: place.into(),
        due: r.state == ReminderStatus::Due,
        busy,
    }
}

fn hex(id: &[u8; 16]) -> String {
    id.iter().map(|b| format!("{b:02x}")).collect()
}

/// The automation id of a row's element: interned, so a name keeps one `&'static str`.
#[cfg(feature = "automation")]
fn track_id(id: &str) -> &'static str {
    use std::cell::RefCell;
    use std::collections::HashMap;
    thread_local! {
        // App-wide by design: interned automation ids (element names, never a client handle).
        static IDS: RefCell<HashMap<String, &'static str>> = RefCell::new(HashMap::new());
    }
    IDS.with(|ids| {
        let mut ids = ids.borrow_mut();
        if let Some(known) = ids.get(id) {
            return *known;
        }
        let leaked: &'static str = Box::leak(id.to_owned().into_boxed_str());
        ids.insert(id.to_owned(), leaked);
        leaked
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::Community;
    use dilla_client::state::bot::ReminderView;

    /// w2-bot-review M-D1 and M-D2 (`fetchInbox`): a failed server drops out of the list as an unavailable one does, and
    /// the reads wait `min(index·120, 900)` ms between servers.
    #[test]
    fn a_failed_server_drops_out_and_the_reads_are_staggered() {
        assert_eq!(outcome(&Ok(())), (false, false));
        assert_eq!(
            outcome(&Err(UtilityError::Unavailable("x".into()))),
            (true, false)
        );
        assert_eq!(outcome(&Err(UtilityError::Status(3))), (true, false));
        assert_eq!(outcome(&Err(UtilityError::Invalid)), (true, true));
        assert_eq!(outcome(&Err(UtilityError::Status(4))), (true, true));
        let ms = |i| stagger(i).map(|d| d.as_millis());
        assert_eq!(ms(0), None);
        assert_eq!(ms(1), Some(120));
        assert_eq!(ms(7), Some(840));
        assert_eq!(ms(8), Some(900));
        assert_eq!(ms(100), Some(900));
    }

    fn reminder(id: u8, due: u64, state: ReminderStatus) -> ReminderView {
        ReminderView {
            id: [id; 16],
            text: format!("r{id}"),
            due,
            timezone: "UTC".into(),
            source_channel: None,
            source_thread: None,
            state,
            revision: 1,
        }
    }

    /// `currentRecords` and `fetchInbox`'s filter: the servers this account is still in, scheduled and due reminders
    /// only, soonest first across servers.
    #[test]
    fn the_list_is_every_joined_servers_open_reminders_soonest_first() {
        let (a, b, gone) = (
            CommunityId([1; 16]),
            CommunityId([2; 16]),
            CommunityId([3; 16]),
        );
        let mut s = AppState::default();
        for id in [a, b] {
            s.communities.push(Community {
                id,
                name: String::new(),
                owner: dilla_client::state::UserId([0; 16]),
                policy_version: 0,
            });
        }
        s.bot.reminders.insert(
            a,
            vec![
                reminder(1, 300, ReminderStatus::Scheduled),
                reminder(2, 50, ReminderStatus::Cancelled),
            ],
        );
        s.bot.reminders.insert(
            b,
            vec![
                reminder(3, 100, ReminderStatus::Due),
                reminder(4, 200, ReminderStatus::Dismissed),
            ],
        );
        s.bot
            .reminders
            .insert(gone, vec![reminder(5, 10, ReminderStatus::Due)]);
        let ids: Vec<u8> = rows(&s).iter().map(|r| r.reminder.id[0]).collect();
        assert_eq!(ids, [3, 1]);
        assert_eq!(due_count(&s), 1);
    }

    /// `tab ?? (dueCount > 0 ? 'due' : 'upcoming')`: a picked tab holds.
    #[test]
    fn the_tab_follows_the_list_until_one_is_picked() {
        assert_eq!(tab_of(None, 2), Tab::Due);
        assert_eq!(tab_of(None, 0), Tab::Upcoming);
        assert_eq!(tab_of(Some(Tab::Upcoming), 2), Tab::Upcoming);
    }
}
