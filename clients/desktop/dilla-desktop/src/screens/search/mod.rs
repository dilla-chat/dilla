//! Area **search** (Task 23): the search palette (`packages/web/src/screens/{Search.tsx,palette.ts}`,
//! `packages/ui/src/SearchPalette`, Signal §5.8).
//!
//! A centred overlay of at most 600px at 14vh over the shell, which a scrim covers and the key trap keeps out of reach:
//! the input row (the accent glyph, the textbox, the esc key cap), the results in groups (Channels, People, Settings,
//! Messages, Actions: [`providers`]), and the footer. The textbox keeps the focus while the arrows move the active result
//! across the groups; Enter opens it; Escape closes the palette from anywhere in it and focus goes back to where it was
//! opened; Tab goes textbox, esc key cap, results.
//!
//! The message search runs over this device's own decrypted store (`dilla_client::Search`); the query is never sent
//! anywhere. A screen with no client behind it (a fixture) matches the messages its state holds with the same rule.
//!
//! **Hosting.** The shell is Task 8's. It calls [`provide`] on each render with what it shows and how a result is opened
//! ([`SearchEnv`]); `slots::overlays` calls [`palette`], which draws nothing until [`request_open`] (Ctrl+K) opens it. See
//! the task report for the lines the shell adds.

pub mod keys;
mod providers;
pub mod settings_index;

use std::collections::HashSet;
use std::rc::Rc;

use dilla_client::state::search::SearchHit;
use dilla_client::state::{ChannelId, CommunityId};
use dilla_desktop_ui::components::dialog::{
    HasOpenFocus, OVERLAY_CONTEXT, OpenFocus, TrapNext, TrapPrevious, sync_open_focus, trap_tab,
};
use dilla_desktop_ui::components::search::{self as ui, ResultRow};
use gpui::{
    AnyElement, App, AppContext as _, Context, Entity, EntityInputHandler, FocusHandle, Focusable,
    Global, InteractiveElement, IntoElement, MouseButton, ParentElement, ScrollHandle,
    SharedString, StatefulInteractiveElement, Styled, Subscription, Window, WindowId, deferred,
    div,
};
use gpui_component::input::{Input, InputEvent, InputState, MoveDown, MoveUp};

use self::providers::{Group, Inputs, Item};
use crate::router::{ServerSection, UserSection};
use crate::screens::FocusPrevious;
use crate::slots::SlotCx;
use crate::strings::{t, tf};

pub use self::providers::Person;

/// The key cap's text. It is a key's name, as the web draws it (`SearchPalette.tsx`), not copy.
const ESC_KEY: &str = "esc";

/// The position the instance gives its default AFK channel (`2^30`).
const AFK_POSITION: u32 = 1 << 30;

/// The overlay's paint priority over the shell (the dialog's).
const OVERLAY_PRIORITY: usize = 10;

/// What choosing a result does (`PaletteItem.run`).
#[derive(Clone, Debug)]
pub enum Navigate {
    /// Opens a channel of a server.
    Channel {
        community: CommunityId,
        channel: ChannelId,
    },
    /// Opens a direct message.
    Dm(ChannelId),
    /// Opens a message: its conversation, scrolled to it, or its thread when it is a reply.
    Message(SearchHit),
    /// Opens a section of the viewer's own settings.
    Settings(UserSection),
    /// Opens a section of a server's settings.
    ServerSettings {
        community: CommunityId,
        section: ServerSection,
    },
    /// The "Open settings" action: Profile.
    OpenSettings,
    /// The "Mute" action: toggles this device's microphone in the call.
    ToggleMute,
    /// The "Message someone" action: the direct-message picker.
    StartDm,
}

/// Answers whether the target was opened: `false` when it is gone (a hit whose conversation was left), which the palette
/// says and stays open for.
pub type OnNavigate = Rc<dyn Fn(Navigate, &mut Window, &mut App) -> bool>;

/// What the host (the shell) tells the palette: what it shows, and how a result is opened.
#[derive(Clone)]
pub struct SearchEnv {
    pub ctx: SlotCx,
    pub navigate: OnNavigate,
}

impl Global for SearchEnv {}

/// Lets go of `client` (Forget, sign-out: `crate::release::release_client`): the palette's environment goes (the next
/// render of a host provides a new one).
pub fn release(client: &std::rc::Rc<dilla_client::Client>, cx: &mut App) {
    if cx.try_global::<SearchEnv>().is_some_and(|env| {
        env.ctx
            .client
            .as_ref()
            .is_some_and(|c| std::rc::Rc::ptr_eq(c, client))
    }) {
        cx.remove_global::<SearchEnv>();
    }
}

/// The host's current view for the palette, set on each render of the host.
pub fn provide(env: SearchEnv, cx: &mut App) {
    cx.set_global(env);
}

/// Ctrl+K asked for the palette: it opens on the next frame.
#[derive(Default)]
struct OpenRequest(bool);

impl Global for OpenRequest {}

/// Asks for the palette to open (the key binding's action, and any button that opens search).
pub fn request_open(cx: &mut App) {
    cx.set_global(OpenRequest(true));
    cx.refresh_windows();
}

/// What the palette remembers between frames, one per window.
struct Palette {
    open: bool,
    input: Entity<InputState>,
    /// The id of the active result; `None` is the first (results that arrive later never change what Enter opens).
    active: Option<SharedString>,
    /// A picked result that is no longer there (`search.unavailable`).
    unavailable: bool,
    panel: FocusHandle,
    esc: FocusHandle,
    results: FocusHandle,
    scroll: ScrollHandle,
    focus: OpenFocus,
    /// The results as last drawn, for Enter: in list order.
    rows: Vec<Shown>,
    /// The status line as last drawn.
    status: String,
    _subscriptions: Vec<Subscription>,
}

/// A result as drawn.
struct Shown {
    id: SharedString,
    label: String,
    sub: Option<String>,
    target: Navigate,
}

impl HasOpenFocus for Palette {
    fn open_focus(&mut self) -> &mut OpenFocus {
        &mut self.focus
    }
}

#[derive(Default)]
struct Palettes(Vec<(WindowId, Entity<Palette>)>);

impl Global for Palettes {}

impl Palette {
    fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let input = cx.new(|cx| InputState::new(window, cx));
        let sub = cx.subscribe_in(&input, window, Self::on_input);
        Self {
            open: false,
            input,
            active: None,
            unavailable: false,
            panel: cx.focus_handle().tab_stop(true),
            esc: cx.focus_handle().tab_stop(true),
            results: cx.focus_handle().tab_stop(true),
            scroll: ScrollHandle::new(),
            focus: OpenFocus::default(),
            rows: Vec::new(),
            status: String::new(),
            _subscriptions: vec![sub],
        }
    }

    fn on_input(
        &mut self,
        input: &Entity<InputState>,
        event: &InputEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            InputEvent::Change => {
                self.active = None;
                self.unavailable = false;
                let query = input.read(cx).value().to_string();
                search_messages(&query, cx);
                window.refresh();
            }
            InputEvent::PressEnter { .. } => {
                // Enter during an input-method composition confirms it and opens nothing.
                if input
                    .update(cx, |s, cx| s.marked_text_range(window, cx))
                    .is_some()
                {
                    return;
                }
                let first = self.rows.first().map(|r| r.id.clone());
                if let Some(id) = self.active.clone().or(first) {
                    let this = cx.entity();
                    window.defer(cx, move |window, cx| pick(&this, &id, window, cx));
                }
            }
            _ => {}
        }
    }
}

/// Starts the message search for `query` on a live client (two or more characters: the palette's own gate).
fn search_messages(query: &str, cx: &mut App) {
    let Some(client) = cx
        .try_global::<SearchEnv>()
        .and_then(|e| e.ctx.client.clone())
    else {
        return;
    };
    let query = query.trim();
    if query.encode_utf16().count() < 2 {
        client.search().clear();
        return;
    }
    let answer = client.search().query(query);
    cx.background_spawn(async move {
        let _ = answer.await;
    })
    .detach();
}

/// The palette of `window`, made on first use.
fn palette_of(window: &mut Window, cx: &mut App) -> Entity<Palette> {
    let id = window.window_handle().window_id();
    if let Some((_, p)) = cx
        .try_global::<Palettes>()
        .and_then(|ps| ps.0.iter().find(|(w, _)| *w == id))
    {
        return p.clone();
    }
    let palette = cx.new(|cx| Palette::new(window, cx));
    if !cx.has_global::<Palettes>() {
        cx.set_global(Palettes::default());
    }
    cx.global_mut::<Palettes>().0.push((id, palette.clone()));
    palette
}

/// Closes the palette. `keep_focus`: the result opened took the focus on purpose, so it is not handed back to the opener.
fn close(palette: &Entity<Palette>, keep_focus: bool, window: &mut Window, cx: &mut App) {
    palette.update(cx, |p, _| {
        p.open = false;
        p.active = None;
        p.unavailable = false;
        if keep_focus {
            p.focus = OpenFocus::default();
        }
    });
    if let Some(client) = cx
        .try_global::<SearchEnv>()
        .and_then(|e| e.ctx.client.clone())
    {
        client.search().clear();
    }
    window.refresh();
}

/// Opens the result `id`: it runs, and the palette closes; one that is no longer there leaves the palette open and says so.
/// Never called inside an update of the palette (the host it asks may look at it).
fn pick(palette: &Entity<Palette>, id: &SharedString, window: &mut Window, cx: &mut App) {
    let target = palette
        .read(cx)
        .rows
        .iter()
        .find(|r| &r.id == id)
        .map(|r| r.target.clone());
    let (Some(target), Some(env)) = (target, cx.try_global::<SearchEnv>().cloned()) else {
        return;
    };
    if (env.navigate)(target, window, cx) {
        close(palette, true, window, cx);
    } else {
        palette.update(cx, |p, _| p.unavailable = true);
        let input = palette.read(cx).input.clone();
        window.focus(&input.read(cx).focus_handle(cx), cx);
        window.refresh();
    }
}

/// The names `tracked` gives the result rows: a row's automation id is `search-option-<n>`.
fn option_name(index: usize) -> &'static str {
    use std::sync::OnceLock;
    static NAMES: OnceLock<Vec<&'static str>> = OnceLock::new();
    let names = NAMES.get_or_init(|| {
        (0..64)
            .map(|i| &*Box::leak(format!("search-option-{i}").into_boxed_str()))
            .collect()
    });
    names.get(index).copied().unwrap_or("search-option-n")
}

/// An element the automation interface can find by name and ask whether it holds the focus.
fn probed<E: ParentElement>(id: &'static str, focus: Option<&FocusHandle>, mut element: E) -> E {
    #[cfg(feature = "automation")]
    {
        element.extend(std::iter::once(
            crate::automation::bounds_probe(id).into_any_element(),
        ));
        if let Some(handle) = focus {
            element.extend(std::iter::once(
                crate::automation::focus_probe(id, handle).into_any_element(),
            ));
        }
    }
    #[cfg(not(feature = "automation"))]
    let _ = (id, focus, &mut element);
    element
}

/// What the palette shows now, for tests and tools that drive it.
pub struct Inspect {
    pub open: bool,
    /// `(label, sub-line)` of every result, in list order.
    pub rows: Vec<(String, Option<String>)>,
    /// The id of the active result.
    pub active: Option<String>,
    /// The status line.
    pub status: String,
    pub input: FocusHandle,
    pub close: FocusHandle,
    pub results: FocusHandle,
}

/// The palette of `window` as last drawn; `None` before it was first drawn.
pub fn inspect(window: &Window, cx: &App) -> Option<Inspect> {
    let id = window.window_handle().window_id();
    let palette = cx
        .try_global::<Palettes>()?
        .0
        .iter()
        .find(|(w, _)| *w == id)?
        .1
        .clone();
    let p = palette.read(cx);
    Some(Inspect {
        open: p.open,
        rows: p
            .rows
            .iter()
            .map(|r| (r.label.clone(), r.sub.clone()))
            .collect(),
        active: p
            .active
            .clone()
            .or_else(|| p.rows.first().map(|r| r.id.clone()))
            .map(|a| a.to_string()),
        status: p.status.clone(),
        input: p.input.read(cx).focus_handle(cx),
        close: p.esc.clone(),
        results: p.results.clone(),
    })
}

/// The search palette (`slots::overlays`): `None` while it is closed.
pub fn palette(window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let env = cx.try_global::<SearchEnv>().cloned()?;
    let memory = palette_of(window, cx);
    if std::mem::take(&mut cx.default_global::<OpenRequest>().0) && !memory.read(cx).open {
        let input = memory.read(cx).input.clone();
        input.update(cx, |s, cx| s.set_value("", window, cx));
        memory.update(cx, |p, _| {
            p.open = true;
            p.active = None;
            p.unavailable = false;
        });
    }
    let (open, input, panel, esc, results, scroll) = memory.read_with(cx, |p, _| {
        (
            p.open,
            p.input.clone(),
            p.panel.clone(),
            p.esc.clone(),
            p.results.clone(),
            p.scroll.clone(),
        )
    });
    // Focus goes to the textbox on open and back to what had it when the palette closes.
    let textbox = input.read(cx).focus_handle(cx);
    sync_open_focus(&memory, open, &panel, textbox.clone(), window, cx);
    if !open {
        return None;
    }

    let state = env.ctx.state.clone();
    let value = input.read(cx).value().to_string();
    let trimmed = value.trim().to_owned();
    // The hits for what is typed now: the client's newest search when it is for this query, else (no client) the state's own.
    let (hits, loading) = if env.ctx.client.is_some() {
        let s = &state.search;
        if s.query == trimmed {
            (s.hits.clone(), s.loading)
        } else {
            (Vec::new(), true)
        }
    } else {
        (
            dilla_client::features::search::local_hits(&state, &trimmed),
            false,
        )
    };
    // The AFK destination is never listed. Until the server settings area (15) publishes it, it is the instance's own
    // default one: the voice channel at the system position.
    let afk: HashSet<_> = state
        .channels
        .iter()
        .filter(|c| c.kind == 1 && c.position == AFK_POSITION)
        .map(|c| c.id)
        .collect();
    let groups: Vec<Group> = providers::groups(
        &Inputs {
            state: &state,
            community: env.ctx.community,
            current: env.ctx.channel,
            clock: env.ctx.clock,
            hits: &hits,
            people: &[],
            afk: &afk,
            muted: false,
        },
        &value,
    );
    let items: Vec<&Item> = groups.iter().flat_map(|g| g.items.iter()).collect();
    let ids: Vec<SharedString> = items
        .iter()
        .map(|i| SharedString::from(i.id.clone()))
        .collect();
    let active_ix = memory
        .read(cx)
        .active
        .as_ref()
        .and_then(|a| ids.iter().position(|id| id == a))
        .unwrap_or(0);
    let active_id = ids.get(active_ix).cloned();
    memory.update(cx, |p, _| {
        p.rows = items
            .iter()
            .map(|i| Shown {
                id: SharedString::from(i.id.clone()),
                label: i.label.clone(),
                sub: i.sub.clone(),
                target: i.target.clone(),
            })
            .collect();
    });
    // The child of the results list each item is, for scrolling it into view: a heading is a child too.
    let mut child_of = Vec::with_capacity(items.len());
    let mut children = 0usize;
    for g in &groups {
        children += 1;
        for _ in &g.items {
            child_of.push(children);
            children += 1;
        }
    }

    let status_text: String = if memory.read(cx).unavailable {
        t("search.unavailable").to_owned()
    } else if !items.is_empty() || trimmed.is_empty() {
        String::new()
    } else if loading && trimmed.encode_utf16().count() >= 2 {
        t("search.loading").to_owned()
    } else {
        tf("search.empty", &[("query", &trimmed)])
    };

    memory.update(cx, |p, _| p.status = status_text.clone());
    let viewport = window.viewport_size();
    let (vw, vh) = (f32::from(viewport.width), f32::from(viewport.height));

    // The input row.
    let field_input = probed(
        "search-input",
        Some(&textbox),
        ui::input_box()
            // The component pads a single-line input by its size; the palette's textbox has no side padding.
            .child(
                Input::new(&input)
                    .appearance(false)
                    .ml(-gpui_component::Size::default().input_px()),
            )
            .children(
                value
                    .is_empty()
                    .then(|| ui::placeholder(t("search.placeholder"))),
            ),
    );
    let on_esc = {
        let memory = memory.clone();
        move |window: &mut Window, cx: &mut App| close(&memory, false, window, cx)
    };
    let esc_key = probed(
        "search-close",
        Some(&esc),
        ui::esc_key("search-close", ESC_KEY)
            .track_focus(&esc)
            .on_click({
                let on_esc = on_esc.clone();
                move |_, window, cx| on_esc(window, cx)
            })
            .on_key_down({
                let on_esc = on_esc.clone();
                move |event, window, cx| {
                    if matches!(event.keystroke.key.as_str(), "enter" | "space") {
                        cx.stop_propagation();
                        on_esc(window, cx);
                    }
                }
            }),
    );
    let field = ui::field()
        .child(ui::field_glyph())
        .child(field_input)
        .child(esc_key);

    // The results.
    let mut list = ui::results("search-results")
        .track_focus(&results.clone().tab_stop(!items.is_empty()))
        .track_scroll(&scroll);
    let mut at = 0usize;
    for g in &groups {
        list = list.child(ui::heading(g.label.clone()));
        for item in &g.items {
            let id = SharedString::from(item.id.clone());
            let selected = Some(&id) == active_id.as_ref();
            let row = ResultRow::new(id.clone(), item.glyph.clone(), item.label.clone())
                .subline(item.sub.clone().map(Into::into))
                .selected(selected)
                .on_click({
                    let (memory, id) = (memory.clone(), id.clone());
                    move |window, cx| pick(&memory, &id, window, cx)
                })
                .on_hover({
                    let (memory, id) = (memory.clone(), id.clone());
                    move |window, cx| {
                        let changed = memory.update(cx, |p, _| {
                            let changed = p.active.as_ref() != Some(&id);
                            p.active = Some(id.clone());
                            changed
                        });
                        if changed {
                            window.refresh();
                        }
                    }
                });
            let marked = if selected {
                probed("search-active", None, div().child(row))
            } else {
                div().child(row)
            };
            list = list.child(probed(option_name(at), None, marked));
            at += 1;
        }
    }
    list = list.children(ui::status(status_text));
    let list = probed(
        "search-results",
        Some(&results),
        div().flex().flex_col().child(list),
    );

    // Arrows move the active result across the groups and never the focus (the textbox's own Up and Down are never typed).
    let step = {
        let (memory, ids, child_of, scroll) = (
            memory.clone(),
            Rc::new(ids.clone()),
            Rc::new(child_of),
            scroll.clone(),
        );
        move |by: i32, window: &mut Window, cx: &mut App| {
            let current = memory.read(cx).active.clone();
            if let Some(next) = ui::step_active(&ids, current.as_deref(), by) {
                memory.update(cx, |p, _| p.active = Some(ids[next].clone()));
                scroll.scroll_to_item(child_of[next]);
                window.refresh();
            }
        }
    };
    let (down, up) = (step.clone(), step);
    let (next, prev) = (panel.clone(), panel.clone());
    let (first, last_stop) = (
        textbox.clone(),
        if items.is_empty() {
            esc.clone()
        } else {
            results.clone()
        },
    );
    let key_memory = memory.clone();
    let key_input = input.clone();
    let panel_el = ui::panel("search-panel", vw, vh)
        .track_focus(&panel)
        .key_context(OVERLAY_CONTEXT)
        .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
        .capture_action(move |_: &MoveDown, window, cx| {
            cx.stop_propagation();
            down(1, window, cx);
        })
        .capture_action(move |_: &MoveUp, window, cx| {
            cx.stop_propagation();
            up(-1, window, cx);
        })
        // Tab and Shift+Tab stay in the palette whichever action the keymap gives them (the overlay's trap, or the
        // window's own focus keys).
        .capture_action({
            let (panel, textbox) = (next.clone(), first.clone());
            let last = last_stop.clone();
            move |_: &TrapNext, window, cx| {
                cx.stop_propagation();
                // From the last stop it wraps to the textbox.
                if last.is_focused(window) {
                    textbox.focus(window, cx);
                } else {
                    trap_tab(&panel, true, window, cx);
                }
            }
        })
        .capture_action({
            let (panel, textbox, last) = (prev.clone(), first.clone(), last_stop.clone());
            move |_: &TrapPrevious, window, cx| {
                cx.stop_propagation();
                // Before the first stop (the textbox) it wraps to the last one.
                if textbox.is_focused(window) {
                    last.focus(window, cx);
                } else {
                    trap_tab(&panel, false, window, cx);
                }
            }
        })
        .capture_action({
            let (panel, textbox, last) = (prev.clone(), first.clone(), last_stop.clone());
            move |_: &FocusPrevious, window, cx| {
                cx.stop_propagation();
                if textbox.is_focused(window) {
                    last.focus(window, cx);
                } else {
                    trap_tab(&panel, false, window, cx);
                }
            }
        })
        // Escape closes from anywhere in the palette, never during an input-method composition.
        .on_key_down(move |event, window, cx| {
            if event.keystroke.key == "escape" {
                cx.stop_propagation();
                let composing = key_input
                    .update(cx, |s, cx| s.marked_text_range(window, cx))
                    .is_some();
                if !composing {
                    close(&key_memory, false, window, cx);
                }
            }
        })
        .child(field)
        .child(list)
        .child(ui::foot(
            t("layout.search.hint.move"),
            t("layout.search.hint.open"),
            t("search.note"),
        ));

    // A press on the scrim closes the palette.
    let scrim_close = {
        let memory = memory.clone();
        move |_: &gpui::MouseDownEvent, window: &mut Window, cx: &mut App| {
            close(&memory, false, window, cx)
        }
    };
    Some(
        deferred(
            ui::scrim("search-palette", vh)
                .on_mouse_down(MouseButton::Left, scrim_close)
                .child(panel_el),
        )
        .with_priority(OVERLAY_PRIORITY)
        .into_any_element(),
    )
}
