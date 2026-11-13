//! How each section of the server settings draws (the `*_section` methods of [`Body`]) and the dialogs it opens.

use dilla_client::features::server_admin::{AFK_TIMEOUTS, Direction, SLOW_STEPS, is_private, nfc};
use dilla_client::state::Channel;
use dilla_client::state::server_admin::GifRating;
use gpui::{
    AnyElement, App, ClickEvent, Context, InteractiveElement, IntoElement, ParentElement,
    SharedString, StatefulInteractiveElement, Styled, Window, div, prelude::FluentBuilder as _,
};

use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::button::{Button, ButtonSize, ButtonVariant};
use dilla_desktop_ui::components::confirm_dialog::{ConfirmDialog, Tone};
use dilla_desktop_ui::components::dialog::{Dialog, DialogKind};
use dilla_desktop_ui::components::icon_button::{IconButton, IconButtonSize};
use dilla_desktop_ui::components::segmented::Segmented;
use dilla_desktop_ui::components::select::Select;
use dilla_desktop_ui::components::server_settings::layout;
use dilla_desktop_ui::components::server_settings::permission_grid::{
    GridGroup, GridRow, PermissionGrid,
};
use dilla_desktop_ui::components::settings::frame::{section_header, section_header_full};
use dilla_desktop_ui::components::settings::row::{GroupPosition, Layout, SettingsRow};
use dilla_desktop_ui::components::text_field::TextField;
use dilla_desktop_ui::components::toggle::Toggle;
use dilla_desktop_ui::components::trailing_slot::TrailingSlot;
use dilla_desktop_ui::snap::{edge_box, snapped};
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, font, radius, text};

use super::{Body, Dialogue};
use crate::router::ServerSection;
use crate::screens::server_settings::model;
use crate::strings::{t, tf};

/// The automation id of an element the screen makes one of per row: interned, so a name keeps one `&'static str`.
fn track_id(id: String) -> &'static str {
    use std::cell::RefCell;
    use std::collections::HashMap;
    thread_local! {
        static IDS: RefCell<HashMap<String, &'static str>> = RefCell::new(HashMap::new());
    }
    IDS.with(|ids| {
        let mut ids = ids.borrow_mut();
        if let Some(known) = ids.get(&id) {
            return *known;
        }
        let leaked: &'static str = Box::leak(id.clone().into_boxed_str());
        ids.insert(id, leaked);
        leaked
    })
}

/// A handler of the settings' own entity, for the components that take a plain closure.
fn act(
    cx: &Context<Body>,
    f: impl Fn(&mut Body, &mut Window, &mut Context<Body>) + 'static,
) -> impl Fn(&mut Window, &mut App) + 'static {
    let view = cx.entity().downgrade();
    move |window, app| {
        let _ = view.update(app, |this, cx| f(this, window, cx));
    }
}

/// [`act`] for a click.
fn act_click(
    cx: &Context<Body>,
    f: impl Fn(&mut Body, &mut Window, &mut Context<Body>) + 'static,
) -> impl Fn(&ClickEvent, &mut Window, &mut App) + 'static {
    let view = cx.entity().downgrade();
    move |_, window, app| {
        let _ = view.update(app, |this, cx| f(this, window, cx));
    }
}

fn dim(text_: impl Into<SharedString>) -> AnyElement {
    div()
        .child(line(
            text_,
            text::UI,
            line_height(text::UI, 1.5),
            color::FG_2,
        ))
        .into_any_element()
}

fn alert(text_: impl Into<SharedString>) -> AnyElement {
    div()
        .child(line(
            text_,
            text::SM,
            line_height(text::SM, 1.5),
            color::DANGER,
        ))
        .into_any_element()
}

/// A small accent chip after a channel's name (`.dw-admin-tag`).
fn tag(label: &str) -> AnyElement {
    div()
        .flex_none()
        .px(layout::TAG_PAD_X)
        .rounded(radius::TAG)
        .bg(color::ACCENT_CHIP)
        .child(
            line(
                label.to_uppercase(),
                text::CHIP,
                line_height(text::CHIP, 1.4),
                color::ACCENT_2,
            )
            .weight(font::WEIGHT_LABEL),
        )
        .into_any_element()
}

fn slow_label(seconds: u32) -> &'static str {
    t(match seconds {
        0 => "slow.off",
        5 => "slow.s5",
        10 => "slow.s10",
        30 => "slow.s30",
        60 => "slow.m1",
        300 => "slow.m5",
        900 => "slow.m15",
        3600 => "slow.h1",
        _ => "slow.h6",
    })
}

fn shown_name(m: &dilla_client::state::Member) -> String {
    if m.display.is_empty() {
        m.username.clone()
    } else {
        m.display.clone()
    }
}

impl Body {
    // ---- Overview ----

    pub(super) fn overview_section(
        &mut self,
        _window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Vec<AnyElement> {
        let access = self.access();
        let name = self.server_name();
        let draft = self.overview.borrow().clone();
        let mut out: Vec<AnyElement> =
            vec![section_header(t("admin.overview.title"), Some(&name)).into_any_element()];
        let mut rows = div().flex().flex_col();
        if access.loaded && access.manage_community {
            let mut field = TextField::new("server-name", t("admin.overview.name"), &self.name)
                .disabled(draft.busy);
            let refused = !draft.afk_failure && draft.error.is_some();
            if refused && let Some(e) = &draft.error {
                field = field.error(e.clone());
            }
            // The label is the accessible name only (`.dw-overview-field`): out of sight, unless a refusal needs room.
            let framed = if refused {
                div().w(layout::text_input_width()).child(field)
            } else {
                div()
                    .w(layout::text_input_width())
                    .h(layout::FIELD_H)
                    .overflow_hidden()
                    .child(div().mt(layout::hidden_label_offset()).child(field))
            };
            rows = rows.child(
                SettingsRow::new("server-name-row", t("admin.overview.name"))
                    .icon(IconName::Server)
                    .hint(t("admin.overview.hint"))
                    .position(if access.loaded && access.manage_community {
                        GroupPosition::First
                    } else {
                        GroupPosition::Only
                    })
                    .child(framed),
            );
        }
        let options: Vec<(String, &'static str)> = AFK_TIMEOUTS
            .iter()
            .map(|n| (n.to_string(), t(&format!("admin.overview.afk.{n}"))))
            .collect();
        let pairs: Vec<(&str, &str)> = options.iter().map(|(v, l)| (v.as_str(), *l)).collect();
        let on_pick = {
            let view = cx.entity().downgrade();
            move |value: &str, _: &mut Window, app: &mut App| {
                if let Ok(seconds) = value.parse::<u64>() {
                    let _ = view.update(app, |this, cx| {
                        let mut d = this.overview.borrow_mut();
                        d.timeout = seconds;
                        d.afk_failure = false;
                        d.error = None;
                        drop(d);
                        cx.notify();
                    });
                }
            }
        };
        let mut afk = SettingsRow::new("afk-row", t("admin.overview.afk"))
            .position(if access.loaded && access.manage_community {
                GroupPosition::Middle
            } else {
                GroupPosition::First
            })
            .icon(IconName::Moon)
            .hint(t("admin.overview.afkHint"))
            .child(
                Segmented::new("afk-timeout", &pairs, draft.timeout.to_string())
                    .disabled(draft.timeout_base.is_none() || draft.busy)
                    .on_change(on_pick),
            );
        if self
            .state
            .server_admin
            .afk_load_failed
            .get(&self.community)
            .copied()
            .unwrap_or(false)
        {
            afk = afk.child(alert(t("admin.afk.loadError")));
        }
        if draft.afk_failure
            && let Some(e) = &draft.error
        {
            afk = afk.child(alert(e.clone()));
        }
        rows = rows.child(afk);
        let mut leave = SettingsRow::new("leave-row", t("admin.overview.leave"))
            .position(GroupPosition::Last)
            .icon(IconName::LogOut)
            .hint(t("admin.overview.leaveHint"))
            .child(crate::tracked_focus(
                "server-settings-leave",
                &self.leave_focus,
                Button::new("leave-server", t("admin.leave.confirm"))
                    .focus_handle(&self.leave_focus)
                    .variant(ButtonVariant::Danger)
                    .on_click(act_click(cx, |this, _, cx| {
                        if this.access().owner {
                            this.owner_note = true;
                        } else {
                            this.dialogue = Dialogue::Leave;
                            this.dialogue_error = None;
                        }
                        cx.notify();
                    })),
            ));
        if self.owner_note {
            leave = leave.child(dim(tf("admin.leave.owner", &[("server", &name)])));
        }
        rows = rows.child(leave);
        out.push(rows.into_any_element());
        out.push(self.leave_dialog(&name, cx));
        out
    }

    fn close_dialogue(&self, cx: &Context<Self>) -> impl Fn(&mut Window, &mut App) + 'static {
        act(cx, |this, _, cx| {
            if !this.dialogue_busy {
                this.dialogue = Dialogue::None;
                this.dialogue_error = None;
                cx.notify();
            }
        })
    }

    fn leave_dialog(&mut self, name: &str, cx: &mut Context<Self>) -> AnyElement {
        ConfirmDialog::new(
            "leave-dialog",
            tf("admin.leave.title", &[("server", name)]),
            tf("admin.leave.body", &[("server", name)]),
            t("admin.leave.confirm"),
            t("admin.leave.busy"),
            t("admin.cancel"),
        )
        .open(self.dialogue == Dialogue::Leave)
        .busy(self.dialogue_busy)
        .error(self.dialogue_error.clone())
        .on_confirm(act(cx, |this, window, cx| this.leave(window, cx)))
        .on_close(self.close_dialogue(cx))
        .into_any_element()
    }

    // ---- Channels ----

    pub(super) fn channels_section(
        &mut self,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Vec<AnyElement> {
        let afk = self.afk_channel();
        let all = self.channels();
        let groups = model::grouped(&all);
        let categories: Vec<&Channel> = groups.iter().filter_map(|g| g.category).collect();
        let access = self.access();
        let mut out: Vec<AnyElement> =
            vec![section_header(t("admin.channels.title"), None).into_any_element()];
        out.push(
            div()
                .flex()
                .pb(layout::SECTION_GAP)
                .child(
                    Button::new("new-category", t("admin.channels.newCategory")).on_click(
                        act_click(cx, |this, _, cx| {
                            this.dialogue = Dialogue::Category;
                            this.dialogue_error = None;
                            cx.notify();
                        }),
                    ),
                )
                .into_any_element(),
        );
        if let Some(e) = &self.move_error {
            out.push(alert(e.clone()));
        }
        for group in &groups {
            let mut block = div().flex().flex_col().pb(layout::GROUP_GAP);
            let head_name: String = group.category.map_or_else(
                || t("admin.channels.noCategory").to_owned(),
                |c| c.name.clone(),
            );
            let mut head = div()
                .flex()
                .items_center()
                .gap(layout::INVITE_GAP)
                .h(layout::HEAD_H)
                .pr(layout::LIST_PAD_X)
                .mb(layout::HEAD_GAP)
                .child(
                    div().flex_1().min_w_0().child(
                        line(head_name, text::SM, line_height(text::SM, 1.5), color::FG)
                            .weight(font::WEIGHT_DISPLAY)
                            .ellipsis(),
                    ),
                );
            if let Some(category) = group.category {
                let (up, down) = model::category_slots(&categories, category);
                head = head.child(self.row_controls(category, up, down, false, cx));
            }
            block = block.child(head);
            let n = group.channels.len();
            // The list is one bordered card; its rows are divided by hairlines, 1px apart.
            let mut card = div()
                .flex()
                .flex_col()
                .gap(layout::LIST_GAP)
                .border_1()
                .border_color(color::HAIRLINE)
                .rounded(radius::XL)
                .overflow_hidden();
            for (i, c) in group.channels.iter().enumerate() {
                let (up, down) = model::move_slots(group, afk, c);
                let fixed = model::fixed(c, afk);
                let private = access.overwrites_loaded
                    && access.everyone.is_some_and(|e| {
                        self.state
                            .server_admin
                            .overwrites
                            .get(&self.community)
                            .is_some_and(|rows| is_private(rows, c.id, e))
                    });
                let mut row = div()
                    .flex()
                    .items_center()
                    .gap(layout::INVITE_GAP)
                    .h(layout::LIST_ROW_H)
                    .px(layout::LIST_PAD_X)
                    .border_color(color::HAIRLINE)
                    .when(i > 0, |r| r.border_t_1())
                    .child(
                        div().flex_1().min_w_0().child(
                            line(
                                model::shown(c),
                                text::SM,
                                line_height(text::SM, 1.5),
                                color::FG,
                            )
                            .ellipsis(),
                        ),
                    );
                if private {
                    row = row.child(tag(t("chperm.privateTag")));
                }
                row = row.child(self.row_controls(c, up, down, fixed, cx));
                card = card.child(row);
            }
            if n > 0 {
                block = block.child(card);
            }
            out.push(block.into_any_element());
        }
        out.push(self.edit_dialog(window, cx));
        out.push(self.delete_dialog(cx));
        out.push(self.category_dialog(cx));
        out.push(self.private_dialog(cx));
        out
    }

    /// The four fixed trailing slots of a row: up, down, edit, delete (the ManageList grammar). The AFK row keeps its
    /// slots but draws no moves and no delete.
    fn row_controls(
        &self,
        c: &Channel,
        up: bool,
        down: bool,
        fixed: bool,
        cx: &Context<Self>,
    ) -> AnyElement {
        let id = c.id;
        let hex = c.id.hex();
        let afk = self.afk_channel() == Some(c.id);
        let button = |key: &'static str, icon: IconName, label: String, enabled: bool| {
            // The web's IconButton default: 32px with an 18px glyph, filling the 32px slot.
            IconButton::new(SharedString::from(format!("{key}-{hex}")), icon, label)
                .size(IconButtonSize::S32)
                .disabled(!enabled)
        };
        let up_slot = (!fixed).then(|| {
            button(
                "move-up",
                IconName::ArrowUp,
                tf("admin.channels.upLabel", &[("name", &c.name)]),
                up,
            )
            .on_click(act_click(cx, move |this, window, cx| {
                this.move_channel(id, Direction::Up, window, cx)
            }))
        });
        let down_slot = (!fixed).then(|| {
            button(
                "move-down",
                IconName::ArrowDown,
                tf("admin.channels.downLabel", &[("name", &c.name)]),
                down,
            )
            .on_click(act_click(cx, move |this, window, cx| {
                this.move_channel(id, Direction::Down, window, cx)
            }))
        });
        let edit = button(
            "edit",
            IconName::SquarePen,
            tf("admin.channels.editLabel", &[("name", &c.name)]),
            true,
        )
        .on_click(act_click(cx, move |this, window, cx| {
            this.open_edit(id, window, cx)
        }));
        let delete = (!afk).then(|| {
            button(
                "delete",
                IconName::X,
                tf("admin.channels.deleteLabel", &[("name", &c.name)]),
                true,
            )
            .on_click(act_click(cx, move |this, _, cx| {
                this.dialogue = Dialogue::Delete(id);
                this.dialogue_error = None;
                cx.notify();
            }))
        });
        let slot = |key: &'static str, child: Option<AnyElement>| {
            let slot = TrailingSlot::new(SharedString::from(format!("slot-{key}-{hex}")));
            match child {
                Some(b) => slot.child(b),
                None => slot,
            }
        };
        let edit = crate::tracked_box(track_id(format!("edit-{}", c.name)), edit);
        div()
            .flex()
            .flex_none()
            .gap(layout::TRAIL_GAP)
            .items_center()
            .child(slot("up", up_slot.map(IntoElement::into_any_element)))
            .child(slot("down", down_slot.map(IntoElement::into_any_element)))
            .child(slot("edit", Some(edit.into_any_element())))
            .child(slot("delete", delete.map(IntoElement::into_any_element)))
            .into_any_element()
    }

    fn edit_dialog(&mut self, window: &Window, cx: &mut Context<Self>) -> AnyElement {
        let Dialogue::Edit(channel_id) = self.dialogue else {
            return div().into_any_element();
        };
        let Some(c) = self.state.channel(channel_id).cloned() else {
            return div().into_any_element();
        };
        let access = self.access();
        let afk = self.afk_channel() == Some(c.id);
        let title = match c.kind {
            2 => tf("admin.channel.titleCategory", &[("name", &c.name)]),
            1 => tf("admin.channel.titleVoice", &[("name", &c.name)]),
            _ => tf("admin.channel.titleText", &[("name", &c.name)]),
        };
        let mut body = div().flex().flex_col().gap(layout::SECTION_GAP);
        body = body.child(
            TextField::new("edit-name", t("admin.channel.name"), &self.edit_name)
                .hint(t("admin.channel.nameHint"))
                .disabled(self.dialogue_busy),
        );
        if c.kind == 0 {
            body = body.child(
                TextField::new("edit-topic", t("admin.channel.topic"), &self.edit_topic)
                    .hint(t("admin.channel.topicHint"))
                    .disabled(self.dialogue_busy),
            );
        }
        if c.kind != 2 {
            let categories: Vec<&Channel> = self
                .channels()
                .into_iter()
                .filter(|x| x.kind == 2)
                .collect();
            let mut options: Vec<(String, String)> =
                vec![("top".into(), t("admin.channels.noCategory").to_owned())];
            options.extend(categories.iter().map(|x| (x.id.hex(), x.name.clone())));
            let pairs: Vec<(&str, &str)> = options
                .iter()
                .map(|(v, l)| (v.as_str(), l.as_str()))
                .collect();
            body = body.child(
                Select::new(
                    "edit-category",
                    t("admin.channel.category"),
                    &pairs,
                    self.edit_parent.clone(),
                )
                .disabled(self.dialogue_busy)
                .on_change(act_value(cx, |this, value, _w, cx| {
                    this.edit_parent = value.to_owned();
                    cx.notify();
                })),
            );
        }
        if c.kind == 0 {
            let options: Vec<(String, &str)> = SLOW_STEPS
                .iter()
                .map(|s| (s.to_string(), slow_label(*s)))
                .collect();
            let pairs: Vec<(&str, &str)> = options.iter().map(|(v, l)| (v.as_str(), *l)).collect();
            body = body.child(
                Select::new(
                    "edit-slowmode",
                    t("slow.label"),
                    &pairs,
                    self.edit_slow.clone(),
                )
                .hint(t("slow.hint"))
                .disabled(self.dialogue_busy)
                .on_change(act_value(cx, |this, value, _w, cx| {
                    this.edit_slow = value.to_owned();
                    cx.notify();
                })),
            );
        }
        if afk {
            body = body.child(dim(t("afk.channel.note")));
        }
        if c.kind != 2 && !afk && access.manage_roles {
            body = body.child(self.permissions_panel(&c, cx));
        }
        if let Some(e) = &self.dialogue_error {
            body = body.child(alert(e.clone()));
        }
        let footer_cancel = Button::new("edit-cancel", t("admin.cancel"))
            .size(ButtonSize::Dialog)
            .on_click(act_click(cx, |this, _, cx| {
                if !this.dialogue_busy {
                    this.dialogue = Dialogue::None;
                    this.dialogue_error = None;
                    cx.notify();
                }
            }));
        let save = Button::new(
            "edit-save",
            if self.dialogue_busy {
                t("admin.channel.saving")
            } else {
                t("admin.channel.save")
            },
        )
        .variant(ButtonVariant::Accent)
        .size(ButtonSize::Dialog)
        .disabled(self.dialogue_busy)
        .on_click(act_click(cx, move |this, window, cx| {
            this.save_edit(channel_id, window, cx)
        }));
        // The panel is anchored under the top and capped; what does not fit scrolls inside (`.d-dialog` max-height).
        let cap = layout::dialog_body_cap(f32::from(window.viewport_size().height));
        let body = div()
            .id("edit-channel-body")
            .max_h(cap)
            .overflow_y_scroll()
            .child(body);
        Dialog::new("edit-channel", title, t("admin.cancel"))
            .kind(DialogKind::Form)
            .show_default_close(false)
            .body(body)
            .footer(footer_cancel)
            .footer(save)
            .on_close(self.close_dialogue(cx))
            .into_any_element()
    }

    /// ChannelPermissions.tsx: the private switch, who has settings of their own here (each a disclosure over a tri-state grid
    /// of the channel's rights, rank-gated), and the Add select. While the overwrites are unknown nothing is called public.
    fn permissions_panel(&self, c: &Channel, cx: &Context<Self>) -> AnyElement {
        use dilla_client::features::server_admin::{
            PERM_GROUPS, PermContext, Tri, add_options, channel_grid_bits, my_channel_bits,
            perm_entries, tri_of,
        };
        let access = self.access();
        let id = c.id;
        let heading = || {
            line(
                t("chperm.title"),
                text::SM,
                line_height(text::SM, 1.5),
                color::FG,
            )
            .weight(font::WEIGHT_LABEL)
        };
        let (Some(everyone), true) = (access.everyone, access.overwrites_loaded) else {
            return div()
                .flex()
                .flex_col()
                .gap(layout::ROW_GAP)
                .child(heading())
                .child(alert(t("chperm.error.unread")))
                .into_any_element();
        };
        let all_rows = self
            .state
            .server_admin
            .overwrites
            .get(&self.community)
            .cloned()
            .unwrap_or_default();
        let mine: Vec<_> = all_rows
            .iter()
            .filter(|o| o.channel == c.id)
            .copied()
            .collect();
        let roles = self
            .state
            .server_admin
            .roles
            .get(&self.community)
            .cloned()
            .unwrap_or_default();
        let members = self
            .state
            .members
            .get(&self.community)
            .cloned()
            .unwrap_or_default();
        let community_owner = self
            .state
            .communities
            .iter()
            .find(|x| x.id == self.community)
            .map(|x| x.owner);
        let me = self.state.account.user_id;
        let (Some(community_owner), Some(me)) = (community_owner, me) else {
            return div().child(heading()).into_any_element();
        };
        let ctx = PermContext {
            everyone,
            owner: access.owner,
            top: access.top,
            community_owner,
            me,
            roles: &roles,
            members: &members,
        };
        let entries = perm_entries(&mine, &self.perm_added, &ctx);
        let options = add_options(&entries, &ctx);
        let my_bits = my_channel_bits(&mine, &ctx);
        let private = dilla_client::features::server_admin::is_private(&mine, c.id, everyone);
        let label_of = |name: &str, is_everyone: bool| {
            if is_everyone {
                t("chperm.everyone").to_owned()
            } else {
                name.to_owned()
            }
        };

        let mut panel = div()
            .flex()
            .flex_col()
            .gap(layout::ROW_GAP)
            .child(heading());
        panel = panel.child(
            Toggle::new("channel-private", t("chperm.private"), private)
                .show_label(true)
                .hint(t("chperm.privateHint"))
                .disabled(self.dialogue_busy)
                .on_change(act_bool(cx, move |this, on, cx| {
                    this.dialogue = Dialogue::PrivateAsk(id, on);
                    this.dialogue_error = None;
                    cx.notify();
                })),
        );
        panel = panel.child(
            line(
                t("chperm.who"),
                text::SM,
                line_height(text::SM, 1.5),
                color::FG,
            )
            .weight(font::WEIGHT_LABEL),
        );
        if entries.is_empty() {
            panel = panel.child(dim(t("chperm.none")));
        }
        for entry in &entries {
            let key = (entry.kind, entry.target);
            let shown = self.perm_open == Some(key);
            let name = label_of(&entry.name, entry.is_everyone);
            let suffix = format!(
                "{}-{}",
                entry.kind,
                dilla_client::features::server_admin::hex_id(&entry.target)
            );
            let mut head = div()
                .flex()
                .items_center()
                .justify_between()
                .gap(layout::ROW_GAP)
                .child(
                    Button::new(
                        SharedString::from(format!("perm-entry-{suffix}")),
                        if entry.kind == 1 {
                            format!("{name} {}", t("chperm.member"))
                        } else {
                            name.clone()
                        },
                    )
                    .variant(ButtonVariant::Ghost)
                    .pressed(shown)
                    .on_click(act_click(cx, move |this, _, cx| {
                        this.perm_open = if this.perm_open == Some(key) {
                            None
                        } else {
                            Some(key)
                        };
                        cx.notify();
                    })),
                );
            if !entry.is_everyone {
                head = head.child(
                    Button::new(
                        SharedString::from(format!("perm-remove-{suffix}")),
                        t("chperm.remove"),
                    )
                    .variant(ButtonVariant::Ghost)
                    .disabled(!entry.editable || self.perm_busy)
                    .on_click(act_click(cx, move |this, window, cx| {
                        this.perm_remove(id, key, window, cx)
                    })),
                );
            }
            panel = panel.child(head);
            if shown {
                let groups: Vec<GridGroup> = PERM_GROUPS
                    .iter()
                    .map(|(group, keys)| GridGroup {
                        title: t(&format!("perm.group.{group}")).into(),
                        rows: keys
                            .iter()
                            .filter(|(_, bit)| channel_grid_bits(*bit))
                            .map(|(k, bit)| GridRow {
                                id: SharedString::from((*bit).to_string()),
                                label: t(&format!("perm.{k}")).into(),
                                hint: t(&format!("perm.{k}.hint")).into(),
                                value: tri_of(entry.allow, entry.deny, *bit).wire(),
                                disabled: !entry.editable || my_bits.is_some_and(|b| b & *bit == 0),
                            })
                            .collect(),
                    })
                    .filter(|g| !g.rows.is_empty())
                    .collect();
                panel = panel.child(
                    PermissionGrid::new(
                        SharedString::from(format!("perm-grid-{suffix}")),
                        groups,
                        [
                            t("perm.allow").into(),
                            t("perm.inherit").into(),
                            t("perm.deny").into(),
                        ],
                    )
                    .on_change({
                        let view = cx.entity().downgrade();
                        move |bit: &str, value: &str, window: &mut Window, app: &mut App| {
                            let (Ok(bit), Some(value)) =
                                (bit.parse::<u32>(), Tri::from_wire(value))
                            else {
                                return;
                            };
                            let _ = view.update(app, |this, cx| {
                                this.perm_change(id, key, bit, value, window, cx)
                            });
                        }
                    }),
                );
            }
        }
        if !options.is_empty() {
            let picked = self
                .perm_choice
                .filter(|c| options.iter().any(|o| (o.kind, o.target) == *c))
                .or_else(|| options.first().map(|o| (o.kind, o.target)));
            let labels: Vec<(String, String)> = options
                .iter()
                .map(|o| {
                    (
                        format!(
                            "{}:{}",
                            o.kind,
                            dilla_client::features::server_admin::hex_id(&o.target)
                        ),
                        label_of(&o.label, o.is_everyone),
                    )
                })
                .collect();
            let pairs: Vec<(&str, &str)> = labels
                .iter()
                .map(|(v, l)| (v.as_str(), l.as_str()))
                .collect();
            let current = picked
                .map(|(k, t)| format!("{k}:{}", dilla_client::features::server_admin::hex_id(&t)))
                .unwrap_or_default();
            panel = panel.child(
                div()
                    .flex()
                    .items_end()
                    .gap(layout::ROW_GAP)
                    .child(
                        Select::new("perm-add", t("chperm.addLabel"), &pairs, current).on_change(
                            act_value(cx, |this, value, _w, cx| {
                                let mut parts = value.splitn(2, ':');
                                if let (Some(kind), Some(hex)) = (parts.next(), parts.next())
                                    && let (Ok(kind), Some(target)) = (
                                        kind.parse::<u8>(),
                                        dilla_client::features::server_admin::unhex_id(hex),
                                    )
                                {
                                    this.perm_choice = Some((kind, target));
                                }
                                cx.notify();
                            }),
                        ),
                    )
                    .child(
                        Button::new("perm-add-button", t("chperm.add"))
                            .variant(ButtonVariant::Ghost)
                            .on_click(act_click(cx, move |this, _, cx| {
                                let first = options.first().map(|o| (o.kind, o.target));
                                if let Some(entry) = this
                                    .perm_choice
                                    .filter(|c| options.iter().any(|o| (o.kind, o.target) == *c))
                                    .or(first)
                                {
                                    if !this.perm_added.contains(&entry) {
                                        this.perm_added.push(entry);
                                    }
                                    this.perm_open = Some(entry);
                                    this.perm_choice = None;
                                }
                                cx.notify();
                            })),
                    ),
            );
        }
        if let Some(e) = &self.perm_error {
            panel = panel.child(alert(e.clone()));
        }
        panel.into_any_element()
    }

    fn private_dialog(&mut self, cx: &mut Context<Self>) -> AnyElement {
        let (open, channel, make_private) = match self.dialogue {
            Dialogue::PrivateAsk(c, on) => (true, Some(c), on),
            _ => (false, None, true),
        };
        let name = channel
            .and_then(|c| self.state.channel(c))
            .map_or_else(String::new, |c| c.name.clone());
        let server = self.server_name();
        let (title, body, confirm) = if make_private {
            (
                tf("chperm.makePrivate.title", &[("name", &name)]),
                t("chperm.makePrivate.body").to_owned(),
                t("chperm.makePrivate.confirm"),
            )
        } else {
            (
                tf("chperm.makePublic.title", &[("name", &name)]),
                tf("chperm.makePublic.body", &[("server", &server)]),
                t("chperm.makePublic.confirm"),
            )
        };
        ConfirmDialog::new(
            "private-dialog",
            title,
            body,
            confirm,
            t("chperm.busy"),
            t("chperm.cancel"),
        )
        .tone(Tone::Accent)
        .open(open)
        .busy(self.dialogue_busy)
        .error(self.dialogue_error.clone())
        .on_confirm(act(cx, move |this, window, cx| {
            if let Dialogue::PrivateAsk(c, on) = this.dialogue {
                this.set_private(c, on, window, cx);
            }
        }))
        .on_close(act(cx, |this, _, cx| {
            if !this.dialogue_busy
                && let Dialogue::PrivateAsk(c, _) = this.dialogue
            {
                this.dialogue = Dialogue::Edit(c);
                this.dialogue_error = None;
                cx.notify();
            }
        }))
        .into_any_element()
    }

    fn delete_dialog(&mut self, cx: &mut Context<Self>) -> AnyElement {
        let (open, channel) = match self.dialogue {
            Dialogue::Delete(c) => (true, Some(c)),
            _ => (false, None),
        };
        let c = channel.and_then(|c| self.state.channel(c)).cloned();
        let (kind, name) = c.as_ref().map_or(("Text", String::new()), |c| {
            (
                match c.kind {
                    2 => "Category",
                    1 => "Voice",
                    _ => "Text",
                },
                c.name.clone(),
            )
        });
        let body = match kind {
            "Text" => t("admin.channelDelete.body"),
            "Voice" => t("admin.channelDelete.bodyVoice"),
            _ => t("admin.channelDelete.bodyCategory"),
        };
        ConfirmDialog::new(
            "delete-channel-dialog",
            tf(
                &format!("admin.channelDelete.title{kind}"),
                &[("name", &name)],
            ),
            body,
            t("admin.channelDelete.confirm"),
            t("admin.channelDelete.busy"),
            t("admin.cancel"),
        )
        .open(open)
        .busy(self.dialogue_busy)
        .error(self.dialogue_error.clone())
        .on_confirm(act(cx, move |this, window, cx| {
            if let Dialogue::Delete(c) = this.dialogue {
                this.delete_channel(c, window, cx);
            }
        }))
        .on_close(self.close_dialogue(cx))
        .into_any_element()
    }

    fn category_dialog(&mut self, cx: &mut Context<Self>) -> AnyElement {
        let server = self.server_name();
        let mut body = div().flex().flex_col().gap(layout::SECTION_GAP).child(
            TextField::new(
                "category-name",
                t("admin.category.name"),
                &self.category_name,
            )
            .hint(t("admin.category.hint"))
            .disabled(self.dialogue_busy),
        );
        if let Some(e) = &self.dialogue_error {
            body = body.child(alert(e.clone()));
        }
        Dialog::new(
            "category-dialog",
            tf("admin.category.title", &[("server", &server)]),
            t("admin.cancel"),
        )
        .open(self.dialogue == Dialogue::Category)
        .kind(DialogKind::Form)
        .body(body)
        .footer(
            Button::new("category-cancel", t("admin.cancel"))
                .size(ButtonSize::Dialog)
                .on_click(act_click(cx, |this, _, cx| {
                    if !this.dialogue_busy {
                        this.dialogue = Dialogue::None;
                        this.dialogue_error = None;
                        cx.notify();
                    }
                })),
        )
        .footer(
            Button::new(
                "category-create",
                if self.dialogue_busy {
                    t("admin.category.busy")
                } else {
                    t("admin.category.submit")
                },
            )
            .variant(ButtonVariant::Accent)
            .size(ButtonSize::Dialog)
            .disabled(self.dialogue_busy)
            .on_click(act_click(cx, |this, window, cx| {
                this.create_category(window, cx)
            })),
        )
        .on_close(self.close_dialogue(cx))
        .into_any_element()
    }

    // ---- GIFs ----

    pub(super) fn gifs_section(
        &mut self,
        _window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Vec<AnyElement> {
        let gifs = self.state.server_admin.gifs.get(&self.community).copied();
        let mut out: Vec<AnyElement> =
            vec![section_header(t("workspace.gifs"), None).into_any_element()];
        let (set_row, form): (Option<(&str, &[&str])>, Option<Vec<&str>>) = match gifs {
            Some(g) if g.on && !self.gif_editing => (
                Some((
                    t("gif.settings.maskedLabel"),
                    &[t("gif.settings.replace"), t("gif.settings.remove")][..],
                )),
                None,
            ),
            Some(g) => (
                None,
                Some(if g.on {
                    vec![t("gif.settings.save"), t("gif.settings.cancel")]
                } else {
                    vec![t("gif.settings.save")]
                }),
            ),
            None => (None, None),
        };
        let mut inner = div()
            .flex()
            .flex_col()
            .gap(layout::GIFS_GAP)
            .w(layout::gifs_column_width(
                t("gif.settings.getKey"),
                set_row,
                form.as_deref(),
            ));
        // The link is the page's own type (`--text-base`).
        inner = inner.child(line(
            t("gif.settings.getKey"),
            text::BASE,
            line_height(text::BASE, 1.5),
            color::FG_LINK,
        ));
        match gifs {
            None if self.client.is_some() => {}
            None => {}
            Some(g) => {
                let showing_field = !g.on || self.gif_editing;
                if g.on && !self.gif_editing {
                    inner = inner.child(
                        // The column is this row's own width: it fits on one line.
                        div()
                            .flex()
                            .items_center()
                            .gap(layout::GIFS_FORM_GAP)
                            .child(div().mr(layout::ROW_GAP).child(line(
                                t("gif.settings.maskedLabel"),
                                text::BASE,
                                line_height(text::BASE, 1.5),
                                color::FG,
                            )))
                            .child(
                                Button::new("gif-replace", t("gif.settings.replace")).on_click(
                                    act_click(cx, |this, _, cx| {
                                        this.gif_editing = true;
                                        this.gif_error = None;
                                        cx.notify();
                                    }),
                                ),
                            )
                            .child(
                                Button::new("gif-remove", t("gif.settings.remove"))
                                    .variant(ButtonVariant::Danger)
                                    .disabled(self.gif_busy)
                                    .on_click(act_click(cx, |this, window, cx| {
                                        this.remove_gif_key(window, cx)
                                    })),
                            ),
                    );
                }
                if showing_field {
                    let mut field = TextField::new("gif-key", t("gif.settings.key"), &self.gif_key);
                    if let Some(e) = &self.gif_error {
                        field = field.error(e.clone());
                    }
                    // `.dw-gifs-settings__form`: the field's 20rem basis takes the line, Save wraps under it.
                    let mut form = div()
                        .flex()
                        .flex_wrap()
                        .items_end()
                        .gap(layout::GIFS_FORM_GAP)
                        .child(div().w_full().child(field))
                        .child(
                            Button::new("gif-save", t("gif.settings.save"))
                                .variant(ButtonVariant::Accent)
                                .disabled(self.gif_busy)
                                .on_click(act_click(cx, |this, window, cx| {
                                    this.save_gif_key(window, cx)
                                })),
                        );
                    if g.on {
                        form = form.child(
                            Button::new("gif-cancel", t("gif.settings.cancel")).on_click(
                                act_click(cx, |this, _, cx| {
                                    this.gif_editing = false;
                                    this.gif_error = None;
                                    cx.notify();
                                }),
                            ),
                        );
                    }
                    inner = inner.child(form);
                } else if let Some(e) = &self.gif_error {
                    inner = inner.child(alert(e.clone()));
                }
                let options: Vec<(&str, &str)> = GifRating::ALL
                    .iter()
                    .map(|r| (r.wire(), t(&format!("gif.rating.{}", r.wire()))))
                    .collect();
                inner = inner.child(
                    Select::new(
                        "gif-rating",
                        t("gif.settings.rating"),
                        &options,
                        g.rating.wire(),
                    )
                    .disabled(self.gif_busy)
                    .on_change(act_value(cx, |this, value, window, cx| {
                        if let Some(r) = GifRating::from_wire(value) {
                            this.pick_rating(r, window, cx);
                        }
                    })),
                );
            }
        }
        out.push(
            SettingsRow::new("gif-row", t("gif.settings.title"))
                .hint(t("gif.settings.hint"))
                .layout(Layout::Stacked)
                .child(inner)
                .into_any_element(),
        );
        out
    }

    // ---- Invites ----

    pub(super) fn invites_section(
        &mut self,
        _window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Vec<AnyElement> {
        let access = self.access();
        let manager = access.loaded && access.manage_community;
        let name = self.server_name();
        let (ttl, uses) = model::clamp_choice(self.ttl, self.uses, manager);
        let now = self.clock.now();
        let mut out: Vec<AnyElement> = vec![
            section_header_full(
                t("admin.invites.title"),
                Some(&tf("serverSettings.invites.body", &[("server", &name)])),
            )
            .into_any_element(),
        ];
        if let Some(e) = &self.invite_load_error {
            out.push(
                div()
                    .flex()
                    .items_center()
                    .gap(layout::FIELD_GAP)
                    .child(alert(e.clone()))
                    .child(
                        Button::new("invites-retry", t("invite.retry"))
                            .on_click(act_click(cx, |this, window, cx| {
                                this.load_for(ServerSection::Invites, window, cx)
                            })),
                    )
                    .into_any_element(),
            );
        }
        let ttl_options: Vec<(String, &str)> = model::ttl_options(manager)
            .into_iter()
            .map(|(s, key)| (s.to_string(), t(key)))
            .collect();
        let ttl_pairs: Vec<(&str, &str)> =
            ttl_options.iter().map(|(v, l)| (v.as_str(), *l)).collect();
        let uses_options: Vec<(String, String)> = model::uses_options(manager)
            .into_iter()
            .map(|n| (n.to_string(), model::uses_label(n)))
            .collect();
        let uses_pairs: Vec<(&str, &str)> = uses_options
            .iter()
            .map(|(v, l)| (v.as_str(), l.as_str()))
            .collect();
        let choice = |label: &str, control: AnyElement| {
            div()
                .flex()
                .flex_col()
                .gap(layout::LABEL_GAP)
                .child(line(
                    label.to_owned(),
                    text::XS,
                    line_height(text::XS, 1.5),
                    color::FG_2,
                ))
                // Under the 16.5px label the control lies on a half pixel: it paints where the web's does.
                .child(snapped(control))
        };
        let mut panel = div()
            .flex()
            .flex_col()
            .gap(layout::SECTION_GAP)
            .relative()
            .p(layout::INVITE_PAD_X)
            // The card's edge is drawn on the web's pixels (it ends on a half pixel); the border keeps its room.
            .border_1()
            .border_color(gpui::transparent_black())
            .child(edge_box(radius::XL, color::HAIRLINE))
            .child(
                div()
                    .flex()
                    .flex_wrap()
                    .gap(layout::CHOICE_GAP)
                    .child(choice(
                        t("serverSettings.invites.expires"),
                        Segmented::new("invite-ttl", &ttl_pairs, ttl.to_string())
                            .on_change(act_value(cx, |this, value, _w, cx| {
                                if let Ok(v) = value.parse() {
                                    this.ttl = v;
                                }
                                cx.notify();
                            }))
                            .into_any_element(),
                    ))
                    .child(choice(
                        t("serverSettings.invites.uses"),
                        Segmented::new("invite-uses-choice", &uses_pairs, uses.to_string())
                            .on_change(act_value(cx, |this, value, _w, cx| {
                                if let Ok(v) = value.parse() {
                                    this.uses = v;
                                }
                                cx.notify();
                            }))
                            .into_any_element(),
                    )),
            );
        if !manager {
            panel = panel.child(dim(t("serverSettings.invites.managersOnly")));
        }
        if self.made.is_none() {
            panel = panel.child(
                div().flex().child(snapped(
                    Button::new(
                        "invite-make",
                        if self.making {
                            t("serverSettings.invites.making")
                        } else {
                            t("serverSettings.invites.make")
                        },
                    )
                    .icon(IconName::Plus)
                    .variant(ButtonVariant::Accent)
                    .size(ButtonSize::Panel)
                    .disabled(self.making)
                    .on_click(act_click(cx, |this, window, cx| {
                        this.make_invite(window, cx)
                    })),
                )),
            );
        }
        if let Some(made) = &self.made {
            panel = panel.child(
                SettingsRow::new("invite-link", t("serverSettings.invites.link"))
                    .hint(made.note.clone())
                    .layout(Layout::Stacked)
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap(layout::FIELD_GAP)
                            .child(
                                div().flex_1().min_w_0().child(
                                    line(
                                        made.url.clone(),
                                        text::SM,
                                        line_height(text::SM, 1.5),
                                        color::FG,
                                    )
                                    .ellipsis(),
                                ),
                            )
                            .child(
                                Button::new("invite-copy", t("serverSettings.invites.copy"))
                                    .on_click(act_click(cx, |this, _, cx| this.copy_invite(cx))),
                            ),
                    ),
            );
        }
        if let Some((message, is_alert)) = &self.invite_message {
            panel = panel.child(if *is_alert {
                alert(message.clone())
            } else {
                dim(message.clone())
            });
        }
        out.push(panel.into_any_element());

        let invites = self
            .state
            .server_admin
            .invites
            .get(&self.community)
            .cloned()
            .unwrap_or_default();
        let me = self.state.account.user_id;
        let members = self.state.members.get(&self.community).cloned();
        out.push(
            div()
                .pt(layout::HEADING_TOP)
                .pb(layout::ROW_GAP)
                .child(
                    line(
                        tf(
                            "serverSettings.invites.active",
                            &[("count", &invites.len().to_string())],
                        )
                        .to_uppercase(),
                        text::MICRO,
                        line_height(text::MICRO, 1.5),
                        color::FG_3,
                    )
                    .weight(font::WEIGHT_LABEL)
                    .tracking(0.12),
                )
                .into_any_element(),
        );
        // Until the members are read a maker cannot be named, so no row is listed rather than naming one wrongly.
        let shown: Vec<_> = if members.is_none() {
            Vec::new()
        } else {
            invites
        };
        // The live links are one rounded card of 48px rows (`.d-invite-panel__list`): the masked code, then uses and
        // expiry at a fixed width each (the expiry wraps inside it), then Revoke.
        let host = if self.state.server_admin.host.is_empty() {
            self.host.clone()
        } else {
            self.state.server_admin.host.clone()
        };
        if !shown.is_empty() {
            let mut card = div()
                .flex()
                .flex_col()
                .border_1()
                .border_color(color::HAIRLINE)
                .rounded(radius::XL)
                .overflow_hidden();
            for (i, invite) in shown.into_iter().enumerate() {
                let texts = model::invite_texts(&invite, &host, now);
                let may_revoke = manager || (me.is_some() && invite.created_by == me);
                let id = invite.id;
                let meta_width = layout::INVITE_META_W;
                let mut row = div()
                    .flex()
                    .flex_wrap()
                    .items_center()
                    .gap(layout::FIELD_GAP)
                    .min_h(layout::LIST_ROW_H)
                    .px(layout::INVITE_PAD_X)
                    .py(layout::ROW_GAP)
                    .border_color(color::HAIRLINE)
                    .when(i > 0, |r| r.border_t_1())
                    .child(
                        div()
                            .flex_auto()
                            .w(layout::INVITE_CODE_BASIS)
                            .min_w_0()
                            .child(line(
                                texts.code,
                                text::UI,
                                line_height(text::UI, 1.5),
                                color::FG,
                            )),
                    )
                    .child(div().w(meta_width).child(line(
                        texts.uses,
                        text::XS,
                        line_height(text::XS, 1.5),
                        color::FG_2,
                    )))
                    .child(div().w(meta_width).flex().flex_col().children(
                        dilla_desktop_ui::text::paragraph(
                            &texts.expires,
                            text::XS,
                            line_height(text::XS, 1.5),
                            color::FG_2,
                            f32::from(meta_width),
                        ),
                    ));
                if may_revoke {
                    row = row.child(snapped(
                        Button::new(
                            SharedString::from(format!("revoke-{}", id.hex())),
                            t("serverSettings.invites.revoke"),
                        )
                        .variant(ButtonVariant::Danger)
                        .size(ButtonSize::Row)
                        .disabled(self.busy_invite.is_some())
                        .on_click(act_click(cx, move |this, window, cx| {
                            this.revoke_invite(id, window, cx)
                        })),
                    ));
                }
                card = card.child(row);
            }
            out.push(snapped(card).into_any_element());
        }
        out
    }

    // ---- Welcome and Join roles ----

    pub(super) fn bot_page(
        &mut self,
        section: ServerSection,
        cx: &mut Context<Self>,
    ) -> Vec<AnyElement> {
        let title = if section == ServerSection::Welcome {
            t("admin.bot.welcomeSettings.title")
        } else {
            t("admin.bot.joinRole.title")
        };
        // The Welcome panel carries its own heading; Join roles has the section's.
        let mut out: Vec<AnyElement> = vec![
            section_header(
                if section == ServerSection::Welcome {
                    t("workspace.welcome")
                } else {
                    title
                },
                None,
            )
            .into_any_element(),
        ];
        out.push(
            div()
                .flex()
                .pb(layout::SECTION_GAP)
                .child(
                    Button::new("bot-setup", t("workspace.botSetup")).on_click(act_click(
                        cx,
                        |this, _, cx| {
                            cx.emit(super::BodyEvent::OpenSection(ServerSection::Bot));
                            let _ = this;
                        },
                    )),
                )
                .into_any_element(),
        );
        if section == ServerSection::Welcome {
            // The panel until the bot installation (Task 21) fills it: its heading and help, why it cannot be used, and
            // its disabled load button.
            let para = |key: &str| {
                line(
                    t(key),
                    text::BASE,
                    line_height(text::BASE, 1.5),
                    color::FG_2,
                )
            };
            out.push(
                div()
                    .flex()
                    .flex_col()
                    .child(
                        line(
                            title,
                            text::PREVIEW,
                            line_height(text::PREVIEW, 1.5),
                            color::FG,
                        )
                        .weight(font::WEIGHT_DISPLAY),
                    )
                    .child(para("admin.bot.welcomeSettings.help"))
                    .child(para("admin.bot.welcomeSettings.requiresCapability"))
                    .child(para("admin.bot.welcomeSettings.noChannels"))
                    .child(
                        div().flex().child(
                            Button::new("welcome-load", t("admin.bot.welcomeSettings.load"))
                                .disabled(true),
                        ),
                    )
                    .into_any_element(),
            );
        } else {
            out.push(dim(t("admin.bot.joinRole.requiresBot")));
        }
        out
    }

    // ---- Danger ----

    pub(super) fn danger_section(
        &mut self,
        _window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Vec<AnyElement> {
        let access = self.access();
        let owner = access.owner;
        let name = self.server_name();
        let me = self.state.account.user_id;
        let others: Vec<_> = self
            .state
            .members
            .get(&self.community)
            .map(|ms| {
                ms.iter()
                    .filter(|m| Some(m.user_id) != me && m.kind == 0)
                    .cloned()
                    .collect()
            })
            .unwrap_or_default();
        let members_known = self.state.members.contains_key(&self.community);
        let title = t(if owner {
            "admin.nav.delete"
        } else {
            "admin.nav.leave"
        });
        let mut out: Vec<AnyElement> = vec![if owner {
            section_header(title, Some(&tf("admin.leave.owner", &[("server", &name)])))
                .into_any_element()
        } else {
            section_header(title, None).into_any_element()
        }];
        let mut rows = div().flex().flex_col();
        if owner {
            if members_known && others.is_empty() {
                rows = rows.child(dim(t("admin.transfer.none")));
            }
            if !others.is_empty() {
                rows = rows.child(
                    SettingsRow::new("transfer-row", t("admin.nav.transfer"))
                        .position(GroupPosition::First)
                        .icon(IconName::Crown)
                        .child(
                            Button::new("transfer-open", t("admin.nav.transfer")).on_click(
                                act_click(cx, |this, window, cx| {
                                    this.open_typed(Dialogue::Transfer, window, cx)
                                }),
                            ),
                        ),
                );
            }
            rows = rows.child(
                SettingsRow::new("delete-row", t("admin.nav.delete"))
                    .position(if others.is_empty() {
                        GroupPosition::Only
                    } else {
                        GroupPosition::Last
                    })
                    .icon(IconName::Trash2)
                    .child(
                        Button::new("delete-open", t("admin.nav.delete"))
                            .variant(ButtonVariant::Danger)
                            .on_click(act_click(cx, |this, window, cx| {
                                this.open_typed(Dialogue::DeleteServer, window, cx)
                            })),
                    ),
            );
        } else {
            rows = rows.child(
                SettingsRow::new("leave-row", t("admin.nav.leave"))
                    .icon(IconName::LogOut)
                    .child(
                        Button::new("leave-open", t("admin.leave.open"))
                            .variant(ButtonVariant::Danger)
                            .on_click(act_click(cx, |this, _, cx| {
                                this.dialogue = Dialogue::Leave;
                                this.dialogue_error = None;
                                cx.notify();
                            })),
                    ),
            );
        }
        out.push(rows.into_any_element());
        out.push(self.leave_dialog(&name, cx));
        out.push(self.delete_server_dialog(&name, cx));
        out.push(self.transfer_dialog(&name, &others, cx));
        out
    }

    fn typed_field(&self, label: &str, cx: &App) -> TextField {
        let typed = self.typed.read(cx).value().to_string();
        let mut field = TextField::new("typed-name", label.to_owned(), &self.typed);
        if !typed.is_empty() && nfc(&typed) != nfc(&self.server_name()) {
            field = field.hint(t("admin.delete.mismatch"));
        }
        field
    }

    fn matches_name(&self, cx: &App) -> bool {
        nfc(&self.typed.read(cx).value()) == nfc(&self.server_name())
    }

    fn delete_server_dialog(&mut self, name: &str, cx: &mut Context<Self>) -> AnyElement {
        let fields = self.typed_field(t("admin.delete.confirmName"), cx);
        ConfirmDialog::new(
            "delete-server-dialog",
            tf("admin.delete.title", &[("server", name)]),
            tf("admin.delete.body", &[("server", name)]),
            t("admin.delete.confirm"),
            t("admin.delete.busy"),
            t("admin.cancel"),
        )
        .open(self.dialogue == Dialogue::DeleteServer)
        .busy(self.dialogue_busy)
        .error(self.dialogue_error.clone())
        .can_confirm(self.matches_name(cx))
        .fields(fields, None)
        .on_confirm(act(cx, |this, window, cx| this.delete_server(window, cx)))
        .on_close(self.close_dialogue(cx))
        .into_any_element()
    }

    fn transfer_dialog(
        &mut self,
        name: &str,
        others: &[dilla_client::state::Member],
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let chosen = others
            .iter()
            .find(|m| Some(m.user_id) == self.transfer_to)
            .or(others.first())
            .cloned();
        let options: Vec<(String, String)> = others
            .iter()
            .map(|m| (m.user_id.hex(), shown_name(m)))
            .collect();
        let pairs: Vec<(&str, &str)> = options
            .iter()
            .map(|(v, l)| (v.as_str(), l.as_str()))
            .collect();
        let fields = div()
            .flex()
            .flex_col()
            .gap(layout::SECTION_GAP)
            .child(
                Select::new(
                    "transfer-member",
                    t("admin.transfer.member"),
                    &pairs,
                    chosen.as_ref().map(|m| m.user_id.hex()).unwrap_or_default(),
                )
                .on_change(act_value(cx, |this, value, _w, cx| {
                    this.transfer_to = this
                        .state
                        .members
                        .get(&this.community)
                        .and_then(|ms| ms.iter().find(|m| m.user_id.hex() == value))
                        .map(|m| m.user_id);
                    this.dialogue_error = None;
                    cx.notify();
                })),
            )
            .child(self.typed_field(t("admin.transfer.confirmName"), cx));
        let can = self.matches_name(cx) && chosen.is_some();
        ConfirmDialog::new(
            "transfer-dialog",
            tf("admin.transfer.title", &[("server", name)]),
            tf("admin.transfer.body", &[("server", name)]),
            t("admin.transfer.confirm"),
            t("admin.transfer.busy"),
            t("admin.cancel"),
        )
        .open(self.dialogue == Dialogue::Transfer)
        .busy(self.dialogue_busy)
        .error(self.dialogue_error.clone())
        .can_confirm(can)
        .fields(fields, None)
        .on_confirm(act(cx, move |this, window, cx| {
            let to = chosen.as_ref().map(|m| (m.user_id, shown_name(m)));
            if let Some((id, name)) = to {
                this.transfer(id, name, window, cx);
            }
        }))
        .on_close(self.close_dialogue(cx))
        .into_any_element()
    }
}

/// A `Select`'s or `Segmented`'s change handler on the settings' own entity.
fn act_value(
    cx: &Context<Body>,
    f: impl Fn(&mut Body, &str, &mut Window, &mut Context<Body>) + 'static,
) -> impl Fn(&str, &mut Window, &mut App) + 'static {
    let view = cx.entity().downgrade();
    move |value, window, app| {
        let _ = view.update(app, |this, cx| f(this, value, window, cx));
    }
}

/// A `Toggle`'s change handler.
fn act_bool(
    cx: &Context<Body>,
    f: impl Fn(&mut Body, bool, &mut Context<Body>) + 'static,
) -> impl Fn(bool, &mut Window, &mut App) + 'static {
    let view = cx.entity().downgrade();
    move |value, _window, app| {
        let _ = view.update(app, |this, cx| f(this, value, cx));
    }
}
