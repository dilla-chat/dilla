//! The sign-up's last step (Onboarding.tsx `shown === 'done'`, step 6 of 6): "You're in", the panel naming the server the
//! invite joined and its first text channel (only when there is one), the small print, the backup codes, and "Open
//! dilla". A join refused after the factor is the result, never a failed sign-up (ruling 34): the shell offers it again
//! in the join dialog.
//!
//! The backup codes are the ones the factor step's add returned (Onboarding.tsx `finishFactor`, Task 3b): their copy
//! (`onboarding.done.codesBody`), a summary with the count, Copy, Save as file and Show/Hide (the list is collapsed until
//! shown, as the web's disclosure), and the tick "I saved my backup codes somewhere safe", which "Open dilla" waits for.
//! The web's Print is the browser's; the desktop saves a file instead (as the key step's Save as file). The codes live
//! in this view only, in a zeroizing buffer: never a log, prefs or the store, and wiped when Open dilla leaves the step
//! (and when the view drops).

use dilla_client::features::factors::BackupCodes;
use dilla_client::features::signup::{self as su, SignupResult};
use dilla_client::state::{AppState, Channel, CommunityId};
use gpui::prelude::FluentBuilder;
use gpui::{
    AppContext as _, ClipboardItem, Context, EventEmitter, FocusHandle, InteractiveElement,
    IntoElement, ParentElement, Render, SharedString, Styled, Window, div, px,
};
use zeroize::Zeroizing;

use dilla_desktop_ui::components::banner::{Banner, BannerTone};
use dilla_desktop_ui::components::button::Button;
use dilla_desktop_ui::components::factor::{CodeList, code_list_columns, copy_text};
use dilla_desktop_ui::components::onboarding::frame::CARD_INNER_W;
use dilla_desktop_ui::components::onboarding::{
    KeyAck, OnboardingFrame, PrimaryAction, ReadyPanel, frame,
};
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, text};

use super::keys::Submit;
use super::signup::TOTAL;
use crate::screens::{FocusNext, FocusPrevious};
use crate::strings::{t, tf};
use crate::{tracked, tracked_box, tracked_focus};

/// The name Save as file suggests. The web prints its codes and has no file of them, so no copy names one; this is
/// the key file's pattern (`onboarding.keys.fileName`) for the codes.
const CODES_FILE: &str = "dilla-backup-codes.txt";
/// The refusal a failed Save as file shows (the key step's: the web has no `onboarding.keys.saveFailed`).
const SAVE_FAILED: &str = "settings.editing.saveFailed";
/// The gap between the summary's actions, and between the count and them (CodeList.css `__actions`, 0.75rem).
const ACTIONS_GAP: f32 = 12.0;
/// The gap between the codes' parts (the list, the summary, the tick).
const CODES_GAP: f32 = 10.0;

/// "Open dilla": the shell opens on the joined server.
pub struct OpenDilla {
    pub result: SignupResult,
}

/// The sign-up's backup codes on their way to the done step across a window handover (`Launch::signup_codes`): the
/// one holder, taken once by the window that shows the step. Never printed.
#[derive(Clone, Default)]
pub struct SignupCodes(std::rc::Rc<std::cell::Cell<Option<BackupCodes>>>);

impl SignupCodes {
    pub fn new(codes: Option<BackupCodes>) -> Self {
        Self(std::rc::Rc::new(std::cell::Cell::new(codes)))
    }

    /// The codes, once.
    pub fn take(&self) -> Option<BackupCodes> {
        self.0.take()
    }
}

/// The focus handles of the codes' controls (the automation build's `focused`, and Tab's order).
struct CodeHandles {
    copy: FocusHandle,
    save: FocusHandle,
    show: FocusHandle,
    ack: FocusHandle,
}

pub struct DoneView {
    result: SignupResult,
    /// The joined server's name and its first text channel's, once listed.
    ready: Option<(String, String)>,
    open: FocusHandle,
    /// The factor's backup codes, until Open dilla (empty: the account already held unused codes).
    codes: Zeroizing<Vec<String>>,
    /// The disclosure: the list is drawn only while shown.
    expanded: bool,
    /// "I saved my backup codes somewhere safe".
    saved: bool,
    /// Why the last Save as file did not write.
    problem: Option<SharedString>,
    handles: CodeHandles,
}

impl EventEmitter<OpenDilla> for DoneView {}

/// The server's first text channel in the channel list's order (shell-model.ts `orderChannels`, text in mode 0).
pub fn first_text_channel(state: &AppState, community: CommunityId) -> Option<&Channel> {
    let mut channels: Vec<&Channel> = state
        .channels
        .iter()
        .filter(|c| c.community_id == community)
        .collect();
    channels.sort_by_key(|c| (c.parent_id.is_some(), c.position));
    channels.into_iter().find(|c| c.is_text())
}

/// The text Save as file writes: the codes' title and copy, then the codes as Copy gives them (one per line, grouped).
fn codes_file(codes: &[String]) -> Zeroizing<String> {
    let listed = Zeroizing::new(copy_text(codes));
    let mut out = Zeroizing::new(String::with_capacity(listed.len() + 256));
    out.push_str(t("factor.codes.title"));
    out.push_str("\n\n");
    out.push_str(t("onboarding.done.codesBody"));
    out.push_str("\n\n");
    out.push_str(&listed);
    out.push('\n');
    out
}

impl DoneView {
    /// `codes`: the backup codes the sign-up's factor step was given (none: nothing to save, Open dilla is ready).
    pub fn new(
        result: SignupResult,
        codes: Option<BackupCodes>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let open = cx.focus_handle().tab_stop(true);
        window.focus(&open, cx);
        Self {
            result,
            ready: None,
            open,
            codes: codes.map(|c| c.0).unwrap_or_default(),
            expanded: false,
            saved: false,
            problem: None,
            handles: CodeHandles {
                copy: cx.focus_handle().tab_stop(true),
                save: cx.focus_handle().tab_stop(true),
                show: cx.focus_handle().tab_stop(true),
                ack: cx.focus_handle().tab_stop(true),
            },
        }
    }

    /// The client's newest state: the panel names the joined server once its channels are listed.
    pub fn set_state(&mut self, state: &AppState, cx: &mut Context<Self>) {
        let ready = self.result.community.and_then(|id| {
            let server = state.communities.iter().find(|c| c.id == id)?;
            let channel = first_text_channel(state, id)?;
            Some((server.name.clone(), channel.name.clone()))
        });
        if ready != self.ready {
            self.ready = ready;
            cx.notify();
        }
    }

    /// The codes still wait for their tick.
    fn owed(&self) -> bool {
        !self.codes.is_empty() && !self.saved
    }

    /// Open dilla: refused while the codes wait for their tick (Onboarding.tsx `open`); the codes are wiped as the step
    /// is left.
    fn open(&mut self, cx: &mut Context<Self>) {
        if self.owed() {
            return;
        }
        self.codes = Zeroizing::new(Vec::new());
        self.expanded = false;
        cx.emit(OpenDilla {
            result: self.result.clone(),
        });
        cx.notify();
    }

    fn toggle_shown(&mut self, cx: &mut Context<Self>) {
        self.expanded = !self.expanded;
        cx.notify();
    }

    fn toggle_saved(&mut self, cx: &mut Context<Self>) {
        self.saved = !self.saved;
        cx.notify();
    }

    fn copy(&self, cx: &mut Context<Self>) {
        let text = Zeroizing::new(copy_text(&self.codes));
        cx.write_to_clipboard(ClipboardItem::new_string(text.as_str().to_owned()));
    }

    /// Save as file: the codes once, in the file the person picks, written as the key file is (0600, never through a
    /// link), never anywhere else.
    fn save(&mut self, cx: &mut Context<Self>) {
        if self.codes.is_empty() {
            return;
        }
        let body = codes_file(&self.codes);
        let dir = std::env::home_dir()
            .map(|h| h.join("Downloads"))
            .filter(|d| d.is_dir())
            .or_else(std::env::home_dir)
            .unwrap_or_default();
        let chosen = cx.prompt_for_new_path(&dir, Some(CODES_FILE));
        self.problem = None;
        cx.spawn(async move |this, cx| {
            let Ok(Ok(Some(path))) = chosen.await else {
                return;
            };
            let written = cx
                .background_spawn(async move { su::write_key_file(&path, body.as_bytes()) })
                .await;
            if written.is_err() {
                let _ = this.update(cx, |this, cx| {
                    this.problem = Some(t(SAVE_FAILED).into());
                    cx.notify();
                });
            }
        })
        .detach();
    }

    /// The codes' block: the copy, the summary (count, Copy, Save as file, Show/Hide), the list while shown, the tick.
    fn codes_block(&self, cx: &mut Context<Self>) -> gpui::AnyElement {
        let count = tf(
            "onboarding.done.codesCount",
            &[("count", &self.codes.len().to_string())],
        );
        let toggle = if self.expanded {
            t("onboarding.done.codesHide")
        } else {
            t("onboarding.done.codesShow")
        };
        let actions = div()
            .flex()
            .flex_wrap()
            .gap(px(ACTIONS_GAP))
            .child(tracked_focus(
                "onboarding-codes-copy",
                &self.handles.copy,
                Button::new("onboarding-codes-copy", t("factor.codes.copy"))
                    .focus_handle(&self.handles.copy)
                    .on_click(cx.listener(|this, _, _, cx| this.copy(cx))),
            ))
            .child(tracked_focus(
                "onboarding-codes-save",
                &self.handles.save,
                Button::new("onboarding-codes-save", t("onboarding.keys.save"))
                    .focus_handle(&self.handles.save)
                    .on_click(cx.listener(|this, _, _, cx| this.save(cx))),
            ))
            .child(tracked_focus(
                "onboarding-codes-show",
                &self.handles.show,
                Button::new("onboarding-codes-show", toggle)
                    .focus_handle(&self.handles.show)
                    .pressed(self.expanded)
                    .on_click(cx.listener(|this, _, _, cx| this.toggle_shown(cx))),
            ));
        let summary = div()
            .w(px(CARD_INNER_W))
            .flex()
            .flex_wrap()
            .items_center()
            .justify_between()
            .gap(px(ACTIONS_GAP))
            .child(line(
                count,
                text::UI,
                line_height(text::UI, 1.5),
                color::FG_STRONG,
            ))
            .child(actions);
        let list = self.expanded.then(|| {
            tracked_box(
                "onboarding-codes",
                CodeList::new("onboarding-codes", t("factor.codes.label"), &self.codes)
                    .columns(code_list_columns(CARD_INNER_W)),
            )
        });
        let ack = tracked_focus(
            "onboarding-codes-ack",
            &self.handles.ack,
            KeyAck::new(
                "onboarding-codes-ack",
                t("onboarding.done.codesAck"),
                self.saved,
            )
            .focus(&self.handles.ack)
            .on_click(cx.listener(|this, _, _, cx| this.toggle_saved(cx))),
        );
        div()
            .flex()
            .flex_col()
            .gap(px(CODES_GAP))
            .child(frame::body_paragraph(
                t("onboarding.done.codesBody"),
                color::FG_2,
            ))
            .when_some(self.problem.clone(), |d, p| {
                d.child(tracked_box(
                    "onboarding-codes-problem",
                    Banner::new("onboarding-codes-problem", BannerTone::Danger, p),
                ))
            })
            .child(summary)
            .children(list)
            // The web's hint ("Tick the box to continue.") is `d-sr-only`, for screen readers only: not drawn here; it
            // comes with the accessibility work after the switch.
            .child(ack)
            .into_any_element()
    }
}

/// Tests (feature `test-hooks`): the step as a person reads and works it.
#[cfg(feature = "test-hooks")]
impl DoneView {
    /// The codes the list shows (as the person reads them off the screen), or none while the list is collapsed.
    pub fn shown_codes(&self) -> Option<Zeroizing<Vec<String>>> {
        self.expanded.then(|| Zeroizing::new(self.codes.to_vec()))
    }

    /// How many codes the step holds (its summary's count).
    pub fn code_count(&self) -> usize {
        self.codes.len()
    }

    /// Show/Hide.
    pub fn press_show(&mut self, cx: &mut Context<Self>) {
        self.toggle_shown(cx);
    }

    /// The tick "I saved my backup codes somewhere safe".
    pub fn press_saved(&mut self, cx: &mut Context<Self>) {
        self.toggle_saved(cx);
    }

    /// Open dilla.
    pub fn press_open(&mut self, cx: &mut Context<Self>) {
        self.open(cx);
    }
}

impl Render for DoneView {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let step = tf(
            "onboarding.step",
            &[("n", &TOTAL.to_string()), ("total", &TOTAL.to_string())],
        );
        let mut f = OnboardingFrame::new("onboarding", t("onboarding.done.title"))
            .step(step)
            .progress(TOTAL, TOTAL)
            .child(frame::body_paragraph(
                t("onboarding.done.body"),
                color::FG_2,
            ));
        if let Some((server, channel)) = &self.ready {
            f = f.child(ReadyPanel::new(tf(
                "onboarding.done.ready",
                &[("server", server), ("channel", channel)],
            )));
        }
        f = f.child(frame::body_paragraph(
            t("onboarding.done.note"),
            color::FG_3,
        ));
        if !self.codes.is_empty() {
            f = f.child(self.codes_block(cx));
        }
        f = f.primary(tracked_focus(
            "onboarding-open",
            &self.open,
            PrimaryAction::new("onboarding-open", t("onboarding.done.next"))
                .focus(&self.open)
                .ready(!self.owed())
                .on_click(cx.listener(|this, _, _, cx| this.open(cx))),
        ));
        tracked(
            "onboarding",
            div()
                .id("onboarding-done")
                .key_context(super::keys::CONTEXT)
                .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
                .on_action(|_: &FocusPrevious, window, cx| window.focus_prev(cx))
                .on_action(cx.listener(|this, _: &Submit, _, cx| this.open(cx)))
                .size_full()
                .bg(color::BG)
                .child(f),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use gpui::TestAppContext;

    fn ten() -> BackupCodes {
        BackupCodes(Zeroizing::new(
            (0..10).map(|i| format!("ABCDEFGHIJKLMNOP{i:04}")).collect(),
        ))
    }

    fn result() -> SignupResult {
        SignupResult::default()
    }

    /// Onboarding.tsx's done step with codes: collapsed until shown, Open dilla refused until the tick, and the codes
    /// wiped as the step is left.
    #[gpui::test]
    fn codes_gate_open_dilla_and_go_with_the_step(cx: &mut TestAppContext) {
        let opened = std::rc::Rc::new(std::cell::Cell::new(0));
        let seen = opened.clone();
        let window = cx.add_window(|window, cx| {
            let view = cx.new(|cx| DoneView::new(result(), Some(ten()), window, cx));
            cx.subscribe(&view, move |_, _, _: &OpenDilla, _| {
                seen.set(seen.get() + 1)
            })
            .detach();
            Host(view)
        });
        let view = window.update(cx, |h, _, _| h.0.clone()).unwrap();
        cx.run_until_parked();
        view.read_with(cx, |v, _| {
            assert_eq!(v.codes.len(), 10);
            assert_eq!(
                v.expanded.then(|| v.codes.len()),
                None,
                "collapsed until Show"
            );
        });
        view.update(cx, |v, cx| v.toggle_shown(cx));
        view.read_with(cx, |v, _| {
            assert_eq!(v.expanded.then(|| v.codes.len()), Some(10));
        });
        view.update(cx, |v, cx| v.open(cx));
        cx.run_until_parked();
        assert_eq!(opened.get(), 0, "Open dilla waits for the tick");
        view.update(cx, |v, cx| {
            v.toggle_saved(cx);
            v.open(cx);
        });
        cx.run_until_parked();
        assert_eq!(opened.get(), 1);
        view.read_with(cx, |v, _| {
            assert_eq!(v.codes.len(), 0, "the codes go with the step");
        });
    }

    /// No codes (the account already held unused ones): nothing to tick, Open dilla goes on.
    #[gpui::test]
    fn no_codes_open_at_once(cx: &mut TestAppContext) {
        let opened = std::rc::Rc::new(std::cell::Cell::new(0));
        let seen = opened.clone();
        let window = cx.add_window(|window, cx| {
            let view = cx.new(|cx| DoneView::new(result(), None, window, cx));
            cx.subscribe(&view, move |_, _, _: &OpenDilla, _| {
                seen.set(seen.get() + 1)
            })
            .detach();
            Host(view)
        });
        let view = window.update(cx, |h, _, _| h.0.clone()).unwrap();
        view.update(cx, |v, cx| v.open(cx));
        cx.run_until_parked();
        assert_eq!(opened.get(), 1);
    }

    /// Onboarding.tsx draws the tick's hint `d-sr-only` (for screen readers, never on screen): the step's layout is the
    /// same ticked or not, so Open dilla does not move when the box is ticked.
    #[gpui::test]
    fn the_ticks_hint_is_not_drawn(cx: &mut TestAppContext) {
        cx.update(crate::install);
        let (view, vcx) = cx.add_window_view(|window, cx| {
            let view = cx.new(|cx| DoneView::new(result(), Some(ten()), window, cx));
            Host(view)
        });
        let done = view.read_with(vcx, |h, _| h.0.clone());
        let open_at = |vcx: &mut gpui::VisualTestContext| {
            vcx.update(|window, _| window.refresh());
            vcx.run_until_parked();
            vcx.debug_bounds("onboarding-open")
                .expect("Open dilla is drawn")
                .origin
        };
        let unticked = open_at(vcx);
        done.update(vcx, |d, cx| d.toggle_saved(cx));
        assert_eq!(open_at(vcx), unticked, "no hint line above Open dilla");
    }

    /// The handover's holder (`AppView::open_workspace`): a launch dropped without a window leaves the codes with the
    /// handing window's handle; a window that opened took them, once.
    #[test]
    fn the_handover_keeps_or_hands_the_codes_once() {
        let kept = SignupCodes::new(Some(ten()));
        drop(kept.clone());
        assert_eq!(kept.take().map(|c| c.0.len()), Some(10), "no window opened");
        let kept = SignupCodes::new(Some(ten()));
        let launch = kept.clone();
        assert!(launch.take().is_some(), "the new window takes them");
        assert!(kept.take().is_none(), "and nothing is left behind");
    }

    /// The saved file: the title, the copy, then the codes one per line as Copy gives them.
    #[test]
    fn the_file_holds_the_copy_and_the_codes() {
        let codes = ten();
        let body = codes_file(&codes.0);
        assert!(body.starts_with(t("factor.codes.title")));
        assert!(body.contains(t("onboarding.done.codesBody")));
        assert!(body.ends_with(&format!("{}\n", copy_text(&codes.0))));
        assert_eq!(body.lines().filter(|l| l.contains('-')).count(), 10);
    }

    struct Host(gpui::Entity<DoneView>);
    impl Render for Host {
        fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
            div().size_full().child(self.0.clone())
        }
    }
}
