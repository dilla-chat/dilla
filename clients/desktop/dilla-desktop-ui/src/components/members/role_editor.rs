//! `RoleEditor` (packages/ui/src/RoleEditor, README §5.9): Server settings → Roles as the prototype draws it. A 200px list of
//! roles (colour dot, name, member count, then "New role") beside the selected role's editor: its name, the colour
//! swatches (`ColorSwatches`) and the permission groups of toggle rows (`PermissionGrid` in `toggle` mode). Every change
//! is reported at once; the screen saves it.
//!
//! The list is a CSS grid whose one implicit column is as wide as its widest row's content (`.d-role-editor__list`), in
//! a 200px track: a role whose name is longer than the track leaves its row wider than the column, under the editor, as
//! the web draws it. Here the rows sit in a track that does not shrink below its content and is at least the column wide.

use std::rc::Rc;

use gpui::{
    App, BoxShadow, ClickEvent, ElementId, Entity, FocusHandle, InteractiveElement, IntoElement,
    KeyDownEvent, MouseButton, MouseDownEvent, ParentElement, Pixels, Point, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, div, point, px, rgb,
};
use gpui_component::input::InputState;

use crate::components::text_field::TextField;
use crate::components::toggle::{Fit, Toggle};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{LABEL_TRACKING_WIDE, focus_ring};
use crate::roving::opens_context_menu;
use crate::text::{flow_paragraph, line, line_height};
use crate::tokens::{color, font, radius, text};

/// RoleEditor.css: the list column, the gap beside it, the editor's gaps.
pub const LIST_W: Pixels = px(200.);
pub const GRID_GAP: Pixels = px(18.);
pub const EDITOR_GAP: Pixels = px(16.);
/// `.d-role-editor__list`: rows 2px apart, 36px high, 10px in; the dot 10px; "New role"'s plus 14px, 8px from its label.
const LIST_GAP: Pixels = px(2.);
const ROW_PAD_X: Pixels = px(10.);
const ROW_GAP: Pixels = px(10.);
/// `.d-role-editor__role { height: 2.25rem }`.
const ROW_H: Pixels = px(36.);
const DOT: Pixels = px(10.);
const NEW_GAP: Pixels = px(8.);
const NEW_ICON: Pixels = px(14.);
/// `.d-role-editor__field { flex: 1 1 200px }`.
const FIELD_BASIS: Pixels = px(200.);
/// ColorSwatches.css: 24px squares 6px apart under the 6px-spaced label; the chosen one ringed by 2px of `--bg`, then
/// 1.5px more of its own colour (`0 0 0 2px var(--bg), 0 0 0 3.5px var(--d-swatch)`).
const SWATCH: Pixels = px(24.);
const SWATCH_GAP: Pixels = px(6.);
const RING_INNER: f32 = 2.;
const RING_OUTER: f32 = 3.5;
/// PermissionGrid.css: a group's heading padded 10px 14px 6px; a row 6px 14px, 12px between its text and its switch.
/// In the role editor a row is `box-sizing: content-box` (RoleEditor.css): 46px of content, the padding and the hairline.
const PAD_X: Pixels = px(14.);
const TITLE_TOP: Pixels = px(10.);
const TITLE_BOTTOM: Pixels = px(6.);
const ROW_PAD_Y: Pixels = px(6.);
const ROW_CONTENT_H: f32 = 46.;
const ROW_INNER_GAP: Pixels = px(12.);
/// `.d-permission-grid__text { line-height: 1.35 }`.
const TEXT_LEADING: f32 = 1.35;

/// One role of the list.
#[derive(Clone, Debug)]
pub struct EditorRole {
    /// The screen's key for the role, handed to the callbacks; the row's element id is `role-<id>`.
    pub id: SharedString,
    pub name: SharedString,
    /// 0xRRGGBB, or 0 for none (drawn in `--fg-2`).
    pub color: u32,
    pub count: u32,
}

/// One permission row: on is allow, off is inherit.
#[derive(Clone, Debug)]
pub struct PermToggle {
    pub id: SharedString,
    pub label: SharedString,
    pub hint: SharedString,
    pub on: bool,
    pub disabled: bool,
    /// Why it is disabled, drawn under it (`disabledHint`).
    pub reason: Option<SharedString>,
}

#[derive(Clone, Debug)]
pub struct PermGroup {
    pub title: SharedString,
    pub rows: Vec<PermToggle>,
}

/// The editor's fixed copy.
#[derive(Clone, Debug)]
pub struct EditorLabels {
    pub new_role: SharedString,
    pub name: SharedString,
    pub color: SharedString,
}

type OnId = Rc<dyn Fn(&str, &mut Window, &mut App)>;
type OnMenu = Rc<dyn Fn(&str, Point<Pixels>, &mut Window, &mut App)>;
type HasMenu = Rc<dyn Fn(&str) -> bool>;
type OnColor = Rc<dyn Fn(u32, &mut Window, &mut App)>;
type OnPermission = Rc<dyn Fn(&str, bool, &mut Window, &mut App)>;
type OnNew = Rc<dyn Fn(&mut Window, &mut App)>;
/// The automation build's bounds probe, laid over an element by its id.
pub type Probe = Rc<dyn Fn(&str) -> gpui::AnyElement>;

#[derive(IntoElement)]
pub struct RoleEditor {
    id: ElementId,
    roles: Vec<EditorRole>,
    selected: SharedString,
    /// The focus handles of the rows, then "New role" (the screen keeps them, so the arrows can move between them).
    focus: Vec<FocusHandle>,
    on_select: Option<OnId>,
    on_new: Option<OnNew>,
    /// Which rows have a context menu, and the handler that opens it.
    menu: Option<(HasMenu, OnMenu)>,
    name: Entity<InputState>,
    name_read_only: bool,
    name_error: Option<SharedString>,
    /// The swatches (value, name); none for @everyone, which has no colour.
    colors: Option<Vec<(u32, SharedString)>>,
    color: u32,
    on_color: Option<OnColor>,
    groups: Vec<PermGroup>,
    on_permission: Option<OnPermission>,
    labels: EditorLabels,
    status: Option<(SharedString, bool)>,
    probe: Option<Probe>,
}

impl RoleEditor {
    pub fn new(
        id: impl Into<ElementId>,
        roles: Vec<EditorRole>,
        selected: impl Into<SharedString>,
        name: &Entity<InputState>,
        labels: EditorLabels,
    ) -> Self {
        Self {
            id: id.into(),
            roles,
            selected: selected.into(),
            focus: Vec::new(),
            on_select: None,
            on_new: None,
            menu: None,
            name: name.clone(),
            name_read_only: false,
            name_error: None,
            colors: None,
            color: 0,
            on_color: None,
            groups: Vec::new(),
            on_permission: None,
            labels,
            status: None,
            probe: None,
        }
    }

    /// One focus handle per role, then one for "New role" when it is offered.
    pub fn focus(mut self, handles: Vec<FocusHandle>) -> Self {
        self.focus = handles;
        self
    }

    pub fn on_select(mut self, f: impl Fn(&str, &mut Window, &mut App) + 'static) -> Self {
        self.on_select = Some(Rc::new(f));
        self
    }

    /// Offers "New role" (absent when this account may not make roles).
    pub fn on_new(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_new = Some(Rc::new(f));
        self
    }

    /// A row's context menu (Ruling X): `has` says which rows have one; `open` opens it at a point.
    pub fn menu(
        mut self,
        has: impl Fn(&str) -> bool + 'static,
        open: impl Fn(&str, Point<Pixels>, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.menu = Some((Rc::new(has), Rc::new(open)));
        self
    }

    pub fn name_read_only(mut self, read_only: bool) -> Self {
        self.name_read_only = read_only;
        self
    }

    pub fn name_error(mut self, error: Option<SharedString>) -> Self {
        self.name_error = error;
        self
    }

    pub fn colors(mut self, colors: Option<Vec<(u32, SharedString)>>, chosen: u32) -> Self {
        self.colors = colors;
        self.color = chosen;
        self
    }

    pub fn on_color(mut self, f: impl Fn(u32, &mut Window, &mut App) + 'static) -> Self {
        self.on_color = Some(Rc::new(f));
        self
    }

    pub fn groups(mut self, groups: Vec<PermGroup>) -> Self {
        self.groups = groups;
        self
    }

    pub fn on_permission(
        mut self,
        f: impl Fn(&str, bool, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.on_permission = Some(Rc::new(f));
        self
    }

    /// The automation build's probe: each role row (`role-<id>`, and `role-open-<id>` for the open one), "New role"
    /// (`role-new`), the name field (`role-name`) and each switch (`perm-<key>`, with `perm-<key>-on` while it is on and
    /// `perm-<key>-locked` while it is disabled) report their bounds.
    pub fn probe(mut self, probe: Probe) -> Self {
        self.probe = Some(probe);
        self
    }

    /// The live line under the editor (saving, saved, refused); `true` is a refusal, in the danger colour.
    pub fn status(mut self, status: Option<(SharedString, bool)>) -> Self {
        self.status = status;
        self
    }
}

/// The chosen swatch's ring: its own colour outside, `--bg` between (the first CSS shadow is painted on top, GPUI paints
/// them in order).
fn ring(c: gpui::Hsla) -> Vec<BoxShadow> {
    let spread = |color, s: f32| BoxShadow {
        color,
        offset: point(px(0.), px(0.)),
        blur_radius: px(0.),
        spread_radius: px(s),
        inset: false,
    };
    vec![spread(c, RING_OUTER), spread(color::BG, RING_INNER)]
}

impl RenderOnce for RoleEditor {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let id_text: SharedString = format!("{:?}", self.id).into();
        let rows_n = self.roles.len() + usize::from(self.on_new.is_some());
        let focus = Rc::new(self.focus);
        let handle = |i: usize| focus.get(i).cloned();
        // Up and Down move between the rows and "New role", wrapping (RoleEditor.tsx `onListKey`).
        let arrows = {
            let focus = focus.clone();
            move |i: usize, e: &KeyDownEvent, window: &mut Window, cx: &mut App| -> bool {
                let m = &e.keystroke.modifiers;
                if m.modified() || rows_n == 0 || focus.len() < rows_n {
                    return false;
                }
                let next = match e.keystroke.key.as_str() {
                    "down" => (i + 1) % rows_n,
                    "up" => (i + rows_n - 1) % rows_n,
                    _ => return false,
                };
                window.focus(&focus[next], cx);
                true
            }
        };
        let arrows = Rc::new(arrows);

        let mut track = div()
            .flex_none()
            .min_w_full()
            .flex()
            .flex_col()
            .gap(LIST_GAP);
        for (i, role) in self.roles.iter().enumerate() {
            let dot = if role.color == 0 {
                color::FG_2
            } else {
                rgb(role.color).into()
            };
            let selected = role.id == self.selected;
            let has_menu = self.menu.as_ref().is_some_and(|(has, _)| has(&role.id));
            let laid_out: Rc<std::cell::Cell<Option<gpui::Bounds<Pixels>>>> = Rc::default();
            let record = laid_out.clone();
            let mut row = div()
                .id(SharedString::from(format!("role-{}", role.id)))
                .relative()
                .h(ROW_H)
                .px(ROW_PAD_X)
                .flex()
                .items_center()
                .gap(ROW_GAP)
                .rounded(radius::MD)
                .cursor_pointer()
                .hover(|s| s.bg(color::HOVER))
                .focus_visible(|s| s.shadow(focus_ring()))
                .child(
                    gpui::canvas(move |b, _, _| record.set(Some(b)), |_, _, _, _| {})
                        .absolute()
                        .top_0()
                        .left_0()
                        .size_full(),
                )
                .child(div().flex_none().size(DOT).rounded(radius::PILL).bg(dot))
                .child(
                    div().flex_1().min_w_0().flex().child(
                        line(
                            role.name.clone(),
                            text::SM,
                            line_height(text::SM, 1.5),
                            color::FG,
                        )
                        .ellipsis(),
                    ),
                )
                .child(line(
                    role.count.to_string(),
                    text::META,
                    line_height(text::META, 1.5),
                    color::FG_3,
                ));
            if selected {
                row = row.bg(color::SURFACE_HI);
            }
            if let Some(p) = &self.probe {
                row = row.child(p(&format!("role-{}", role.id)));
                if selected {
                    row = row.child(p(&format!("role-open-{}", role.id)));
                }
            }
            if let Some(h) = handle(i) {
                row = row.track_focus(&h.tab_stop(true));
            }
            if let Some(f) = self.on_select.clone() {
                let key = role.id.clone();
                row = row.on_click(move |_: &ClickEvent, window, cx| f(&key, window, cx));
            }
            let menu = has_menu
                .then(|| self.menu.as_ref().map(|(_, open)| open.clone()))
                .flatten();
            if let Some(open) = menu.clone() {
                let key = role.id.clone();
                row =
                    row.on_mouse_down(MouseButton::Right, move |e: &MouseDownEvent, window, cx| {
                        open(&key, e.position, window, cx);
                        cx.stop_propagation();
                    });
            }
            let (arrows, key) = (arrows.clone(), role.id.clone());
            row = row.on_key_down(move |e: &KeyDownEvent, window, cx| {
                if arrows(i, e, window, cx) {
                    cx.stop_propagation();
                } else if opens_context_menu(&e.keystroke)
                    && let Some(open) = &menu
                {
                    // From the keyboard the menu opens under the row (RoleEditor.tsx anchors it to the row).
                    let at = laid_out
                        .get()
                        .map_or_else(|| window.mouse_position(), |b| b.bottom_left());
                    open(&key, at, window, cx);
                    cx.stop_propagation();
                }
            });
            track = track.child(row);
        }
        if let Some(on_new) = self.on_new.clone() {
            let i = self.roles.len();
            let mut row = div()
                .id(SharedString::from(format!("{id_text}-new")))
                .debug_selector(|| "role-new".to_owned())
                .h(ROW_H)
                .px(ROW_PAD_X)
                .flex()
                .items_center()
                .gap(NEW_GAP)
                .rounded(radius::MD)
                .cursor_pointer()
                .hover(|s| s.bg(color::HOVER))
                .focus_visible(|s| s.shadow(focus_ring()))
                .child(
                    Icon::new(IconName::Plus)
                        .size(NEW_ICON)
                        .color(color::ACCENT),
                )
                .child(line(
                    self.labels.new_role.clone(),
                    text::UI,
                    line_height(text::UI, 1.5),
                    color::ACCENT,
                ))
                .relative()
                .children(self.probe.as_ref().map(|p| p("role-new")))
                .on_click(move |_: &ClickEvent, window, cx| on_new(window, cx));
            if let Some(h) = handle(i) {
                row = row.track_focus(&h.tab_stop(true));
            }
            let arrows = arrows.clone();
            row = row.on_key_down(move |e: &KeyDownEvent, window, cx| {
                if arrows(i, e, window, cx) {
                    cx.stop_propagation();
                }
            });
            track = track.child(row);
        }
        // The column is 200px; the track is at least that, and as wide as its widest row's content.
        let list = div().flex_none().w(LIST_W).flex().child(track);

        // The head: the name field (flex 1 1 200px) and the colours, wrapping, bottom aligned.
        let mut field = TextField::new(
            SharedString::from(format!("{id_text}-name")),
            self.labels.name.clone(),
            &self.name,
        )
        .disabled(self.name_read_only);
        if let Some(e) = self.name_error.clone() {
            field = field.error(e);
        }
        let mut head = div().flex().flex_wrap().items_end().gap(EDITOR_GAP).child(
            div()
                .flex_grow(1.)
                .flex_shrink(1.)
                .flex_basis(FIELD_BASIS)
                .min_w_0()
                .relative()
                .child(field)
                .children(self.probe.as_ref().map(|p| p("role-name"))),
        );
        if let Some(colors) = self.colors.clone() {
            let chosen = self.color;
            let mut group = div().flex().flex_wrap().gap(SWATCH_GAP);
            for (value, name) in colors {
                let c: gpui::Hsla = rgb(value).into();
                let mut swatch = div()
                    .id(SharedString::from(format!("role-colour-{value:06x}")))
                    .flex_none()
                    .size(SWATCH)
                    .rounded(radius::MD)
                    .bg(c)
                    .cursor_pointer()
                    .tab_index(0)
                    .focus_visible(|s| s.shadow(focus_ring()))
                    .tooltip(crate::components::tooltip::tooltip(name.clone(), &[]));
                if value == chosen {
                    swatch = swatch.shadow(ring(c));
                }
                if let Some(f) = self.on_color.clone() {
                    swatch = swatch.on_click(move |_: &ClickEvent, window, cx| {
                        if value != chosen {
                            f(value, window, cx);
                        }
                    });
                }
                group = group.child(swatch);
            }
            head = head.child(
                div()
                    .flex()
                    .flex_col()
                    .gap(SWATCH_GAP)
                    .min_w_0()
                    .child(line(
                        self.labels.color.clone(),
                        text::XS,
                        line_height(text::XS, 1.5),
                        color::FG_2,
                    ))
                    .child(group),
            );
        }

        // The permission groups: one 10px-radius card with a hairline edge.
        let mut grid = div()
            .flex()
            .flex_col()
            .min_w_0()
            .overflow_hidden()
            .border_1()
            .border_color(color::HAIRLINE)
            .rounded(radius::XL);
        for (g, group) in self.groups.into_iter().enumerate() {
            let mut title = div()
                .pt(TITLE_TOP)
                .pb(TITLE_BOTTOM)
                .px(PAD_X)
                .bg(color::BG_2)
                .child(
                    line(
                        group.title.to_uppercase(),
                        text::MICRO,
                        line_height(text::MICRO, 1.5),
                        color::FG_3,
                    )
                    .weight(font::WEIGHT_LABEL)
                    .tracking(LABEL_TRACKING_WIDE),
                );
            if g > 0 {
                title = title.border_t_1().border_color(color::HAIRLINE);
            }
            grid = grid.child(title);
            for row in group.rows {
                grid = grid.child(perm_row(
                    &id_text,
                    row,
                    self.on_permission.clone(),
                    self.probe.clone(),
                ));
            }
        }

        let mut editor = div()
            .flex_1()
            .min_w_0()
            .flex()
            .flex_col()
            .gap(EDITOR_GAP)
            .child(head)
            .child(grid);
        if let Some((status, alert)) = self.status
            && !status.is_empty()
        {
            editor = editor.child(line(
                status,
                text::XS,
                line_height(text::XS, 1.5),
                if alert { color::DANGER } else { color::FG_2 },
            ));
        }

        div()
            .id(self.id)
            .flex()
            .items_start()
            .gap(GRID_GAP)
            .child(list)
            .child(editor)
    }
}

/// A toggle row (`.d-permission-grid__row--toggle`): the name over its description at 1.35, the switch on the right, and
/// the reason it is disabled under both.
fn perm_row(
    id_text: &str,
    row: PermToggle,
    on_change: Option<OnPermission>,
    probe: Option<Probe>,
) -> gpui::AnyElement {
    // `.d-permission-grid__row--toggle .d-toggle__label` is `--fg` whether or not the row is disabled (the grid's
    // disabled `__name` rule does not reach a toggle row's label); the switch dims.
    let name_colour = color::FG;
    let words = div()
        .flex()
        .flex_col()
        .flex_1()
        .min_w_0()
        .child(flow_paragraph(
            row.label.clone(),
            text::SM,
            line_height(text::SM, TEXT_LEADING),
            name_colour,
        ))
        .child(flow_paragraph(
            row.hint.clone(),
            text::META,
            line_height(text::META, TEXT_LEADING),
            color::FG_3,
        ));
    let mut toggle = Toggle::new(
        SharedString::from(format!("perm-{}", row.id)),
        row.label.clone(),
        row.on,
    )
    .fit(Fit::Tight)
    .disabled(row.disabled);
    if let Some(f) = on_change {
        let (key, disabled) = (row.id.clone(), row.disabled);
        toggle = toggle.on_change(move |on, window, cx| {
            if !disabled {
                f(&key, on, window, cx);
            }
        });
    }
    let mut out = div()
        .id(SharedString::from(format!("{id_text}-row-{}", row.id)))
        .flex()
        .flex_wrap()
        .items_center()
        .gap(ROW_INNER_GAP)
        .min_h(px(ROW_CONTENT_H) + ROW_PAD_Y * 2. + px(1.))
        .px(PAD_X)
        .py(ROW_PAD_Y)
        .border_t_1()
        .border_color(color::HAIRLINE)
        .child(
            div()
                .w_full()
                .flex()
                .items_center()
                .gap(ROW_INNER_GAP)
                .child(words)
                .child(
                    div()
                        .flex_none()
                        .relative()
                        .debug_selector({
                            let (key, on) = (row.id.clone(), row.on);
                            move || {
                                if on {
                                    format!("perm-{key}-on")
                                } else {
                                    format!("perm-{key}")
                                }
                            }
                        })
                        .child(toggle)
                        .children(probe.as_ref().map(|p| p(&format!("perm-{}", row.id))))
                        // The switch's state, for the automation build: on, and disabled.
                        .children(
                            probe
                                .as_ref()
                                .filter(|_| row.on)
                                .map(|p| p(&format!("perm-{}-on", row.id))),
                        )
                        .children(
                            probe
                                .as_ref()
                                .filter(|_| row.disabled)
                                .map(|p| p(&format!("perm-{}-locked", row.id))),
                        ),
                ),
        );
    if let Some(reason) = row.reason {
        out = out.child(div().w_full().child(flow_paragraph(
            reason,
            text::META,
            line_height(text::META, 1.5),
            color::FG_2,
        )));
    }
    out.into_any_element()
}
