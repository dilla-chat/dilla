//! The sign-up's last step (Onboarding.tsx `shown === 'done'`, step 6 of 6): "You're in", the panel naming the server the
//! invite joined and its first text channel (only when there is one), the small print, and "Open dilla". A join refused
//! after the factor is the result, never a failed sign-up (ruling 34): the shell offers it again in the join dialog.
//!
//! The backup codes the factor's set-up returned (Task 26) are shown by the factor views; this step shows none yet.

use dilla_client::features::signup::SignupResult;
use dilla_client::state::{AppState, Channel, CommunityId};
use gpui::{
    Context, EventEmitter, FocusHandle, InteractiveElement, IntoElement, ParentElement, Render,
    Styled, Window, div,
};

use dilla_desktop_ui::components::onboarding::{OnboardingFrame, PrimaryAction, ReadyPanel, frame};
use dilla_desktop_ui::tokens::color;

use super::keys::Submit;
use super::signup::TOTAL;
use crate::strings::{t, tf};
use crate::{tracked, tracked_focus};

/// "Open dilla": the shell opens on the joined server.
pub struct OpenDilla {
    pub result: SignupResult,
}

pub struct DoneView {
    result: SignupResult,
    /// The joined server's name and its first text channel's, once listed.
    ready: Option<(String, String)>,
    open: FocusHandle,
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

impl DoneView {
    pub fn new(result: SignupResult, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let open = cx.focus_handle().tab_stop(true);
        window.focus(&open, cx);
        Self {
            result,
            ready: None,
            open,
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

    fn open(&mut self, cx: &mut Context<Self>) {
        cx.emit(OpenDilla {
            result: self.result.clone(),
        });
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
        f = f
            .child(frame::body_paragraph(
                t("onboarding.done.note"),
                color::FG_3,
            ))
            .primary(tracked_focus(
                "onboarding-open",
                &self.open,
                PrimaryAction::new("onboarding-open", t("onboarding.done.next"))
                    .focus(&self.open)
                    .on_click(cx.listener(|this, _, _, cx| this.open(cx))),
            ));
        tracked(
            "onboarding",
            div()
                .id("onboarding-done")
                .key_context(super::keys::CONTEXT)
                .on_action(cx.listener(|this, _: &Submit, _, cx| this.open(cx)))
                .size_full()
                .bg(color::BG)
                .child(f),
        )
    }
}
