//! What the messages area keeps between frames: the row being edited, the delete asked about, the open "more" menu, the
//! pins dialog and the row a jump flashes. The timeline is a function of the client's state, so this lives in one keyed
//! entity of the window ([`ui`]); a message action (a closure the toolbar calls) reaches it through [`handle`].

use std::rc::Rc;
use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::features::moderation::{
    Evidence, REMOVAL_EXCERPT_CHARS, Removal, RemovalOutcome, removal_reason_ok,
};
use dilla_client::state::{ChannelId, CommunityId, MessageId, TimelineItem};
use gpui::{
    App, AppContext as _, Context, Entity, EntityInputHandler as _, Focusable as _, Subscription,
    Window,
};
use gpui_component::input::{InputEvent, TextareaState};
use zeroize::Zeroizing;

use super::mentions::{
    MentionMember, MentionRole, edit_roles, editable_text, encode_mentions, mention_roles,
};
use crate::slots::SlotCx;

/// The message editor's budget (MessageEditor `maxLength`).
pub const EDIT_MAX: usize = 4000;
/// How long a jump's flash lasts (`--duration-flash`).
pub const FLASH: Duration = Duration::from_millis(1400);

/// The row being edited.
pub struct Editing {
    pub key: String,
    pub channel: ChannelId,
    pub msg: MessageId,
    /// Set from the item when the editor opened; only an own message gets one.
    pub own: bool,
    pub input: Entity<TextareaState>,
    /// `None` while a fixture is drawn: saving then only closes the editor.
    client: Option<Rc<Client>>,
    /// The window the editor opened in: a refused save is said there.
    window: gpui::WindowId,
    members: Vec<MentionMember>,
    /// The members' usernames, which a role's name may not shadow (`mention_roles`).
    usernames: Vec<String>,
    /// The message's text as the editor opened it: a role it already names stays encodable (`editRoles`).
    body: String,
    roles: Vec<MentionRole>,
    broadcast: bool,
    _subscription: Subscription,
}

/// What a refused edit says (`None` when it was queued): the changed-key copy for the trust gate's refusal, else
/// `shell.banner.commandError` with the refusal's code.
pub(crate) fn edit_refusal<E>(
    answer: Result<Result<(), dilla_client::ClientError>, E>,
) -> Option<String> {
    let code = match answer {
        Ok(Ok(())) => return None,
        Ok(Err(dilla_client::ClientError::Sync(code))) => code,
        Ok(Err(dilla_client::ClientError::Stopped)) | Err(_) => "E_STOPPED".to_owned(),
        Ok(Err(_)) => "E_NETWORK".to_owned(),
    };
    Some(if code == "E_UMK_CHANGED" {
        crate::strings::t("privacy.changed").to_owned()
    } else {
        crate::strings::tf("shell.banner.commandError", &[("code", &code)])
    })
}

/// Conversation.tsx's edit rule, from `state` now: the composer's broadcast right in `channel` and the roles offered plus
/// every role `body` already names.
pub(crate) fn edit_rights(
    state: &dilla_client::state::AppState,
    channel: ChannelId,
    body: &str,
    usernames: &[&str],
) -> (bool, Vec<MentionRole>) {
    let (broadcast, roles) = crate::screens::composer::mention_rights(state, Some(channel));
    let offered = edit_roles(&roles, broadcast, body);
    (broadcast, mention_roles(&offered, usernames))
}

impl Editing {
    /// The text as it will be sent: names encoded back to their tokens. The rights are the conversation's now, as the web
    /// recomputes them on every render (review IJ m2): bits read after the editor opened apply to its save.
    pub fn encoded(&self, cx: &App) -> String {
        let (broadcast, roles) = match &self.client {
            Some(client) => {
                let state = client.state().borrow().clone();
                let names: Vec<&str> = self.usernames.iter().map(String::as_str).collect();
                edit_rights(&state, self.channel, &self.body, &names)
            }
            None => (self.broadcast, self.roles.clone()),
        };
        encode_mentions(
            &self.input.read(cx).value(),
            &self.members,
            broadcast,
            &roles,
        )
    }

    /// The budget used, in UTF-8 bytes of the encoded text (the web's `measure`).
    pub fn used(&self, cx: &App) -> usize {
        self.encoded(cx).len()
    }
}

/// A delete waiting for its confirmation.
#[derive(Clone, Copy)]
pub struct Deleting {
    pub channel: ChannelId,
    pub msg: MessageId,
}

/// Where a moderated removal is (manual-removal.ts `ManualRemovalPhase`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ModPhase {
    /// The reason is asked for.
    Confirm,
    Sending,
    /// The removal's and its evidence's outcome is shown, with a retry while either is not settled.
    Result,
}

/// A moderated removal of another person's message (Conversation.tsx `moderateFor`): the dialog while `open`, then, once
/// closed on an unsettled result, the status line with its retry. The excerpt and the reason as sent are plaintext of a
/// message, held in [`Zeroizing`] buffers; the field's own text goes with the entity when the removal is let go.
pub struct Moderating {
    pub channel: ChannelId,
    pub community: CommunityId,
    pub msg: MessageId,
    pub seq: u64,
    pub excerpt: Zeroizing<String>,
    /// The reason as sent (kept for "Retry evidence"); empty until the removal is sent.
    pub sent_reason: Zeroizing<String>,
    pub reason: Entity<TextareaState>,
    pub phase: ModPhase,
    pub removal: Option<Removal>,
    pub proof: Option<[u8; 16]>,
    pub evidence: Option<Evidence>,
    /// The dialog is open (`moderateDialogOpen`); closed on an unsettled result, the status line shows instead.
    pub open: bool,
    client: Option<Rc<Client>>,
}

impl Moderating {
    /// The reason typed so far.
    pub fn reason_text(&self, cx: &App) -> Zeroizing<String> {
        Zeroizing::new(self.reason.read(cx).value().to_string())
    }

    /// Conversation.tsx: Remove acts on a non-blank reason of at most 1024 bytes.
    pub fn can_remove(&self, cx: &App) -> bool {
        removal_reason_ok(&self.reason_text(cx))
    }

    /// What the result says: the outcome's strings key (Conversation.tsx's chain).
    pub fn result_key(&self) -> &'static str {
        match self.evidence {
            Some(Evidence::Accepted) => "shell.moderation.evidenceSent",
            Some(Evidence::Pending) => "shell.moderation.evidencePending",
            Some(Evidence::Rejected) => "shell.moderation.evidenceRejected",
            _ if self.removal == Some(Removal::Unknown) => "shell.moderation.removalUnknown",
            _ => "shell.moderation.evidenceUnknown",
        }
    }

    /// The result's action: Done once the evidence is stored, else the retry that fits.
    pub fn retry_key(&self) -> &'static str {
        if self.removal == Some(Removal::Unknown) {
            "shell.moderation.retryRemoval"
        } else {
            "shell.moderation.retryEvidence"
        }
    }

    pub fn settled(&self) -> bool {
        self.evidence == Some(Evidence::Accepted)
    }

    /// An accepted or unknown outcome: the result is shown.
    fn take(&mut self, outcome: RemovalOutcome) {
        self.phase = ModPhase::Result;
        self.removal = Some(outcome.removal);
        self.proof = Some(outcome.proof_request_id);
        self.evidence = Some(outcome.evidence);
    }
}

#[derive(Default)]
pub struct MessageUi {
    pub editing: Option<Editing>,
    pub deleting: Option<Deleting>,
    /// The moderated removal asked for, being sent, or with its result.
    pub moderating: Option<Moderating>,
    /// The row whose "more" menu is open.
    pub menu: Option<String>,
    /// The channel whose pins dialog is open.
    pub pins: Option<ChannelId>,
    /// The row a jump flashes, and since when: the flash starts at the row's first draw (the log may need frames to bring
    /// the row in), not at the jump.
    pub flash: Option<(String, Option<Instant>)>,
    /// Which jump the flash is: a new jump to the same row starts its animation again.
    pub flash_seq: u64,
    /// A row the log still has to scroll into view (a jump from the pins or a reply).
    pub jump: Option<String>,
    /// The pending [`MessageUi::jump`] puts its row in the middle of the log and takes the focus (a search hit,
    /// Shell.tsx:1788-1793), instead of near the top with a flash.
    pub centre: bool,
    /// What the timeline was last drawn with: the dialogs (drawn over the whole window by the shell's overlays) read it.
    pub ctx: Option<SlotCx>,
    /// The log's focus: PageUp and PageDown act while it holds the focus.
    pub log_focus: Option<gpui::FocusHandle>,
    /// The log's active row (Conversation.tsx `activeKey`): the row that is the log's one tab stop; the newest when
    /// it is `None` or no longer in the log.
    pub active: Option<String>,
    /// Each row's focus handle, by the message's key (kept while the row is in the log).
    pub row_focus: std::collections::HashMap<String, gpui::FocusHandle>,
    /// A row the arrow keys moved to that the log still has to bring into view (no flash, the least scroll).
    pub reveal: Option<String>,
    /// The conversation's notice and the channel it is about (Conversation.tsx `notice`): a jump to a message the log
    /// has not loaded says so.
    notice: Option<(ChannelId, &'static str)>,
    /// The log's first row and its scrolled height when it was last drawn (MessageLog.tsx `prevFirst`, `prevHeight`):
    /// a new first row is earlier rows put in front, and the offset moves by the added height.
    pub prev_first: Option<String>,
    pub prev_height: f32,
    /// The channel whose "load earlier" is being read (Shell.tsx `loadingEarlier`): its log is busy meanwhile.
    pub loading_earlier: Option<ChannelId>,
    /// Task 36c: the log's mount (Conversation.tsx `key={logKey}`: the channel and whether its timeline is read), when it
    /// mounted, whether its first paint is still to come, and when each row entered. The rows are not all drawn, so a row's
    /// entrance cannot live in its element state (it would play again when the row scrolls back into the drawn range).
    pub log: LogEntrances<(ChannelId, bool)>,
}

/// A message log's entrances: its mount (the web's `key={logKey}`: Conversation.tsx:394 for the channel's log, Shell.tsx:2257
/// `key={rootMsgId}` for a thread's) and when each row entered. The rows are not all drawn, so a row's entrance cannot live
/// in its element state (it would play again when the row scrolls back into the drawn range).
#[derive(Debug)]
pub struct LogEntrances<K> {
    mount: Option<LogMount<K>>,
    entrances: dilla_desktop_ui::motion::Entrances<String>,
}

impl<K> Default for LogEntrances<K> {
    fn default() -> Self {
        Self {
            mount: None,
            entrances: Default::default(),
        }
    }
}

/// The log's mount: its key, and when it mounted.
#[derive(Clone, Copy, Debug, PartialEq)]
struct LogMount<K> {
    key: K,
    at: Instant,
    /// The first paint's rows have not been staggered yet (MessageLog.tsx's mount effect runs once).
    fresh: bool,
}

/// MessageLog.tsx `STAGGER_ROWS`: how many of the log's last rows the first paint staggers.
pub const STAGGER_ROWS: usize = 20;

/// What the open log's entrances ask of this frame.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct LogMotion {
    /// The log's own fade and rise (`d-log-fade`, `d-log-rise`): its opacity and its offset down in px; `None` once over.
    pub log: Option<(f32, f32)>,
    /// Some entrance, the log's or a row's, is still running: the log asks for another frame.
    pub running: bool,
    /// The log mounted anew this frame: what lives in its rows (cards, chips, voters) enters again too.
    pub remounted: bool,
}

impl LogMotion {
    /// Nothing enters (reduced motion, or the end state).
    pub const STILL: Self = Self {
        log: None,
        running: false,
        remounted: false,
    };
}

/// MessageLog.tsx's first-paint stagger: the log's last [`STAGGER_ROWS`] rows take `--d-enter-i` 0, 1, 2... in order, each
/// delayed by that many `--duration-stagger`; the rows before them, and every row that arrives later, have none.
pub fn stagger(keys: &[String], first_paint: bool) -> Vec<(String, Duration)> {
    let from = keys.len().saturating_sub(STAGGER_ROWS);
    keys.iter()
        .enumerate()
        .map(|(n, key)| {
            let delay = if first_paint && n >= from {
                dilla_desktop_ui::tokens::motion::STAGGER * (n - from) as u32
            } else {
                Duration::ZERO
            };
            (key.clone(), delay)
        })
        .collect()
}

impl<K: Copy + PartialEq> LogEntrances<K> {
    /// The log is drawn at `now` for `key` with the rows `order`: a new key mounts it anew; every row new to the log enters
    /// now, the first paint staggered ([`stagger`]). With `reduced` (or the web's zeroed durations) nothing enters.
    pub fn enter(&mut self, key: K, order: &[String], now: Instant, reduced: bool) -> LogMotion {
        use dilla_desktop_ui::motion;
        use dilla_desktop_ui::tokens::motion as token;
        // Reduced motion draws every entrance finished; the rows and the mount are still taken in as long past, so that
        // turning motion on later plays nothing back.
        let long_ago = now.checked_sub(Duration::from_secs(3600)).unwrap_or(now);
        let (at, fresh) = if reduced {
            (long_ago, false)
        } else {
            (now, true)
        };
        let remounted = self.mount.is_none_or(|m| m.key != key);
        if remounted {
            self.mount = Some(LogMount { key, at, fresh });
            self.entrances.clear();
        }
        let mount = self.mount.as_mut().expect("just mounted");
        let first_paint = std::mem::take(&mut mount.fresh);
        let seen = if reduced { long_ago } else { now };
        let rows = self
            .entrances
            .sync(stagger(order, first_paint), seen, token::ENTER);
        if reduced {
            return LogMotion {
                remounted,
                ..LogMotion::STILL
            };
        }
        let since = now.saturating_duration_since(mount.at);
        let log_over = since >= token::SWITCH.max(token::SWITCH_RISE);
        LogMotion {
            log: (!log_over).then(|| motion::log_at(since)),
            running: rows || !log_over,
            remounted,
        }
    }

    /// Whether drawing the log for `key` mounts it anew (the first draw, another key, or after [`Self::unmount`]).
    pub fn remounts(&self, key: K) -> bool {
        self.mount.is_none_or(|m| m.key != key)
    }

    /// How long `key` (a row) has been entering at `now`, and its delay.
    pub fn of(&self, key: &str, now: Instant) -> Option<(Duration, Duration)> {
        self.entrances.of(&key.to_owned(), now)
    }

    /// The log is not on screen (a stage took its place, the pane closed): the web unmounts it, so the next draw mounts it
    /// anew.
    pub fn unmount(&mut self) {
        self.mount = None;
        self.entrances.clear();
    }
}

/// The window's message state, created on first use. Notifying it redraws the view that drew the timeline.
pub fn ui(window: &mut Window, cx: &mut App) -> Entity<MessageUi> {
    let e = window.use_keyed_state("messages-ui", cx, |_, _| MessageUi::default());
    let id = window.window_handle().window_id();
    if crate::per_window::get::<UiHandle>(id, cx).is_none_or(|h| h.0.entity_id() != e.entity_id()) {
        crate::per_window::set(id, Some(UiHandle(e.clone())), cx);
    }
    e
}

/// Tests: `window`'s message state, made outside a frame.
#[cfg(test)]
pub(crate) fn ui_for_test(window: gpui::WindowId, cx: &mut App) -> Entity<MessageUi> {
    let e = cx.new(|_| MessageUi::default());
    crate::per_window::set(window, Some(UiHandle(e.clone())), cx);
    e
}

/// Each window's message state (final review sweep: per window; release walks them all).
#[derive(Clone)]
struct UiHandle(Entity<MessageUi>);

/// Lets go of `client` (Forget, sign-out: `crate::release::release_client`): the context the dialogs read and an open
/// editor that holds it go.
pub fn release(client: &Rc<Client>, cx: &mut App) {
    let mut handles = Vec::new();
    crate::per_window::each_mut::<UiHandle>(cx, |_, h| handles.push(h.clone()));
    let holds = |c: &Option<Rc<Client>>| c.as_ref().is_some_and(|c| Rc::ptr_eq(c, client));
    for handle in handles {
        handle.0.update(cx, |ui, cx| {
            if ui.ctx.as_ref().is_some_and(|ctx| holds(&ctx.client)) {
                ui.ctx = None;
                ui.pins = None;
                ui.deleting = None;
                ui.menu = None;
            }
            if ui.moderating.as_ref().is_some_and(|m| holds(&m.client)) {
                ui.moderating = None;
            }
            if ui.editing.as_ref().is_some_and(|e| holds(&e.client)) {
                ui.editing = None;
            }
            cx.notify();
        });
    }
}

/// The state of `window`'s log that a closure of an action reaches (`None` before its timeline's first frame).
pub fn handle(window: &Window, cx: &App) -> Option<Entity<MessageUi>> {
    crate::per_window::get::<UiHandle>(window.window_handle().window_id(), cx).map(|h| h.0.clone())
}

impl MessageUi {
    /// Whether the row `key` is the flashing one (a jump to it that has not run its course).
    pub fn flashing(&self, key: &str) -> bool {
        self.flash
            .as_ref()
            .is_some_and(|(k, at)| k == key && at.is_none_or(|at| at.elapsed() < FLASH))
    }

    /// The row `key` being drawn: which jump's flash it shows (`flash_seq`), if it is the flashing row and the flash is not
    /// over. The flash starts at the row's first draw; one redraw at its end takes the tint off (reduced motion has no
    /// frames of its own).
    pub fn flash_of(&mut self, key: &str, cx: &mut Context<Self>) -> Option<u64> {
        let (_, at) = self.flash.as_mut().filter(|(k, _)| k == key)?;
        if at.is_none() {
            *at = Some(Instant::now());
            cx.spawn(async move |this, cx| {
                cx.background_executor().timer(FLASH).await;
                let _ = this.update(cx, |_, cx| cx.notify());
            })
            .detach();
        }
        at.is_some_and(|at| at.elapsed() < FLASH)
            .then_some(self.flash_seq)
    }

    /// The channel's log is drawn at `now` for `key` (the channel and whether its timeline is read: a change of either mounts
    /// the log anew, as Conversation.tsx's `key={logKey}` does) with the rows `order`; see [`LogEntrances::enter`].
    pub fn enter_log(
        &mut self,
        key: (ChannelId, bool),
        order: &[String],
        now: Instant,
        reduced: bool,
    ) -> LogMotion {
        self.log.enter(key, order, now, reduced)
    }

    /// Starts editing `item`: the field holds the text as `@names`, focused with the caret at its end.
    pub fn begin_edit(
        &mut self,
        ctx: &SlotCx,
        item: &TimelineItem,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        // Only the own message can be edited (the instance refuses another's fold, and a foreign row must never show an editor).
        if !item.own {
            return;
        }
        let (Some(channel), Some(msg)) = (ctx.channel, item.msg_id) else {
            return;
        };
        // An edit is a send into the conversation (a type-1 fold through the core's `send_prepare`): while the trust gate
        // holds it, as it pauses the composer, no editor opens.
        if super::composing::trust_block(&ctx.state, channel).is_some() {
            return;
        }
        let client = ctx.client.clone();
        let state = &ctx.state;
        let community = state.channel(channel).map(|c| c.community_id);
        let members = community
            .and_then(|c| state.members.get(&c))
            .map(Vec::as_slice)
            .unwrap_or_default();
        // Conversation.tsx: an edit is encoded with the composer's rule (the channel's bits, the shell's rule while
        // unknown) and with the offered roles plus every role the message already names (`editRoles`).
        let (_, all_roles) = crate::screens::composer::mention_rights(state, Some(channel));
        let self_user = state.account.user_id.map(|u| u.hex());
        let text = editable_text(
            &item.body,
            members,
            self_user
                .as_deref()
                .map(|id| (id, state.account.username.as_str())),
            &all_roles,
        );
        let usernames: Vec<&str> = members.iter().map(|m| m.username.as_str()).collect();
        let (broadcast, roles) = edit_rights(state, channel, &item.body, &usernames);
        let input = cx.new(|cx| TextareaState::new(window, cx).submit_on_enter(true));
        input.update(cx, |s, cx| s.set_value(text, window, cx));
        let focus = input.read(cx).focus_handle(cx);
        window.focus(&focus, cx);
        let sub = cx.subscribe_in(&input, window, Self::on_edit_event);
        self.menu = None;
        self.editing = Some(Editing {
            key: item.key.clone(),
            channel,
            msg,
            own: item.own,
            input,
            client,
            window: window.window_handle().window_id(),
            members: members
                .iter()
                .filter(|m| !m.username.is_empty())
                .map(|m| MentionMember {
                    user_id: m.user_id.hex(),
                    username: m.username.clone(),
                })
                .collect(),
            usernames: usernames.iter().map(|u| (*u).to_owned()).collect(),
            body: item.body.clone(),
            roles,
            broadcast,
            _subscription: sub,
        });
        cx.notify();
    }

    fn on_edit_event(
        &mut self,
        input: &Entity<TextareaState>,
        event: &InputEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            InputEvent::Change => cx.notify(),
            InputEvent::PressEnter { shift: false, .. } => {
                // Enter during an IME composition confirms it and never saves (the web's `isComposing`).
                let composing = input
                    .update(cx, |s, cx| s.marked_text_range(window, cx))
                    .is_some();
                if !composing {
                    self.save_edit(cx);
                }
            }
            _ => {}
        }
    }

    /// Whether the edit may be saved: not blank, not over the budget, and not into a conversation the trust gate holds (the
    /// state the log was last drawn with: the gate may have closed since the editor opened).
    pub fn can_save(&self, cx: &App) -> bool {
        self.editing.as_ref().is_some_and(|e| {
            !e.input.read(cx).value().trim().is_empty()
                && e.used(cx) <= EDIT_MAX
                && !self
                    .ctx
                    .as_ref()
                    .is_some_and(|c| super::composing::trust_block(&c.state, e.channel).is_some())
        })
    }

    /// Saves the edit: the fold is queued and the editor closes (the row changes when the core folds it).
    pub fn save_edit(&mut self, cx: &mut Context<Self>) {
        // Own messages only (begin_edit refuses the rest; checked again here against the timeline the editor was opened on).
        if !self.can_save(cx) || !self.editing.as_ref().is_some_and(|e| e.own) {
            return;
        }
        let Some(e) = self.editing.take() else { return };
        let text = e.encoded(cx);
        if let Some(client) = &e.client {
            let done = client.messages().edit(e.channel, e.msg, text);
            let shown_in = Some(e.window);
            // A refused save is said, never dropped: the changed-key copy for the trust gate, else the code.
            cx.spawn(async move |_, cx| {
                let Some(text) = edit_refusal(done.await) else {
                    return;
                };
                cx.update(|cx| {
                    dilla_desktop_ui::components::toast::show_toast_in(shown_in, text, cx);
                });
            })
            .detach();
        }
        cx.notify();
    }

    /// Escape: the editor closes and nothing changes.
    pub fn cancel_edit(&mut self, cx: &mut Context<Self>) {
        if self.editing.take().is_some() {
            cx.notify();
        }
    }

    pub fn ask_delete(&mut self, channel: ChannelId, msg: MessageId, cx: &mut Context<Self>) {
        self.menu = None;
        self.deleting = Some(Deleting { channel, msg });
        cx.notify();
    }

    pub fn cancel_delete(&mut self, cx: &mut Context<Self>) {
        self.deleting = None;
        cx.notify();
    }

    /// Conversation.tsx `onRowEvent` `moderateRemove`: the dialog opens on `item` with an empty reason and the message's
    /// excerpt (its readable text, first 120 characters, `plain` being what the row shows).
    pub fn ask_moderate(
        &mut self,
        ctx: &SlotCx,
        item: &TimelineItem,
        plain: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let (Some(channel), Some(msg), Some(seq)) = (ctx.channel, item.msg_id, item.seq) else {
            return;
        };
        let Some(community) = ctx.state.channel(channel).map(|c| c.community_id) else {
            return;
        };
        // The dialog takes the focus when it opens (Dialog.tsx), not the field.
        let reason = cx.new(|cx| TextareaState::new(window, cx));
        self.menu = None;
        self.moderating = Some(Moderating {
            channel,
            community,
            msg,
            seq,
            excerpt: Zeroizing::new(super::mentions::excerpt(plain, REMOVAL_EXCERPT_CHARS)),
            sent_reason: Zeroizing::new(String::new()),
            reason,
            phase: ModPhase::Confirm,
            removal: None,
            proof: None,
            evidence: None,
            open: true,
            client: ctx.client.clone(),
        });
        cx.notify();
    }

    /// Conversation.tsx `closeModerateDialog`: the dialog closes; what is still unsettled stays as the status line.
    /// Nothing closes while the removal is being sent.
    pub fn close_moderate(&mut self, cx: &mut Context<Self>) {
        let Some(m) = self.moderating.as_mut() else {
            return;
        };
        if m.phase == ModPhase::Sending {
            return;
        }
        m.open = false;
        if m.phase == ModPhase::Confirm || m.settled() {
            self.moderating = None;
        }
        cx.notify();
    }

    /// Conversation.tsx `removeForEveryone`: sends the removal with the typed reason. A refused removal goes back to the
    /// reason; a failure goes back to it too and is shown (the shell's command error).
    pub fn remove_for_everyone(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(m) = self.moderating.as_mut() else {
            return;
        };
        let reason = m.reason_text(cx);
        if m.phase != ModPhase::Confirm || !removal_reason_ok(&reason) {
            return;
        }
        m.phase = ModPhase::Sending;
        m.sent_reason = reason.clone();
        let (channel, msg, seq) = (m.channel, m.msg, m.seq);
        let excerpt = m.excerpt.clone();
        let Some(client) = m.client.clone() else {
            // A fixture: nothing is sent.
            m.phase = ModPhase::Confirm;
            cx.notify();
            return;
        };
        let job = client.moderation().run(move |mo| async move {
            mo.remove_message(channel, msg, seq, &reason, &excerpt)
                .await
        });
        let shown_in = Some(window.window_handle().window_id());
        cx.spawn(async move |this, cx| {
            let answer = job.await;
            let _ = this.update(cx, |ui, cx| {
                let Some(m) = ui.moderating.as_mut().filter(|m| m.msg == msg) else {
                    return;
                };
                match answer {
                    Ok(outcome) if outcome.removal == Removal::Rejected => {
                        m.phase = ModPhase::Confirm;
                        m.removal = None;
                        m.proof = None;
                        m.evidence = None;
                    }
                    Ok(outcome) => m.take(outcome),
                    Err(e) => {
                        m.phase = ModPhase::Confirm;
                        let text = crate::strings::tf(
                            "shell.banner.commandError",
                            &[("code", e.code.as_str())],
                        );
                        dilla_desktop_ui::components::toast::show_toast_in(shown_in, text, cx);
                    }
                }
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    /// Conversation.tsx `retryModerationEvidence`: an unknown removal is checked or posted again with the same proof; an
    /// accepted one has its evidence sent again. The dialog opens again for it.
    pub fn retry_moderation(&mut self, cx: &mut Context<Self>) {
        let Some(m) = self.moderating.as_mut() else {
            return;
        };
        let (Some(proof), Some(client)) = (m.proof, m.client.clone()) else {
            return;
        };
        if m.phase == ModPhase::Sending {
            return;
        }
        m.open = true;
        m.phase = ModPhase::Sending;
        let msg = m.msg;
        let unknown = m.removal == Some(Removal::Unknown);
        let (community, reason, excerpt) = (m.community, m.sent_reason.clone(), m.excerpt.clone());
        let job = client.moderation().run(move |mo| async move {
            if unknown {
                // manual-removal.ts `resolveManualRemoval`: any failure is an unknown outcome.
                Ok(mo.resolve_removal(proof).await.unwrap_or(RemovalOutcome {
                    removal: Removal::Unknown,
                    proof_request_id: proof,
                    evidence_request_id: None,
                    evidence: Evidence::Unknown,
                }))
            } else {
                let (request, evidence) = mo
                    .send_removal_evidence(community, proof, &reason, &excerpt)
                    .await;
                Ok(RemovalOutcome {
                    removal: Removal::Accepted,
                    proof_request_id: proof,
                    evidence_request_id: Some(request),
                    evidence,
                })
            }
        });
        cx.spawn(async move |this, cx| {
            let answer = job.await;
            let _ = this.update(cx, |ui, cx| {
                let Some(m) = ui.moderating.as_mut().filter(|m| m.msg == msg) else {
                    return;
                };
                match answer {
                    Ok(outcome) if outcome.removal == Removal::Rejected => {
                        m.phase = ModPhase::Confirm;
                        m.removal = None;
                        m.proof = None;
                        m.evidence = None;
                    }
                    Ok(outcome) => m.take(outcome),
                    Err(_) => {
                        m.phase = ModPhase::Result;
                        m.evidence = Some(Evidence::Unknown);
                    }
                }
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    pub fn set_menu(&mut self, key: Option<String>, cx: &mut Context<Self>) {
        if self.menu != key {
            self.menu = key;
            cx.notify();
        }
    }

    pub fn open_pins(&mut self, channel: ChannelId, cx: &mut Context<Self>) {
        self.pins = Some(channel);
        cx.notify();
    }

    pub fn close_pins(&mut self, cx: &mut Context<Self>) {
        self.pins = None;
        cx.notify();
    }

    /// The notice `channel`'s conversation shows (`strings.rs` key), if any.
    pub fn notice_in(&self, channel: ChannelId) -> Option<&'static str> {
        self.notice.filter(|(c, _)| *c == channel).map(|(_, k)| k)
    }

    /// Conversation.tsx's jump effect: the message a jump aims at is not in `channel`'s loaded log, so the conversation
    /// says the original is further back (`shell.message.replyNotLoaded`) instead of jumping.
    pub fn not_loaded(&mut self, channel: ChannelId, cx: &mut Context<Self>) {
        self.notice = Some((channel, "shell.message.replyNotLoaded"));
        cx.notify();
    }

    /// The notice's Dismiss.
    pub fn dismiss_notice(&mut self, cx: &mut Context<Self>) {
        self.notice = None;
        cx.notify();
    }

    /// A jump to `key`: the log scrolls it into view and the row flashes the accent tint for 1.4 s.
    pub fn jump_to(&mut self, key: String, cx: &mut Context<Self>) {
        self.flash = Some((key.clone(), None));
        self.flash_seq += 1;
        self.jump = Some(key);
        cx.notify();
    }

    /// A search hit's row (Shell.tsx:1788-1793): it becomes the log's active row, and the log scrolls it to the middle of
    /// the viewport and gives it the focus on its next frame. The row does not flash.
    pub fn focus_row(&mut self, key: String, cx: &mut Context<Self>) {
        self.active = Some(key.clone());
        self.jump = Some(key);
        self.centre = true;
        cx.notify();
    }
}

/// Opens the pins dialog of `channel` (the channel header's pin button calls this).
pub fn open_pins(channel: ChannelId, client: Option<&Rc<Client>>, window: &Window, cx: &mut App) {
    if let Some(client) = client {
        let loaded = client.messages().load_pins(channel);
        cx.background_spawn(async move {
            let _ = loaded.await;
        })
        .detach();
    }
    if let Some(ui) = handle(window, cx) {
        ui.update(cx, |ui, cx| ui.open_pins(channel, cx));
    }
}

#[cfg(test)]
mod entrance_tests {
    use super::*;
    use dilla_desktop_ui::tokens::motion as token;

    fn keys(n: usize) -> Vec<String> {
        (0..n).map(|i| format!("k{i}")).collect()
    }

    /// MessageLog.tsx:82-83: only the last 20 rows of the first paint take `--d-enter-i` 0.., the rows before them none.
    #[test]
    fn the_first_paint_staggers_the_last_twenty_rows_40ms_apart() {
        let all = stagger(&keys(30), true);
        assert!(all[..10].iter().all(|(_, d)| d.is_zero()));
        assert_eq!(all[10].1, Duration::ZERO);
        assert_eq!(all[11].1, token::STAGGER);
        assert_eq!(all[29].1, token::STAGGER * 19);
        // A later paint (a row arriving, earlier rows loaded) staggers nothing.
        assert!(stagger(&keys(30), false).iter().all(|(_, d)| d.is_zero()));
        // Fewer than twenty rows: they take 0, 1, 2...
        let few = stagger(&keys(3), true);
        assert_eq!(few[2].1, token::STAGGER * 2);
    }

    /// The first paint's rows enter staggered; a row that arrives later enters at once; scrolling or a redraw never replays
    /// an entrance; another channel (or the timeline becoming read) mounts the log afresh; reduced motion draws nothing
    /// and plays nothing back when motion is turned on.
    #[test]
    fn rows_enter_once_and_a_remount_enters_them_again() {
        let mut ui = MessageUi::default();
        let (a, b) = (ChannelId([1; 16]), ChannelId([2; 16]));
        let t0 = Instant::now();
        let rows = keys(3);
        let first = ui.enter_log((a, true), &rows, t0, false);
        assert!(first.running && first.log.is_some());
        assert_eq!(ui.log.of("k2", t0).unwrap().1, token::STAGGER * 2);
        // A redraw much later: nothing runs, the log is over.
        let later = t0 + Duration::from_secs(5);
        assert_eq!(
            ui.enter_log((a, true), &rows, later, false),
            LogMotion::STILL
        );
        // A new row enters then, with no delay.
        let more = keys(4);
        let fresh = ui.enter_log((a, true), &more, later, false);
        assert!(fresh.running && fresh.log.is_none());
        assert_eq!(
            ui.log.of("k3", later),
            Some((Duration::ZERO, Duration::ZERO))
        );
        // Scrolling draws more or fewer rows of the log; it holds the same rows, so nothing enters again.
        let age = later + Duration::from_secs(5);
        ui.enter_log((a, true), &more, age, false);
        assert!(ui.log.of("k0", age).unwrap().0 >= Duration::from_secs(5));
        // Another channel, or the same one once it is read, mounts afresh and staggers again.
        ui.enter_log((b, false), &[], age, false);
        let again = ui.enter_log((b, true), &rows, age, false);
        assert!(again.running);
        assert_eq!(ui.log.of("k1", age).unwrap().1, token::STAGGER);
        // Reduced motion: still, and turning motion on afterwards plays nothing back.
        let mut ui = MessageUi::default();
        let reduced = ui.enter_log((a, true), &rows, t0, true);
        assert!(reduced.log.is_none() && !reduced.running);
        let on = ui.enter_log((a, true), &rows, t0 + Duration::from_millis(10), false);
        assert_eq!(on, LogMotion::STILL, "{on:?}");
    }

    /// A thread's log is the same machine keyed by the root message: it mounts per thread, staggers its first paint, enters a
    /// later reply at once, and mounts anew after the pane unmounts it (closed, or a stage took the channel log's place).
    #[test]
    fn a_log_mounts_per_key_and_anew_after_it_is_unmounted() {
        let (a, b) = (MessageId([1; 16]), MessageId([2; 16]));
        let mut log: LogEntrances<MessageId> = LogEntrances::default();
        let t0 = Instant::now();
        let rows = keys(3);
        assert!(log.remounts(a));
        let first = log.enter(a, &rows, t0, false);
        assert!(first.remounted && first.running && first.log.is_some());
        assert_eq!(log.of("k2", t0).unwrap().1, token::STAGGER * 2);
        let later = t0 + Duration::from_secs(5);
        let calm = log.enter(a, &rows, later, false);
        assert_eq!(calm, LogMotion::STILL);
        assert!(!log.remounts(a) && log.remounts(b));
        // The pane is closed and opened on the same thread: the log mounts anew and staggers again.
        log.unmount();
        assert!(log.remounts(a));
        let again = log.enter(a, &rows, later, false);
        assert!(again.remounted && again.running);
        assert_eq!(log.of("k1", later).unwrap().1, token::STAGGER);
    }
}

#[cfg(test)]
mod trust_tests {
    use super::*;
    use dilla_client::state::dms::{DM_KIND_PAIR, DmSummary};
    use dilla_client::state::trust::{SendBlock, TrustBlock, TrustSummary};
    use dilla_client::state::{AppState, ChannelGroupState, TimelineItemState, UserId};
    use std::sync::Arc;

    /// A DM with Mira; `held`: her identity changed and the core's gate holds the DM.
    fn dm(held: bool) -> (SlotCx, TimelineItem) {
        let (me, mira, channel) = (UserId([8; 16]), UserId([3; 16]), ChannelId([5; 16]));
        let mut s = AppState::default();
        s.account.user_id = Some(me);
        s.dms.list = Some(vec![DmSummary {
            id: channel,
            kind: DM_KIND_PAIR,
            members: vec![me, mira],
            name: "Mira".into(),
            group: ChannelGroupState::Active,
        }]);
        s.trust.loaded = true;
        if held {
            s.trust.contacts.push(TrustSummary {
                user_id: mira,
                name: "Mira".into(),
                shared: true,
                umk: "aa".repeat(32),
                first_seen: 1,
                verified: false,
                changed_umk: Some("bb".repeat(32)),
                unlisted: false,
                removed_in: Vec::new(),
                blocked: None,
            });
            s.trust.send_blocks.insert(
                channel,
                SendBlock {
                    code: TrustBlock::UmkChanged,
                    own: false,
                },
            );
        }
        let item = TimelineItem {
            key: "s1".into(),
            state: TimelineItemState::Ok,
            reason: String::new(),
            sender_user: Some(me),
            sender_device: None,
            own: true,
            bot: false,
            ts: 1,
            body: "on it after standup".into(),
            msg_id: Some(MessageId([1; 16])),
            seq: Some(1),
            edited: false,
            reply: None,
            reactions: Vec::new(),
            pinned: false,
            attachments: Vec::new(),
            mention: false,
            actions: Vec::new(),
        };
        let ctx = SlotCx {
            channel: Some(channel),
            state: Arc::new(s),
            ..SlotCx::default()
        };
        (ctx, item)
    }

    /// Task 2c fix round 1: an edit is a send into the conversation, so the trust gate that pauses the composer keeps the
    /// editor shut, and an editor opened before the gate closed cannot save (Save and Enter both run `save_edit`). The
    /// control: the open DM edits and saves.
    #[gpui::test]
    fn a_changed_member_keeps_the_editor_from_opening_and_saving(cx: &mut gpui::TestAppContext) {
        cx.update(crate::install);
        let window = cx.add_empty_window();
        let (held, item) = dm(true);
        let (open, _) = dm(false);
        window.update(|window, cx| {
            let ui = ui_for_test(window.window_handle().window_id(), cx);
            ui.update(cx, |ui, cx| {
                ui.ctx = Some(held.clone());
                ui.begin_edit(&held, &item, window, cx);
                assert!(ui.editing.is_none(), "no editor while the gate holds");
                ui.ctx = Some(open.clone());
                ui.begin_edit(&open, &item, window, cx);
                assert!(ui.editing.is_some(), "the open DM edits");
                assert!(ui.can_save(cx));
                // The gate closes while the editor is open.
                ui.ctx = Some(held.clone());
                assert!(!ui.can_save(cx), "no save while the gate holds");
                ui.save_edit(cx);
                assert!(ui.editing.is_some(), "the edit was not taken");
                ui.ctx = Some(open.clone());
                ui.save_edit(cx);
                assert!(ui.editing.is_none(), "the open DM saves");
            });
        });
    }

    /// A refused save is said, never dropped: the changed-key copy for the trust gate, the code otherwise.
    #[test]
    fn a_refused_edit_says_why() {
        use dilla_client::ClientError;
        assert_eq!(edit_refusal::<()>(Ok(Ok(()))), None);
        assert_eq!(
            edit_refusal::<()>(Ok(Err(ClientError::Sync("E_UMK_CHANGED".into())))).as_deref(),
            Some(crate::strings::t("privacy.changed"))
        );
        assert_eq!(
            edit_refusal::<()>(Ok(Err(ClientError::Sync("E_CREDENTIAL".into())))),
            Some(crate::strings::tf(
                "shell.banner.commandError",
                &[("code", "E_CREDENTIAL")]
            ))
        );
        assert_eq!(
            edit_refusal(Err(())),
            Some(crate::strings::tf(
                "shell.banner.commandError",
                &[("code", "E_STOPPED")]
            ))
        );
    }
}
