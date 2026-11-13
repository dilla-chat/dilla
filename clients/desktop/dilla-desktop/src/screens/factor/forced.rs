//! The screen of an account that owes a second factor (web `factor/ForcedFactor.tsx` and `ForgetBrowser.tsx`): the factor
//! set-up and nothing else, no skip and no cancel. Its one way out is Forget this device, which clears this device's copy
//! of the instance (its store and key) and never reaches the shell; the device stays in the account's device list until
//! it is removed from another device.
//!
//! When the set-up ends, the factors are read again (`Factors::setup_done`): an account that owes nothing goes on
//! (`Client::factor_completed`, Task 2), one that still owes one goes back to the choice, any other refusal is shown and
//! the same press retries.

use std::rc::Rc;

use dilla_client::Client;
use gpui::{
    AppContext as _, ClickEvent, Context, Entity, EventEmitter, IntoElement, ParentElement, Render,
    Styled, Subscription, Window, div,
};

use dilla_desktop_ui::components::confirm_dialog::ConfirmDialog;

use super::frame::link;
use super::setup::{FactorSetupView, Origin, SetupEvent};
use crate::strings::t;

/// What the forced screen tells the app.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum ForcedEvent {
    /// The factor is in place and the client goes on (its phase leaves `FactorSetup`).
    Completed,
    /// The person confirmed Forget this device: the app stops the client and forgets the instance
    /// (`dilla_client::features::factors::forget_device`), then shows the sign-in.
    Forget,
}

pub struct ForcedFactorView {
    client: Rc<Client>,
    setup: Entity<FactorSetupView>,
    confirm: bool,
    /// Forgetting is under way (the sign-out and the wipe).
    forgetting: bool,
    forget_error: Option<String>,
    _subscription: Subscription,
}

impl EventEmitter<ForcedEvent> for ForcedFactorView {}

impl ForcedFactorView {
    /// `origin` is [`Origin::Required`] (the forced screen) or [`Origin::SignIn`] (a sign-in that found the factor owed).
    pub fn new(
        client: Rc<Client>,
        server: url::Url,
        origin: Origin,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let username = client.state().borrow().account.username.clone();
        let setup =
            cx.new(|cx| FactorSetupView::new(client.clone(), server, origin, username, window, cx));
        let subscription =
            cx.subscribe_in(&setup, window, |this, setup, e: &SetupEvent, window, cx| {
                if let SetupEvent::Done { .. } = e {
                    this.finish(setup.clone(), window, cx);
                }
            });
        Self {
            client,
            setup,
            confirm: false,
            forgetting: false,
            forget_error: None,
            _subscription: subscription,
        }
    }

    /// The app's Forget did not go through (the sign-out was refused, or the store could not go): said in the dialog,
    /// and the device stays as it was.
    pub fn forget_refused(&mut self, code: &str, cx: &mut Context<Self>) {
        self.forgetting = false;
        self.forget_error = Some(crate::strings::tf("factor.error.other", &[("code", code)]));
        cx.notify();
    }

    fn finish(
        &mut self,
        setup: Entity<FactorSetupView>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let reply = self.client.factors().setup_done();
        cx.spawn_in(window, async move |this, cx| {
            let answer = reply.await;
            let _ = this.update_in(cx, |this, window, cx| match answer {
                Ok(()) => {
                    this.client.factor_completed();
                    cx.emit(ForcedEvent::Completed);
                }
                Err(e) => setup.update(cx, |s, cx| s.finish_refused(&e, window, cx)),
            });
        })
        .detach();
    }
}

impl Render for ForcedFactorView {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let this = cx.entity().downgrade();
        let close = this.clone();
        div()
            .size_full()
            .relative()
            .child(self.setup.clone())
            .child(
                div()
                    .absolute()
                    .bottom_0()
                    .left_0()
                    .right_0()
                    .flex()
                    .justify_center()
                    .child(link(
                        "factor-forget",
                        t("devices.forget"),
                        false,
                        cx.listener(|this, _: &ClickEvent, _, cx| {
                            this.confirm = true;
                            cx.notify();
                        }),
                    )),
            )
            .child(
                ConfirmDialog::new(
                    "factor-forget-confirm",
                    t("devices.forgetTitle"),
                    t("devices.forgetBody"),
                    t("devices.confirmForget"),
                    t("devices.working"),
                    t("signin.cancel"),
                )
                .open(self.confirm)
                .busy(self.forgetting)
                .error(self.forget_error.clone().map(Into::into))
                .on_confirm(move |_, cx| {
                    let _ = this.update(cx, |this, cx| {
                        if !this.forgetting {
                            this.forgetting = true;
                            this.forget_error = None;
                            cx.emit(ForcedEvent::Forget);
                            cx.notify();
                        }
                    });
                })
                .on_close(move |_, cx| {
                    let _ = close.update(cx, |this, cx| {
                        this.confirm = false;
                        cx.notify();
                    });
                }),
            )
    }
}
