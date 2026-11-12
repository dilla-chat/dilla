//! The "Dilla - your instances" window's content (`packages/desktop/src/manager.{html,css,ts}`): the brand, the heading,
//! a field to connect by address, a field to join by a pasted invite, the list of instances and the footer.
//!
//! Sizes are the CSS's rem values (`rem_size`), never literal pixels. Keyboard: Tab runs address field, Connect,
//! invite field, Join, then the list, which is one tab stop that Up, Down, Home and End rove; Enter opens the focused
//! instance and Delete removes it after a confirmation.

use std::rc::Rc;

use dilla_client::instances::{InstanceError, Instances};
use dilla_client::seams::{InstanceConfig, InstanceId};
use gpui::prelude::FluentBuilder;
use gpui::{
    App, AppContext as _, ClickEvent, Context, Entity, EventEmitter, FocusHandle, Focusable,
    InteractiveElement, IntoElement, KeyDownEvent, ParentElement, Render, SharedString,
    StatefulInteractiveElement, Styled, Subscription, Window, div,
};
use gpui_component::input::{InputEvent, InputState};

use dilla_desktop_ui::components::button::{Button, ButtonVariant};
use dilla_desktop_ui::components::confirm_dialog::{ConfirmDialog, Tone};
use dilla_desktop_ui::components::text_field::TextField;
use dilla_desktop_ui::metrics::focus_ring;
use dilla_desktop_ui::roving::Roving;
use dilla_desktop_ui::text::{line, line_height, paragraph};
use dilla_desktop_ui::tokens::{color, font, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use super::copy;
use super::model::{Connect, Entry, Manager, Refusal, label_of};
use crate::screens::{FocusNext, FocusPrevious};
use crate::tracked_box;

/// What the window asks of its host.
pub enum ManagerEvent {
    /// Open this instance's window; `invite` is the code of a pasted invite.
    Open {
        instance: InstanceConfig,
        invite: Option<String>,
    },
}

type OnInstance = Rc<dyn Fn(InstanceId, &mut App)>;
type InstanceHeld = Rc<dyn Fn(InstanceId, &App) -> bool>;
type InstanceStopped = Rc<dyn Fn(InstanceId, &mut App) -> bool>;

/// What the manager asks of the window host (`crate::windows`), kept as closures so the view runs without one.
#[derive(Clone)]
pub struct Host {
    /// Closes the instance's window and drops its client; its hold stays until the client has stopped.
    pub release: OnInstance,
    /// Whether this process holds the instance (a window or client of it exists, or one is still stopping).
    pub held_here: InstanceHeld,
    /// Whether a released instance has really stopped, so its data can go.
    pub stopped: InstanceStopped,
}

/// How many times a removal looks again for a released client to stop, before it gives up with "in use".
const STOP_ATTEMPTS: u32 = 200;

enum Confirm {
    /// The instance and the server the person saw in the list: an id reused for another server is not removed.
    Remove(InstanceId, url::Url),
    /// A pasted invite naming an instance not listed yet: ask before adding it.
    NewInstance { input: String, host: String },
}

pub struct ManagerView {
    model: Result<Manager, SharedString>,
    url: Entity<InputState>,
    invite: Entity<InputState>,
    error: Option<SharedString>,
    confirm: Option<Confirm>,
    /// The list's tab stop: the instance that last had focus.
    stop: usize,
    /// One focus handle per listed instance, grown as the list does (made while drawing).
    row_focus: Vec<FocusHandle>,
    host: Host,
    /// A removal is running (its keychain and disk work is off the UI thread).
    busy: bool,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<ManagerEvent> for ManagerView {}

impl ManagerView {
    pub fn new(
        model: Result<Manager, dilla_client::store::StoreError>,
        host: Host,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let field = |placeholder: &'static str, window: &mut Window, cx: &mut Context<Self>| {
            cx.new(|cx| InputState::new(window, cx).placeholder(placeholder))
        };
        let url = field(copy::ADDRESS_PLACEHOLDER, window, cx);
        let invite = field(copy::INVITE_PLACEHOLDER, window, cx);
        let subscriptions = vec![
            cx.subscribe_in(
                &url,
                window,
                |this: &mut Self, _, event: &InputEvent, window, cx| {
                    if matches!(event, InputEvent::PressEnter { .. }) {
                        this.connect_url(window, cx);
                    }
                },
            ),
            cx.subscribe_in(
                &invite,
                window,
                |this: &mut Self, _, event: &InputEvent, window, cx| {
                    if matches!(event, InputEvent::PressEnter { .. }) {
                        this.connect_invite(window, cx);
                    }
                },
            ),
        ];
        url.update(cx, |field, cx| field.focus(window, cx));
        Self {
            model: model.map_err(|_| SharedString::from(copy::ERROR_STORE)),
            url,
            invite,
            error: None,
            confirm: None,
            stop: 0,
            row_focus: Vec::new(),
            host,
            busy: false,
            _subscriptions: subscriptions,
        }
    }

    /// Types `text` into the address field (as a person would, replacing what is there).
    pub fn set_address(&mut self, text: &str, window: &mut Window, cx: &mut Context<Self>) {
        self.url
            .update(cx, |f, cx| f.set_value(text.to_owned(), window, cx));
    }

    /// Types `text` into the invite field.
    pub fn set_invite(&mut self, text: &str, window: &mut Window, cx: &mut Context<Self>) {
        self.invite
            .update(cx, |f, cx| f.set_value(text.to_owned(), window, cx));
    }

    /// The focus handles of the two fields, in tab order (address, invite).
    pub fn field_focus(&self, cx: &App) -> [FocusHandle; 2] {
        [self.url.focus_handle(cx), self.invite.focus_handle(cx)]
    }

    /// The instances as drawn.
    pub fn entries(&self) -> Vec<Entry> {
        self.model
            .as_ref()
            .map(Manager::entries)
            .unwrap_or_default()
    }

    /// The refusal on screen, if any.
    pub fn error(&self) -> Option<&str> {
        self.error.as_deref()
    }

    /// Whether a confirmation dialog is open.
    pub fn confirming(&self) -> bool {
        self.confirm.is_some()
    }

    fn fail(&mut self, refusal: &Refusal, cx: &mut Context<Self>) {
        self.error = Some(refusal.message().into());
        cx.notify();
    }

    fn opened(&mut self, connect: Connect, window: &mut Window, cx: &mut Context<Self>) {
        self.error = None;
        let _ = window;
        cx.emit(ManagerEvent::Open {
            instance: connect.instance,
            invite: connect.invite,
        });
        cx.notify();
    }

    /// Connect by the address in the field.
    pub fn connect_url(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let input = self.url.read(cx).value().to_string();
        let Ok(model) = self.model.as_mut() else {
            return;
        };
        match model.connect_url(&input) {
            Ok(connect) => {
                self.url.update(cx, |f, cx| f.set_value("", window, cx));
                self.opened(connect, window, cx);
            }
            Err(e) => self.fail(&e, cx),
        }
    }

    /// Join by the invite in the field; an instance not listed yet is confirmed first.
    pub fn connect_invite(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let input = self.invite.read(cx).value().to_string();
        let Ok(model) = self.model.as_mut() else {
            return;
        };
        match model.invite_names_new_instance(&input) {
            Ok(true) => {
                let host = dilla_client::instances::address::parse_invite(input.trim())
                    .map(|i| label_of(&i.instance))
                    .unwrap_or_default();
                self.error = None;
                self.confirm = Some(Confirm::NewInstance { input, host });
                cx.notify();
            }
            Ok(false) => self.finish_invite(&input, window, cx),
            Err(e) => self.fail(&e, cx),
        }
    }

    fn finish_invite(&mut self, input: &str, window: &mut Window, cx: &mut Context<Self>) {
        let Ok(model) = self.model.as_mut() else {
            return;
        };
        match model.connect_invite(input) {
            Ok(connect) => {
                self.invite.update(cx, |f, cx| f.set_value("", window, cx));
                self.opened(connect, window, cx);
            }
            Err(e) => self.fail(&e, cx),
        }
    }

    /// Opens the instance at `index` of the list. The row is taken before the list is re-read, and opened only if the
    /// same instance (id and server) is still listed after it: an id removed and reused for another server meanwhile
    /// opens nothing.
    pub fn open_at(&mut self, index: usize, window: &mut Window, cx: &mut Context<Self>) {
        let Ok(model) = self.model.as_mut() else {
            return;
        };
        let Some(seen) = model.entries().get(index).cloned() else {
            return;
        };
        if let Err(e) = model.refresh() {
            return self.fail(&Refusal::Store(e), cx);
        }
        match model.instances().get(seen.id).cloned() {
            Some(cfg) if cfg.server == seen.server => {
                self.opened(
                    Connect {
                        instance: cfg,
                        invite: None,
                    },
                    window,
                    cx,
                );
            }
            _ => self.fail(&Refusal::Changed, cx),
        }
    }

    /// Asks to remove the instance at `index`.
    pub fn ask_remove(&mut self, index: usize, cx: &mut Context<Self>) {
        if self.busy {
            return;
        }
        if let Some(entry) = self.entries().get(index) {
            self.error = None;
            self.confirm = Some(Confirm::Remove(entry.id, entry.server.clone()));
            cx.notify();
        }
    }

    pub fn confirm(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        match self.confirm.take() {
            Some(Confirm::Remove(id, server)) => self.start_removal(id, server, window, cx),
            Some(Confirm::NewInstance { input, .. }) => self.finish_invite(&input, window, cx),
            None => {}
        }
        cx.notify();
    }

    /// Removal, in steps, so a refusal changes nothing and nothing waits on the UI thread:
    /// 1. off the UI thread, the key is deleted from the keychain, but only if `id` is still listed for `server`;
    /// 2. only then the instance's window and client are released (a refused keychain closes nothing);
    /// 3. once the client has really stopped, off the UI thread again, the data dir and the entry go.
    fn start_removal(
        &mut self,
        id: InstanceId,
        server: url::Url,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Ok(model) = self.model.as_ref() else {
            return;
        };
        let (root, kc) = (model.root().to_path_buf(), model.keychain().clone());
        let hold = !(self.host.held_here)(id, cx);
        self.busy = true;
        self.error = None;
        let expected = server.clone();
        let task = cx.background_spawn(async move {
            Instances::load(&root)
                .map_err(InstanceError::from)
                .and_then(|mut list| list.erase_key(id, &expected, kc.as_ref(), hold))
        });
        cx.spawn_in(window, async move |this, cx| {
            let result = task.await;
            this.update_in(cx, |this, window, cx| {
                this.erased(id, server, result, window, cx);
            })
            .ok();
        })
        .detach();
    }

    fn erased(
        &mut self,
        id: InstanceId,
        server: url::Url,
        result: Result<Option<dilla_client::seams::InstanceConfig>, InstanceError>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match result {
            Ok(Some(_)) => {
                (self.host.release)(id, cx);
                self.await_stop(id, server, 0, window, cx);
            }
            // Gone already: nothing to erase.
            Ok(None) => self.removed(Ok(()), window, cx),
            Err(e) => self.removed(Err(Refusal::from(e)), window, cx),
        }
    }

    /// Waits (a few turns of the event loop) until the released client has stopped, then deletes the data.
    fn await_stop(
        &mut self,
        id: InstanceId,
        server: url::Url,
        attempt: u32,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if (self.host.stopped)(id, cx) {
            let Ok(model) = self.model.as_ref() else {
                return;
            };
            let root = model.root().to_path_buf();
            let task = cx.background_spawn(async move {
                Instances::load(&root)
                    .map_err(InstanceError::from)
                    .and_then(|mut list| list.remove_verified(id, &server, true))
            });
            cx.spawn_in(window, async move |this, cx| {
                let result = task.await.map(|_| ()).map_err(Refusal::from);
                this.update_in(cx, |this, window, cx| this.removed(result, window, cx))
                    .ok();
            })
            .detach();
        } else if attempt < STOP_ATTEMPTS {
            let this = cx.entity().downgrade();
            window.defer(cx, move |window, cx| {
                this.update(cx, |this, cx| {
                    this.await_stop(id, server, attempt + 1, window, cx);
                })
                .ok();
            });
        } else {
            self.removed(Err(Refusal::InUse), window, cx);
        }
    }

    fn removed(
        &mut self,
        result: Result<(), Refusal>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.busy = false;
        self.error = result.err().map(|e| e.message().into());
        if let Ok(model) = self.model.as_mut() {
            let _ = model.refresh();
        }
        let len = self.entries().len();
        self.stop = self.stop.min(len.saturating_sub(1));
        self.focus_row(self.stop, window, cx);
        cx.notify();
    }

    /// Shows a refusal from the host (an instance that is open in another process).
    pub fn show_error(&mut self, message: String, cx: &mut Context<Self>) {
        self.error = Some(message.into());
        cx.notify();
    }

    pub fn cancel_confirmation(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let was_remove = matches!(self.confirm, Some(Confirm::Remove(..)));
        self.confirm = None;
        if was_remove {
            self.focus_row(self.stop, window, cx);
        }
        cx.notify();
    }

    fn focus_row(&self, index: usize, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(handle) = self.row_focus.get(index) {
            handle.focus(window, cx);
        } else {
            self.url.read(cx).focus_handle(cx).focus(window, cx);
        }
    }

    fn row_key(
        &mut self,
        index: usize,
        event: &KeyDownEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let key = &event.keystroke;
        let len = self.entries().len();
        if let Some(next) = Roving::List.next(key, index, len) {
            self.stop = next;
            self.focus_row(next, window, cx);
            cx.stop_propagation();
        } else if key.key == "enter" {
            self.open_at(index, window, cx);
            cx.stop_propagation();
        } else if key.key == "delete" {
            self.ask_remove(index, cx);
            cx.stop_propagation();
        }
    }
}

impl Render for ManagerView {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let rem = window.rem_size();
        let entries = self.entries();
        while self.row_focus.len() < entries.len() {
            self.row_focus.push(cx.focus_handle());
        }
        let handles = self.row_focus.clone();
        let stop = Roving::stop(Some(self.stop), None, entries.len());
        // main: `max-width: 38rem; padding: 0 2rem`, the lead wraps in what is left of it.
        let content = (window.viewport_size().width - rem * 4.0).min(rem * 38.0);
        let content_px = f32::from(content);

        let lead_size = text::MD;
        let lead = paragraph(
            copy::LEAD,
            lead_size,
            line_height(lead_size, 1.6),
            color::FG_2,
            content_px,
        );

        let url_field = div().flex_1().min_w_0().child(tracked_box(
            "instances-url",
            TextField::new("instances-url-field", copy::ADDRESS_LABEL, &self.url),
        ));
        let connect = Button::new(
            "instances-connect",
            if self.busy {
                copy::CONNECT_BUSY
            } else {
                copy::CONNECT
            },
        )
        .disabled(self.busy)
        .variant(ButtonVariant::Accent)
        .on_click(cx.listener(|this, _: &ClickEvent, window, cx| this.connect_url(window, cx)));
        let invite_field = div().flex_1().min_w_0().child(tracked_box(
            "instances-invite",
            TextField::new("instances-invite-field", copy::INVITE_LABEL, &self.invite)
                .hint(copy::INVITE_HINT),
        ));
        let join = Button::new("instances-join", copy::INVITE_CONNECT)
            .disabled(self.busy)
            .on_click(
                cx.listener(|this, _: &ClickEvent, window, cx| this.connect_invite(window, cx)),
            );

        let rows = entries.iter().enumerate().map(|(index, entry)| {
            let handle: FocusHandle = handles[index].clone().tab_stop(index == stop);
            let focused = handle.is_focused(window);
            div()
                .id(("instance-row", index))
                .track_focus(&handle)
                .key_context("InstanceRow")
                .flex()
                .items_center()
                .gap_3()
                .w_full()
                .px_3()
                .py_2()
                .rounded(radius::MD)
                .bg(color::SURFACE_2)
                .border_1()
                .border_color(color::CONTROL_EDGE_FIELD)
                .when(focused, |row| row.shadow(focus_ring()))
                .hover(|row| row.bg(color::HOVER))
                .cursor_pointer()
                .on_key_down(cx.listener(move |this, event: &KeyDownEvent, window, cx| {
                    this.row_key(index, event, window, cx);
                }))
                .on_click(cx.listener(move |this, _: &ClickEvent, window, cx| {
                    this.stop = index;
                    this.open_at(index, window, cx);
                }))
                .child(
                    div().flex_1().min_w_0().child(
                        line(
                            entry.label.clone(),
                            text::MD,
                            line_height(text::MD, 1.6),
                            color::FG,
                        )
                        .ellipsis(),
                    ),
                )
                .child(
                    div()
                        .id(("instance-remove", index))
                        .flex_none()
                        .flex()
                        .items_center()
                        .gap_1()
                        .px_2()
                        .rounded(radius::SM)
                        .text_color(color::FG_3)
                        .hover(|d| d.text_color(color::DANGER).bg(color::DANGER_SOFT))
                        .on_click(cx.listener(move |this, _: &ClickEvent, _, cx| {
                            cx.stop_propagation();
                            this.stop = index;
                            this.ask_remove(index, cx);
                        }))
                        .child(
                            Icon::new(IconName::Trash2)
                                .size(text::BASE)
                                .color(color::FG_3),
                        )
                        .child(line(
                            copy::REMOVE,
                            text::XS,
                            line_height(text::XS, 1.6),
                            color::FG_3,
                        )),
                )
        });

        let list: gpui::AnyElement = if entries.is_empty() {
            line(
                copy::EMPTY,
                text::MD,
                line_height(text::MD, 1.6),
                color::FG_2,
            )
            .into_any_element()
        } else {
            div()
                .id("instances-list")
                .flex()
                .flex_col()
                .gap_2()
                .children(rows)
                .into_any_element()
        };

        let footer = paragraph(
            copy::FOOTER,
            text::SM,
            line_height(text::SM, 1.6),
            color::FG_2,
            content_px,
        );

        let main = div()
            .w(content)
            .flex()
            .flex_col()
            .child(
                div()
                    .flex()
                    .items_baseline()
                    .gap_4()
                    .child(
                        line(
                            copy::BRAND,
                            rem * 2.0,
                            f32::from(rem * 2.0 * 1.6),
                            color::ACCENT,
                        )
                        .weight(font::WEIGHT_DISPLAY)
                        .tracking(-0.05),
                    )
                    .child(line(
                        copy::BRAND_NOTE,
                        rem * 0.8,
                        f32::from(rem * 0.8 * 1.6),
                        color::FG_2,
                    )),
            )
            .child(div().mt_8().mb_4().flex().flex_col().children(
                [copy::HEADING_1, copy::HEADING_2].map(|h| {
                    line(h, rem * 2.8, f32::from(rem * 2.8 * 1.1), color::FG)
                        .weight(font::WEIGHT_DISPLAY)
                        .tracking(-0.04)
                }),
            ))
            .child(div().flex().flex_col().children(lead))
            .child(
                div()
                    .mt_8()
                    .flex()
                    .items_end()
                    .gap_3()
                    .child(url_field)
                    .child(tracked_box("instances-connect-button", connect)),
            )
            .child(
                div()
                    .mt_4()
                    .flex()
                    .items_end()
                    .gap_3()
                    .child(invite_field)
                    .child(tracked_box("instances-join-button", join)),
            )
            .child(
                div()
                    .id("instances-error")
                    .min_h(rem * 1.6)
                    .mt_2()
                    .when_some(self.error.clone(), |d, e| {
                        d.child(line(e, text::MD, line_height(text::MD, 1.6), color::DANGER))
                    }),
            )
            .child(
                div()
                    .mt_8()
                    .pt_4()
                    .border_t_1()
                    .border_color(color::HAIRLINE_2)
                    .flex()
                    .flex_col()
                    .gap_3()
                    .child(
                        line(copy::LIST_TITLE, rem, f32::from(rem * 1.6), color::FG)
                            .weight(font::WEIGHT_LABEL),
                    )
                    .child(list),
            )
            .child(
                div()
                    .mt_8()
                    .pt_4()
                    .border_t_1()
                    .border_color(color::HAIRLINE_2)
                    .flex()
                    .flex_col()
                    .children(footer),
            );

        let dialog = self.confirm.as_ref().map(|confirm| {
            let weak = cx.entity().downgrade();
            let (title, body, confirm_label, tone) = match confirm {
                Confirm::Remove(_, server) => (
                    copy::REMOVE_TITLE,
                    copy::REMOVE_BODY.replace("{host}", &label_of(server)),
                    copy::REMOVE_CONFIRM,
                    Tone::Danger,
                ),
                Confirm::NewInstance { host, .. } => (
                    copy::NEW_TITLE,
                    copy::NEW_BODY.replace("{host}", host),
                    copy::CONNECT,
                    Tone::Accent,
                ),
            };
            let on_close = weak.clone();
            ConfirmDialog::new(
                "instances-confirm",
                title,
                body,
                confirm_label,
                copy::REMOVE_BUSY,
                copy::CANCEL,
            )
            .tone(tone)
            .on_confirm(move |window, cx| {
                weak.update(cx, |this, cx| this.confirm(window, cx)).ok();
            })
            .on_close(move |window, cx| {
                on_close
                    .update(cx, |this, cx| this.cancel_confirmation(window, cx))
                    .ok();
            })
        });

        div()
            .id("instances")
            .key_context("Instances")
            .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
            .on_action(|_: &FocusPrevious, window, cx| window.focus_prev(cx))
            .size_full()
            .relative()
            .bg(color::BG)
            .child(
                div()
                    .id("instances-scroll")
                    .size_full()
                    .overflow_y_scroll()
                    .flex()
                    .justify_center()
                    .py_12()
                    .child(main),
            )
            .children(dialog)
    }
}
