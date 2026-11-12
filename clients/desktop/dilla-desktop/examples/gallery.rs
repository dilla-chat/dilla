//! The component gallery: every shared component of `dilla-desktop-ui` in each of its states, on one scrolling page, the
//! native counterpart of the web client's Storybook stories. `cargo run -p dilla-desktop --example gallery`.
//!
//! The overlays (the dialog, the confirmation, the action menu, the toast, the settings frame) open from their buttons, so
//! their behaviour can be tried by hand; the controls keep their state, so a toggle toggles and a segmented group moves.

use dilla_desktop_ui::components::action_menu::{ActionMenu, MenuItem};
use dilla_desktop_ui::components::audio_meter::AudioMeter;
use dilla_desktop_ui::components::banner::{Banner, BannerTone};
use dilla_desktop_ui::components::check_list::{CheckList, CheckListItem};
use dilla_desktop_ui::components::choice_cards::{ChoiceCardOption, ChoiceCards};
use dilla_desktop_ui::components::confirm_dialog::{ConfirmDialog, Tone};
use dilla_desktop_ui::components::dialog::Dialog;
use dilla_desktop_ui::components::empty_state::EmptyState;
use dilla_desktop_ui::components::icon_button::{IconButton, IconButtonSize};
use dilla_desktop_ui::components::key_hint::KeyHint;
use dilla_desktop_ui::components::link_banner::{LinkBanner, LinkStatus};
use dilla_desktop_ui::components::pill::{Pill, PillKind};
use dilla_desktop_ui::components::qr_code::QrCode;
use dilla_desktop_ui::components::row::Person;
use dilla_desktop_ui::components::segmented::Segmented;
use dilla_desktop_ui::components::select::Select;
use dilla_desktop_ui::components::settings::footer::SettingsFooter;
use dilla_desktop_ui::components::settings::frame::{SettingsFrame, group_heading, section_header};
use dilla_desktop_ui::components::settings::nav::{NavItem, SettingsNav};
use dilla_desktop_ui::components::settings::row::{GroupPosition, Layout, SettingsRow};
use dilla_desktop_ui::components::state_dot::{StateDot, StateDotSize, StateDotTone};
use dilla_desktop_ui::components::tag::{Tag, TagKind};
use dilla_desktop_ui::components::toast::{ToastHost, show_toast};
use dilla_desktop_ui::components::toggle::Toggle;
use dilla_desktop_ui::components::trailing_slot::TrailingSlot;
use dilla_desktop_ui::components::{
    Avatar, AvatarSize, Button, ButtonSize, ButtonVariant, Presence, Row, RowKind,
};
use dilla_desktop_ui::metrics::Density;
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, font, text};
use dilla_desktop_ui::{Icon, IconName};
use gpui::{
    AppContext, Context, Entity, InteractiveElement, IntoElement, ParentElement, Render,
    SharedString, StatefulInteractiveElement, Styled, Window, div, px, size,
};
use gpui_component::input::{InputState, TextareaState};

pub struct Gallery {
    toggle: bool,
    density: String,
    pick: String,
    factor: String,
    roles: Vec<(&'static str, bool)>,
    menu_open: bool,
    dialog: bool,
    confirm: bool,
    settings: bool,
    section: String,
    name: Entity<InputState>,
    bio: Entity<TextareaState>,
}

impl Gallery {
    /// Opens every overlay at once (the smoke test draws them all).
    pub fn open_all(&mut self) {
        self.menu_open = true;
        self.dialog = true;
        self.confirm = true;
        self.settings = true;
    }

    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        Self {
            toggle: true,
            density: "regular".into(),
            pick: "b".into(),
            factor: "totp".into(),
            roles: vec![("Admin", true), ("Moderator", false), ("Everyone", true)],
            menu_open: false,
            dialog: false,
            confirm: false,
            settings: false,
            section: "profile".into(),
            name: cx.new(|cx| InputState::new(window, cx).placeholder("Display name")),
            bio: cx.new(|cx| TextareaState::new(window, cx)),
        }
    }
}

fn heading(label: &'static str) -> impl IntoElement {
    div().pt(px(28.0)).pb(px(10.0)).child(
        line(
            label,
            text::MICRO,
            line_height(text::MICRO, 1.5),
            color::FG_3,
        )
        .weight(font::WEIGHT_LABEL)
        .tracking(0.12),
    )
}

fn row(children: impl IntoIterator<Item = gpui::AnyElement>) -> gpui::Div {
    div()
        .flex()
        .flex_wrap()
        .items_center()
        .gap(px(12.0))
        .children(children)
}

impl Render for Gallery {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let this = cx.entity();
        let update = move |f: fn(&mut Gallery)| {
            let this = this.clone();
            move |_: &gpui::ClickEvent, _: &mut Window, cx: &mut gpui::App| {
                this.update(cx, |g, cx| {
                    f(g);
                    cx.notify();
                });
            }
        };
        let view = cx.entity();
        let set = move |f: Box<dyn Fn(&mut Gallery)>| {
            let view = view.clone();
            move |cx: &mut gpui::App| {
                view.update(cx, |g, cx| {
                    f(g);
                    cx.notify();
                });
            }
        };

        let buttons = row([
            Button::new("b-default", "Default").into_any_element(),
            Button::new("b-accent", "Accent")
                .variant(ButtonVariant::Accent)
                .into_any_element(),
            Button::new("b-danger", "Danger")
                .variant(ButtonVariant::Danger)
                .into_any_element(),
            Button::new("b-ghost", "Ghost")
                .variant(ButtonVariant::Ghost)
                .into_any_element(),
            Button::new("b-sm", "Small")
                .size(ButtonSize::Sm)
                .into_any_element(),
            Button::new("b-icon", "With icon")
                .icon(IconName::Plus)
                .into_any_element(),
            Button::new("b-hint", "Mute")
                .key_hint("M")
                .into_any_element(),
            Button::new("b-on", "Pressed")
                .pressed(true)
                .into_any_element(),
            Button::new("b-off", "Not pressed")
                .pressed(false)
                .into_any_element(),
            Button::new("b-disabled", "Disabled")
                .disabled(true)
                .into_any_element(),
            Button::new("b-acc-disabled", "Disabled")
                .variant(ButtonVariant::Accent)
                .disabled(true)
                .into_any_element(),
        ]);
        let icon_buttons = row([
            IconButton::new("ib-28", IconName::Search, "Search")
                .size(IconButtonSize::S28)
                .into_any_element(),
            IconButton::new("ib-32", IconName::Settings, "Settings")
                .keys(&["Ctrl", ","])
                .into_any_element(),
            IconButton::new("ib-40", IconName::Bell, "Notifications")
                .size(IconButtonSize::S40)
                .into_any_element(),
            IconButton::new("ib-44", IconName::Pin, "Pins")
                .size(IconButtonSize::S44)
                .into_any_element(),
            IconButton::new("ib-on", IconName::Mic, "Mute")
                .pressed(true)
                .into_any_element(),
            IconButton::new("ib-off", IconName::Mic, "Mute")
                .pressed(false)
                .into_any_element(),
            IconButton::new("ib-dis", IconName::Mic, "Mute")
                .disabled(true)
                .into_any_element(),
        ]);
        let marks = row([
            Pill::new("p-unread", PillKind::Unread, 3).into_any_element(),
            Pill::new("p-99", PillKind::Unread, 120).into_any_element(),
            Pill::new("p-mention", PillKind::Mention, 2).into_any_element(),
            Tag::new("t-bot", TagKind::Bot).into_any_element(),
            Tag::new("t-web", TagKind::Web).into_any_element(),
            Tag::new("t-admin", TagKind::Admin).into_any_element(),
            Tag::new("t-live", TagKind::Live).into_any_element(),
            Tag::new("t-playback", TagKind::PlaybackOnly).into_any_element(),
            Tag::new("t-readable", TagKind::Readable).into_any_element(),
            KeyHint::new("kh", &["Ctrl", "K"])
                .label("search")
                .into_any_element(),
            TrailingSlot::new("slot")
                .child(Pill::new("slot-pill", PillKind::Unread, 1))
                .into_any_element(),
        ]);
        let dots = row([
            StateDotTone::Ok,
            StateDotTone::Off,
            StateDotTone::Pending,
            StateDotTone::Idle,
            StateDotTone::Dnd,
            StateDotTone::Warn,
            StateDotTone::Danger,
        ]
        .into_iter()
        .enumerate()
        .flat_map(|(i, tone)| {
            [
                StateDot::new(("dot-sm", i), tone).into_any_element(),
                StateDot::new(("dot-md", i), tone)
                    .size(StateDotSize::Md)
                    .into_any_element(),
            ]
        }));
        let toggles = {
            let on = set(Box::new(|g| g.toggle = !g.toggle));
            let on = std::rc::Rc::new(on);
            row([
                {
                    let on = on.clone();
                    Toggle::new("tg-1", "Mute sounds", self.toggle)
                        .on_change(move |_, _, cx| on(cx))
                        .into_any_element()
                },
                Toggle::new("tg-2", "Off", false)
                    .show_label(true)
                    .into_any_element(),
                Toggle::new("tg-3", "Locked", true)
                    .disabled(true)
                    .hint("The host sets this")
                    .into_any_element(),
            ])
        };
        let seg = {
            let density = set(Box::new(|_| {}));
            let _ = density;
            let this = cx.entity();
            Segmented::new(
                "seg",
                &[
                    ("compact", "Compact"),
                    ("regular", "Regular"),
                    ("cozy", "Cozy"),
                ],
                self.density.clone(),
            )
            .on_change(move |v, _, cx| {
                let v = v.to_owned();
                this.update(cx, |g, cx| {
                    g.density = v;
                    cx.notify();
                });
            })
        };
        let select = {
            let this = cx.entity();
            Select::new(
                "sel",
                "Notifications",
                &[
                    ("a", "All messages"),
                    ("b", "Mentions only"),
                    ("c", "Nothing"),
                ],
                self.pick.clone(),
            )
            .hint("What this server may notify you about.")
            .on_change(move |v, _, cx| {
                let v = v.to_owned();
                this.update(cx, |g, cx| {
                    g.pick = v;
                    cx.notify();
                });
            })
        };
        let fields = div()
            .flex()
            .flex_col()
            .gap(px(14.0))
            .w(px(360.0))
            .child(
                dilla_desktop_ui::components::text_field::TextField::new(
                    "tf-name",
                    "Display name",
                    &self.name,
                )
                .hint("Shown to everyone in the server."),
            )
            .child(
                dilla_desktop_ui::components::text_field::TextField::multiline(
                    "tf-bio",
                    "About you",
                    &self.bio,
                )
                .error("That is too long."),
            );
        let roles = {
            let this = cx.entity();
            CheckList::new(
                "roles",
                "Roles",
                self.roles
                    .iter()
                    .enumerate()
                    .map(|(i, (name, on))| {
                        let item = CheckListItem::new(format!("r{i}"), *name, *on)
                            .detail("Can manage channels");
                        if i == 0 { item.color(0x5AA8E6) } else { item }
                    })
                    .collect(),
                "No roles yet.",
            )
            .busy(Some("r1".into()))
            .on_toggle(move |id, on, _, cx| {
                let i: usize = id[1..].parse().unwrap_or(0);
                this.update(cx, |g, cx| {
                    g.roles[i].1 = on;
                    cx.notify();
                });
            })
        };
        let cards = {
            let this = cx.entity();
            ChoiceCards::new(
                "factor",
                vec![
                    ChoiceCardOption::new(
                        "totp",
                        "Authenticator app",
                        "Six digits that change every 30 seconds",
                    )
                    .icon(IconName::Smartphone),
                    ChoiceCardOption::new("passkey", "Passkey", "Use this device's lock")
                        .icon(IconName::Fingerprint)
                        .disabled("This browser has no passkeys"),
                    ChoiceCardOption::new("key", "Security key", "Touch a hardware key")
                        .icon(IconName::Key),
                ],
                self.factor.clone(),
            )
            .on_change(move |v, _, cx| {
                let v = v.to_owned();
                this.update(cx, |g, cx| {
                    g.factor = v;
                    cx.notify();
                });
            })
        };
        let meters = row([0.0f32, 3.0, 6.0, 9.0, 12.0]
            .into_iter()
            .enumerate()
            .map(|(i, l)| AudioMeter::new(("meter", i), l).into_any_element()));
        let menu = {
            let (open, select) = (cx.entity(), cx.entity());
            ActionMenu::new(
                "menu",
                "Actions for Astrid",
                vec![
                    MenuItem::new("copy", "Copy link").icon(IconName::Link),
                    MenuItem::new("mute", "Mute")
                        .icon(IconName::Bell)
                        .check(true)
                        .hint("M"),
                    MenuItem::new("kick", "Kick")
                        .icon(IconName::UserX)
                        .danger()
                        .sep(),
                ],
            )
            .open(self.menu_open)
            .on_open_change(move |is_open, _, cx| {
                open.update(cx, |g, cx| {
                    g.menu_open = is_open;
                    cx.notify();
                });
            })
            .on_select(move |id, _, cx| {
                let id = id.to_owned();
                select.update(cx, |_, _| ());
                show_toast(format!("Chose {id}"), cx);
            })
        };
        let overlays = row([
            Button::new("open-dialog", "Open dialog")
                .on_click(update(|g| g.dialog = true))
                .into_any_element(),
            Button::new("open-confirm", "Open confirmation")
                .on_click(update(|g| g.confirm = true))
                .into_any_element(),
            Button::new("open-settings", "Open settings")
                .on_click(update(|g| g.settings = true))
                .into_any_element(),
            Button::new("show-toast", "Show a toast")
                .on_click(|_, _, cx| show_toast("Link copied", cx))
                .into_any_element(),
            menu.into_any_element(),
        ]);
        let people = div()
            .flex()
            .flex_col()
            .w(px(244.0))
            .child(Row::new("r-text", "general", RowKind::Text).active(true))
            .child(Row::new("r-unread", "announcements", RowKind::Announcement).unread(3))
            .child(Row::new("r-mention", "voice-lounge", RowKind::Voice).mentions(2))
            .child(
                Row::new("r-muted", "archive", RowKind::Text)
                    .muted(true)
                    .private(true),
            )
            .child(
                Row::new("r-dm", "Astrid", RowKind::Dm)
                    .person(Person {
                        sub: Some("Online".into()),
                        presence: Some(Presence::Online),
                        ..Person::default()
                    })
                    .unread(1),
            )
            .child(Row::new("r-group", "Crew", RowKind::Dm).person(Person {
                sub: Some("Astrid, Björn, Mira".into()),
                group: true,
                ..Person::default()
            }));
        let avatars = row([Density::Compact, Density::Regular, Density::Cozy]
            .into_iter()
            .enumerate()
            .map(|(i, d)| {
                Avatar::new(("av", i), "Astrid Lind")
                    .density(d)
                    .size(AvatarSize::Md)
                    .into_any_element()
            }));
        let groups = div()
            .flex()
            .flex_col()
            .w(px(640.0))
            .child(
                SettingsRow::new("sr-1", "Display name")
                    .hint("Shown to everyone in the server.")
                    .icon(IconName::User)
                    .position(GroupPosition::First)
                    .child(Button::new("sr-1-b", "Change")),
            )
            .child(
                SettingsRow::new("sr-2", "Reduced motion")
                    .tag("new")
                    .position(GroupPosition::Middle)
                    .child(Toggle::new("sr-2-t", "Reduced motion", false)),
            )
            .child(
                SettingsRow::new("sr-3", "Sign out everywhere")
                    .hint("Ends every session but this one.")
                    .position(GroupPosition::Last)
                    .menu(
                        "Row actions",
                        vec![MenuItem::new("out", "Sign out")],
                        Some("More actions"),
                        |_, _, _| {},
                    )
                    .child(Button::new("sr-3-b", "Sign out").variant(ButtonVariant::Danger)),
            )
            .child(
                div().pt(px(14.0)).child(
                    SettingsRow::new("sr-4", "Recovery key")
                        .layout(Layout::Stacked)
                        .hint("Shown once.")
                        .child(div().child("RK7M-2QXA-9T4C")),
                ),
            );
        let nav = SettingsNav::new(
            "gallery-nav",
            vec![
                NavItem::new("profile", "Profile")
                    .icon(IconName::User)
                    .group("Account"),
                NavItem::new("devices", "Devices")
                    .icon(IconName::Monitor)
                    .group("Account"),
                NavItem::new("voice", "Voice")
                    .icon(IconName::Mic)
                    .group("App"),
            ],
            self.section.clone(),
        )
        .on_select({
            let this = cx.entity();
            move |id, _, cx| {
                let id = id.to_owned();
                this.update(cx, |g, cx| {
                    g.section = id;
                    cx.notify();
                });
            }
        });
        let footer = SettingsFooter::new("gallery-footer")
            .status("Unsaved changes")
            .action(Button::new("sf-reset", "Reset"))
            .action(Button::new("sf-save", "Save").variant(ButtonVariant::Accent));

        let page = div()
            .id("gallery-page")
            .flex()
            .flex_col()
            .size_full()
            .overflow_y_scroll()
            .px(px(32.0))
            .pb(px(80.0))
            .bg(color::BG)
            .text_color(color::FG)
            .font_family(font::MONO)
            .text_size(text::BASE)
            .child(heading("Buttons"))
            .child(buttons)
            .child(heading("Icon buttons"))
            .child(icon_buttons)
            .child(heading("Pills, tags, key hints, slot"))
            .child(marks)
            .child(heading("State dots"))
            .child(dots)
            .child(heading("Toggles"))
            .child(toggles)
            .child(heading("Segmented and select"))
            .child(row([
                seg.into_any_element(),
                div().w(px(240.0)).child(select).into_any_element(),
            ]))
            .child(heading("Text fields"))
            .child(fields)
            .child(heading("Check list"))
            .child(div().w(px(420.0)).child(roles))
            .child(heading("Choice cards"))
            .child(div().w(px(420.0)).child(cards))
            .child(heading("Audio meter"))
            .child(meters)
            .child(heading("QR code"))
            .child(
                QrCode::new("qr", "otpauth://totp/dilla:ada?secret=JBSWY3DPEHPK3PXP")
                    .size(px(160.0)),
            )
            .child(heading("Banners and empty state"))
            .child(
                div()
                    .flex()
                    .flex_col()
                    .gap(px(8.0))
                    .w(px(640.0))
                    .child(Banner::new(
                        "bn-info",
                        BannerTone::Info,
                        "Voice is connecting.",
                    ))
                    .child(
                        Banner::new("bn-warn", BannerTone::Warn, "This server is slow.")
                            .action("Retry", |_, _, _| {}),
                    )
                    .child(Banner::new(
                        "bn-danger",
                        BannerTone::Danger,
                        "You were disconnected.",
                    ))
                    .child(LinkBanner::new(
                        "lb-off",
                        LinkStatus::Offline,
                        "dilla is offline",
                        "Connecting to dilla",
                    ))
                    .child(LinkBanner::new(
                        "lb-conn",
                        LinkStatus::Connecting,
                        "dilla is offline",
                        "Connecting to dilla",
                    ))
                    .child(
                        EmptyState::new(
                            "es",
                            "Nothing new",
                            "Mentions and direct messages show up here.",
                        )
                        .action("Start a server", |_, _, _| {})
                        .secondary("Join one", |_, _, _| {}),
                    ),
            )
            .child(heading("Rows and avatars"))
            .child(people)
            .child(avatars)
            .child(heading("Settings"))
            .child(row([nav.into_any_element()]))
            .child(groups)
            .child(div().w(px(640.0)).pt(px(14.0)).child(footer))
            .child(heading("Overlays"))
            .child(overlays);

        div()
            .relative()
            .size_full()
            .child(page)
            .child(
                Dialog::new("gallery-dialog", "Rename channel")
                    .open(self.dialog)
                    .body(div().child("Pick a new name."))
                    .footer(
                        Button::new("gd-save", "Rename")
                            .variant(ButtonVariant::Accent)
                            .size(ButtonSize::Dialog),
                    )
                    .on_close(update_close(cx, |g| g.dialog = false)),
            )
            .child(
                ConfirmDialog::new(
                    "gallery-confirm",
                    "Kick Astrid?",
                    "They can rejoin with a new invite.",
                    "Kick",
                    "Kicking",
                    "Cancel",
                )
                .open(self.confirm)
                .tone(Tone::Danger)
                .person("Astrid Lind")
                .on_close(update_close(cx, |g| g.confirm = false))
                .on_confirm(update_close(cx, |g| g.confirm = false)),
            )
            .child(
                SettingsFrame::new("gallery-settings", "Close settings")
                    .open(self.settings)
                    .nav(SettingsNav::new(
                        "frame-nav",
                        vec![
                            NavItem::new("profile", "Profile").icon(IconName::User),
                            NavItem::new("devices", "Devices").icon(IconName::Monitor),
                        ],
                        "profile",
                    ))
                    .footer(
                        SettingsFooter::new("frame-footer")
                            .status("Unsaved changes")
                            .action(Button::new("ff-save", "Save").variant(ButtonVariant::Accent)),
                    )
                    .on_close(update_close(cx, |g| g.settings = false))
                    .child(section_header("Profile", Some("How others see you.")))
                    .child(group_heading("Identity"))
                    .child(
                        SettingsRow::new("fr-1", "Display name").child(Icon::new(IconName::Check)),
                    ),
            )
            .child(ToastHost::new("gallery-toasts"))
    }
}

/// A close handler that changes the gallery's state and redraws.
fn update_close(
    cx: &Context<Gallery>,
    f: fn(&mut Gallery),
) -> impl Fn(&mut Window, &mut gpui::App) + 'static {
    let this = cx.entity();
    move |_, cx| {
        this.update(cx, |g, cx| {
            f(g);
            cx.notify();
        });
    }
}

fn main() {
    gpui_platform::application()
        .with_assets(dilla_desktop_ui::theme::Assets)
        .run(|cx| {
            cx.on_window_closed(|cx, _| {
                if cx.windows().is_empty() {
                    cx.quit();
                }
            })
            .detach();
            dilla_desktop::install(cx);
            let bounds = gpui::Bounds::centered(None, size(px(1100.0), px(900.0)), cx);
            cx.open_window(
                gpui::WindowOptions {
                    window_bounds: Some(gpui::WindowBounds::Windowed(bounds)),
                    ..Default::default()
                },
                |window, cx| {
                    window.set_window_title("Dilla components");
                    cx.new(|cx| Gallery::new(window, cx))
                },
            )
            .expect("the gallery window opens");
            cx.activate(true);
        });
}

#[allow(dead_code)]
fn _unused(_: SharedString) {}
