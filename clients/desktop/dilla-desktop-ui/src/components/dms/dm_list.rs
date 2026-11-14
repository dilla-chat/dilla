//! The Home sidebar's DM list (packages/ui ChannelList on `/dm`, ChannelList.css): the title in the server head's
//! display serif (Shell.tsx passes `titleKind="server"`), the square-pen "message someone" button and the host's menu
//! (the DM defaults' ⌄) in the head, the stats line (online, conversations), the section label in tracked
//! capitals over a hairline, and the DMs as person rows (Row with `person`, 42px). Automation ids: `dm-list`,
//! `dm-new`, `dm-<i>` (a row).

use std::rc::Rc;

use gpui::{
    App, ClickEvent, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::components::icon_button::{IconButton, IconButtonSize};
use crate::components::row::{Person, Row, RowKind};
use crate::icon_names::IconName;
use crate::metrics::LABEL_TRACKING_WIDE;
use crate::text::{Face, line, line_height};
use crate::tokens::{color, font, layout, text};

/// One DM as the list draws it.
#[derive(Clone, Debug, PartialEq)]
pub struct DmItem {
    pub name: SharedString,
    pub person: Person,
    pub active: bool,
    pub unread: u32,
    pub mentions: u32,
}

/// `.d-channel-list__section`: 14px above its label box, and the 1px hairline over it.
const SECTION_PAD_TOP: f32 = 14.0;
const HAIRLINE: f32 = 1.0;

type Select = Rc<dyn Fn(usize, &mut Window, &mut App) + 'static>;
type Click = Rc<dyn Fn(&ClickEvent, &mut Window, &mut App) + 'static>;

#[derive(IntoElement)]
pub struct DmList {
    title: SharedString,
    stats: Vec<SharedString>,
    section: SharedString,
    empty: SharedString,
    new_label: SharedString,
    items: Vec<DmItem>,
    on_select: Select,
    on_new: Click,
    menu: Option<gpui::AnyElement>,
}

impl DmList {
    /// `stats` are the stats line's parts (`shell.dms.online`, `shell.dms.count`), `section` the section label
    /// (`shell.dms.section`), `empty` the empty list's line (`shell.dms.empty`), `new_label` the head button's name
    /// (`shell.dms.new`).
    pub fn new(
        title: impl Into<SharedString>,
        stats: Vec<SharedString>,
        section: impl Into<SharedString>,
        empty: impl Into<SharedString>,
        new_label: impl Into<SharedString>,
        items: Vec<DmItem>,
    ) -> Self {
        Self {
            title: title.into(),
            stats,
            section: section.into(),
            empty: empty.into(),
            new_label: new_label.into(),
            items,
            on_select: Rc::new(|_, _, _| {}),
            on_new: Rc::new(|_, _, _| {}),
            menu: None,
        }
    }

    /// The head's menu after the "message someone" button (Shell.tsx `dmsMenu`, the host's own `ActionMenu`).
    pub fn menu(mut self, menu: impl IntoElement) -> Self {
        self.menu = Some(menu.into_any_element());
        self
    }

    /// A row was chosen (its index in the items).
    pub fn on_select(mut self, handler: impl Fn(usize, &mut Window, &mut App) + 'static) -> Self {
        self.on_select = Rc::new(handler);
        self
    }

    /// The head's "message someone" button.
    pub fn on_new(
        mut self,
        handler: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.on_new = Rc::new(handler);
        self
    }
}

impl RenderOnce for DmList {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let on_new = self.on_new.clone();
        let head = div()
            .flex_none()
            .flex()
            .items_center()
            .gap(px(8.0))
            .pt(px(14.0))
            .pb(px(8.0))
            .pl(px(14.0))
            .pr(px(12.0))
            .child(
                div().flex_1().min_w_0().flex().child(
                    line(
                        self.title,
                        text::TITLE,
                        line_height(text::TITLE, 1.5),
                        color::FG,
                    )
                    .face(Face::SerifItalic)
                    .clip(),
                ),
            )
            .child(
                IconButton::new("dm-new", IconName::SquarePen, self.new_label)
                    .size(IconButtonSize::S28)
                    .on_click(move |e, w, cx| on_new(e, w, cx)),
            )
            .children(self.menu);
        let meta = |s: SharedString| line(s, text::META, line_height(text::META, 1.5), color::FG_3);
        let mut stats = div()
            .flex_none()
            .flex()
            .gap(px(10.0))
            .px(px(14.0))
            .pb(px(10.0));
        for (i, s) in self.stats.into_iter().enumerate() {
            if i == 0 {
                stats = stats.child(
                    div()
                        .flex()
                        .items_center()
                        .gap(px(5.0))
                        .child(div().size(px(6.0)).rounded_full().bg(color::OK))
                        .child(meta(s)),
                );
            } else {
                stats = stats.child(meta("·".into())).child(meta(s));
            }
        }
        // `.d-channel-list__section` is content-box: its 1.625rem label box under 14px of padding and the hairline.
        let section = div()
            .flex_none()
            .flex()
            .items_center()
            .h(layout::ROW_H_SUB + px(SECTION_PAD_TOP + HAIRLINE))
            .pt(px(SECTION_PAD_TOP))
            .px(px(16.0))
            .border_t_1()
            .border_color(color::HAIRLINE)
            .child(
                line(
                    self.section.to_uppercase(),
                    text::MICRO,
                    line_height(text::MICRO, 1.5),
                    color::FG_3,
                )
                .weight(font::WEIGHT_LABEL)
                .tracking(LABEL_TRACKING_WIDE),
            );
        let rows: Vec<_> = if self.items.is_empty() {
            vec![
                div()
                    .px(px(8.0))
                    .child(line(
                        self.empty,
                        text::SM,
                        line_height(text::SM, 1.5),
                        color::FG_3,
                    ))
                    .into_any_element(),
            ]
        } else {
            self.items
                .into_iter()
                .enumerate()
                .map(|(i, item)| {
                    let select = self.on_select.clone();
                    div()
                        .mt(layout::LIST_GAP)
                        .child(
                            Row::new(("dm", i), item.name, RowKind::Dm)
                                .person(item.person)
                                .active(item.active)
                                .unread(item.unread)
                                .mentions(item.mentions)
                                .on_click(move |_, window, cx| select(i, window, cx)),
                        )
                        .into_any_element()
                })
                .collect()
        };
        div()
            .size_full()
            .flex()
            .flex_col()
            .child(head)
            .child(stats)
            .child(section)
            .child(
                div()
                    .id("dm-list")
                    .flex_1()
                    .min_h_0()
                    .overflow_y_scroll()
                    // `.d-channel-list__section + .d-channel-list__list`: no top padding under the label.
                    .px(px(8.0))
                    .flex()
                    .flex_col()
                    .children(rows),
            )
    }
}
