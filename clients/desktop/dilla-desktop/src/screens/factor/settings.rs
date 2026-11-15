//! Settings → Sign-in (web `settings/SignInFactors.tsx` and `PasswordDialog.tsx`): the password, the authenticator app,
//! the passkeys and the backup codes, each a factor row with its action. Every change that touches a factor asks
//! "Confirm it is you" first ([`StepUpDialog`]) and spends the token on that one change; a refused token (spent,
//! expired) asks again, saying so. The password change is its own step-up (the old password). New backup codes are shown
//! once, in a dialog, from a zeroizing buffer.

use std::rc::Rc;

use dilla_client::Client;
use dilla_client::features::factors::{
    BackupCodes, FactorError, FactorKind, InstanceDoc, StepUpToken,
};
use dilla_client::state::factors::Factors as FactorsView;
use gpui::prelude::FluentBuilder;
use gpui::{
    AnyElement, AppContext as _, ClickEvent, ClipboardItem, Context, Entity, InteractiveElement,
    IntoElement, ParentElement, Render, Styled, Subscription, Window, div, px,
};
use gpui_component::input::InputState;
use zeroize::Zeroizing;

use dilla_desktop_ui::components::banner::{Banner, BannerTone};
use dilla_desktop_ui::components::button::{Button, ButtonVariant};
use dilla_desktop_ui::components::confirm_dialog::{ConfirmDialog, Tone};
use dilla_desktop_ui::components::dialog::Dialog;
use dilla_desktop_ui::components::factor::{
    CodeList, FactorRow, Place, code_list_columns, copy_text,
};
use dilla_desktop_ui::components::settings::frame::{group_heading, section_header};
use dilla_desktop_ui::components::text_field::TextField;

use super::setup::{Choice, FactorSetupView, Lost, Origin, SetupEvent, shared_refusal};
use super::step_up::{Notice, Proof, StepUpDialog, StepUpEvent};
use crate::strings::{t, tf};
use crate::tracked_box;

/// The section's rhythm: 12px between its parts, the passkeys' list 8px under its heading.
const GAP: f32 = 12.0;
/// A settings dialog's content width, for the code list's columns (`Dialog` form width less its padding).
const DIALOG_CONTENT: f32 = 432.0;
/// The shortest password the instance takes (`settings.password.error.short`).
const PASSWORD_MIN: usize = 12;

/// What the person asked for, waiting for its step-up.
#[derive(Clone, Debug)]
enum Purpose {
    Totp,
    Passkey,
    Remove {
        kind: FactorKind,
        name: String,
    },
    Codes,
    /// A first password on an account that holds a factor.
    FirstPassword,
}

#[derive(Clone)]
enum Modal {
    None,
    StepUp(Entity<StepUpDialog>),
    Confirm(Purpose),
    Add(Entity<FactorSetupView>),
    Codes,
    Password,
}

pub struct SignInSection {
    client: Rc<Client>,
    server: url::Url,
    factors: Option<FactorsView>,
    instance: InstanceDoc,
    load_error: Option<String>,
    purpose: Option<Purpose>,
    token: Option<StepUpToken>,
    modal: Modal,
    busy: bool,
    error: Option<String>,
    status: Option<String>,
    codes: Zeroizing<Vec<String>>,
    old: Entity<InputState>,
    new: Entity<InputState>,
    again: Entity<InputState>,
    password_error: Option<String>,
    /// The open modal's subscription (the step-up dialog or the add dialog): one at a time, replaced.
    modal_sub: Option<Subscription>,
}

impl SignInSection {
    pub fn new(
        client: Rc<Client>,
        server: url::Url,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let masked = |window: &mut Window, cx: &mut Context<Self>| {
            cx.new(|cx| InputState::new(window, cx).masked(true))
        };
        let factors = client.state().borrow().factors.factors.clone();
        let mut this = Self {
            old: masked(window, cx),
            new: masked(window, cx),
            again: masked(window, cx),
            client,
            server,
            factors,
            instance: InstanceDoc::default(),
            load_error: None,
            purpose: None,
            token: None,
            modal: Modal::None,
            busy: false,
            error: None,
            status: None,
            codes: Zeroizing::new(Vec::new()),
            password_error: None,
            modal_sub: None,
        };
        this.refresh(window, cx);
        this
    }

    /// The section reads the factors again each time it opens.
    fn refresh(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.load_error = None;
        let read = self.client.factors().refresh();
        let doc = self.client.factors().instance();
        cx.spawn_in(window, async move |this, cx| {
            let (read, doc) = (read.await, doc.await);
            let _ = this.update(cx, |this, cx| {
                match read {
                    Ok(Some(f)) => this.factors = Some(f),
                    Ok(None) => {}
                    Err(e) => {
                        this.load_error =
                            Some(tf("factorSettings.error.load", &[("code", &e.code)]));
                    }
                }
                if let Ok(doc) = doc {
                    this.instance = doc;
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn proof(&self) -> Proof {
        match &self.factors {
            Some(f) if !f.has_password && !f.passkeys.is_empty() => Proof::Passkey,
            Some(f) if !f.has_password => Proof::Code,
            _ => Proof::Password,
        }
    }

    fn ask(
        &mut self,
        purpose: Purpose,
        notice: Option<Notice>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.purpose = Some(purpose);
        self.token = None;
        self.error = None;
        let dialog = cx.new(|cx| {
            StepUpDialog::new(
                self.client.factors(),
                self.server.clone(),
                self.proof(),
                notice,
                window,
                cx,
            )
        });
        self.modal_sub = Some(cx.subscribe_in(
            &dialog,
            window,
            |this, _, e: &StepUpEvent, window, cx| match e {
                StepUpEvent::Token(token) => this.got_token(token.clone(), window, cx),
                StepUpEvent::Close => this.end(cx),
            },
        ));
        self.modal = Modal::StepUp(dialog);
        cx.notify();
    }

    fn got_token(&mut self, token: StepUpToken, window: &mut Window, cx: &mut Context<Self>) {
        let Some(purpose) = self.purpose.clone() else {
            return;
        };
        // Only a change this section makes itself keeps the token; an add hands it to its dialog.
        if matches!(
            purpose,
            Purpose::Remove { .. } | Purpose::Codes | Purpose::FirstPassword
        ) {
            self.token = Some(token.clone());
        }
        match purpose {
            Purpose::Totp | Purpose::Passkey => {
                let start = if matches!(purpose, Purpose::Totp) {
                    Choice::App
                } else {
                    Choice::Passkey
                };
                let username = self.client.state().borrow().account.username.clone();
                let client = self.client.clone();
                let server = self.server.clone();
                let view = cx.new(|cx| {
                    FactorSetupView::new(
                        client,
                        server,
                        Origin::Settings {
                            stepup: token,
                            start,
                        },
                        username,
                        window,
                        cx,
                    )
                });
                self.modal_sub = Some(cx.subscribe_in(
                    &view,
                    window,
                    |this, _, e: &SetupEvent, window, cx| match e {
                        SetupEvent::Done { added, name, .. } => {
                            this.status = Some(match (added, name) {
                                (super::setup::Added::Passkey, Some(name)) => {
                                    tf("factorSettings.added.passkey", &[("name", name)])
                                }
                                _ => t("factorSettings.added.totp").to_owned(),
                            });
                            this.end(cx);
                            this.refresh(window, cx);
                        }
                        SetupEvent::StepUpLost(why) => {
                            let purpose = this.purpose.clone().unwrap_or(Purpose::Totp);
                            let notice = match why {
                                Lost::Expired => Notice::Expired,
                                Lost::Again => Notice::Again,
                            };
                            this.ask(purpose, Some(notice), window, cx);
                        }
                        SetupEvent::Close => this.end(cx),
                    },
                ));
                self.modal = Modal::Add(view);
            }
            Purpose::Remove { .. } => self.modal = Modal::Confirm(purpose),
            Purpose::Codes => self.regenerate(window, cx),
            Purpose::FirstPassword => self.modal = Modal::Password,
        }
        cx.notify();
    }

    fn end(&mut self, cx: &mut Context<Self>) {
        self.modal = Modal::None;
        self.purpose = None;
        self.token = None;
        self.busy = false;
        self.error = None;
        self.codes = Zeroizing::new(Vec::new());
        cx.notify();
    }

    /// SignInFactors `refusedChange`: by status and code, never by detail.
    fn refused(&mut self, e: &FactorError, window: &mut Window, cx: &mut Context<Self>) {
        self.busy = false;
        if e.is_step_up() {
            let purpose = self.purpose.clone().unwrap_or(Purpose::Codes);
            self.ask(purpose, Some(Notice::Expired), window, cx);
            return;
        }
        self.error = Some(if e.status == 409 {
            t("factorSettings.error.only").to_owned()
        } else if e.status == 404 || e.is(403, "E_FORBIDDEN") {
            t("factorSettings.error.changed").to_owned()
        } else {
            shared_refusal(e)
                .unwrap_or_else(|| tf("factorSettings.error.other", &[("code", &e.code)]))
        });
        if e.status == 404 || e.status == 409 || e.is(403, "E_FORBIDDEN") {
            self.token = None;
            self.refresh(window, cx);
        }
        cx.notify();
    }

    fn remove(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(Purpose::Remove { kind, name }) = self.purpose.clone() else {
            return;
        };
        let Some(token) = self.token.take() else {
            self.ask(Purpose::Remove { kind, name }, None, window, cx);
            return;
        };
        self.busy = true;
        let reply = self.client.factors().remove(token, kind.clone());
        cx.spawn_in(window, async move |this, cx| {
            let answer = reply.await;
            let _ = this.update_in(cx, |this, window, cx| match answer {
                Ok(()) => {
                    this.status = Some(match kind {
                        FactorKind::Totp => t("factorSettings.removed.totp").to_owned(),
                        FactorKind::Passkey(_) => {
                            tf("factorSettings.removed.passkey", &[("name", &name)])
                        }
                    });
                    this.end(cx);
                    this.refresh(window, cx);
                }
                Err(e) => this.refused(&e, window, cx),
            });
        })
        .detach();
    }

    fn regenerate(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(token) = self.token.take() else {
            self.ask(Purpose::Codes, None, window, cx);
            return;
        };
        self.busy = true;
        self.modal = Modal::None;
        let reply = self.client.factors().regenerate_backup_codes(token);
        cx.spawn_in(window, async move |this, cx| {
            let answer: Result<BackupCodes, FactorError> = reply.await;
            let _ = this.update_in(cx, |this, window, cx| match answer {
                Ok(codes) => {
                    this.busy = false;
                    this.codes = codes.0;
                    this.modal = Modal::Codes;
                    this.status = Some(t("factorSettings.codesMade").to_owned());
                    this.refresh(window, cx);
                    cx.notify();
                }
                Err(e) => this.refused(&e, window, cx),
            });
        })
        .detach();
    }

    fn ask_password(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.password_error = None;
        for field in [&self.old, &self.new, &self.again] {
            field.update(cx, |s, cx| s.set_value("", window, cx));
        }
        match &self.factors {
            Some(f) if !f.has_password && f.holds_factor() => {
                self.ask(Purpose::FirstPassword, None, window, cx)
            }
            _ => {
                self.modal = Modal::Password;
                cx.notify();
            }
        }
    }

    fn save_password(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.busy {
            return;
        }
        let has = self.factors.as_ref().is_none_or(|f| f.has_password);
        let old = Zeroizing::new(self.old.read(cx).value().to_string());
        let new = Zeroizing::new(self.new.read(cx).value().to_string());
        let again = Zeroizing::new(self.again.read(cx).value().to_string());
        let problem = if has && old.is_empty() {
            Some("settings.password.error.oldRequired")
        } else if new.chars().count() < PASSWORD_MIN {
            Some("settings.password.error.short")
        } else if *new != *again {
            Some("settings.password.error.mismatch")
        } else {
            None
        };
        if let Some(p) = problem {
            self.password_error = Some(t(p).to_owned());
            cx.notify();
            return;
        }
        for field in [&self.old, &self.new, &self.again] {
            field.update(cx, |s, cx| s.set_value("", window, cx));
        }
        self.busy = true;
        let reply =
            self.client
                .factors()
                .change_password(has.then_some(old), new, self.token.take());
        cx.spawn_in(window, async move |this, cx| {
            let answer = reply.await;
            let _ = this.update_in(cx, |this, window, cx| {
                this.busy = false;
                match answer {
                    Ok(()) => {
                        this.status = Some(
                            t(if has {
                                "settings.password.changed"
                            } else {
                                "settings.password.added"
                            })
                            .to_owned(),
                        );
                        this.end(cx);
                    }
                    Err(e) if e.is(403, "E_FORBIDDEN") => {
                        this.password_error = Some(t("settings.password.error.wrong").to_owned());
                    }
                    Err(e) if e.is_step_up() => {
                        this.ask(Purpose::FirstPassword, Some(Notice::Expired), window, cx)
                    }
                    Err(e) => {
                        this.password_error = Some(shared_refusal(&e).unwrap_or_else(|| {
                            tf("settings.password.error.other", &[("code", &e.code)])
                        }));
                    }
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn rows(&mut self, cx: &mut Context<Self>) -> Vec<AnyElement> {
        let Some(f) = self.factors.clone() else {
            return vec![div().child(t("factorSettings.loading")).into_any_element()];
        };
        let only = (f.count() <= 1).then(|| t("factorSettings.only"));
        let mut out = vec![
            div()
                .text_size(dilla_desktop_ui::tokens::text::SM)
                .text_color(dilla_desktop_ui::tokens::color::FG_2)
                .child(t("factorSettings.body"))
                .into_any_element(),
        ];
        // The password.
        let detail = if !f.has_password {
            t("factorSettings.password.none").to_owned()
        } else {
            f.password_changed
                .map(|when| {
                    let now = std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .map_or(0, |d| d.as_secs());
                    tf(
                        "factorSettings.password.changed",
                        &[("when", &crate::format::relative(when, now))],
                    )
                })
                .unwrap_or_default()
        };
        out.push(
            FactorRow::new("signin-password", t("factorSettings.password"), detail)
                .action(tracked_box(
                    "signin-password-action",
                    Button::new(
                        "signin-password-action",
                        t(if f.has_password {
                            "settings.password.change"
                        } else {
                            "settings.password.add"
                        }),
                    )
                    .on_click(
                        cx.listener(|this, _: &ClickEvent, window, cx| {
                            this.ask_password(window, cx)
                        }),
                    ),
                ))
                .into_any_element(),
        );
        // The authenticator app.
        let totp = match f.totp {
            None => FactorRow::new(
                "signin-totp",
                t("factorSettings.totp"),
                t("factorSettings.totp.off"),
            )
            .action(tracked_box(
                "signin-totp-add",
                Button::new("signin-totp-add", t("factorSettings.totp.add")).on_click(cx.listener(
                    |this, _: &ClickEvent, window, cx| this.ask(Purpose::Totp, None, window, cx),
                )),
            )),
            Some(at) => {
                let row = FactorRow::new(
                    "signin-totp",
                    t("factorSettings.totp"),
                    tf(
                        "factorSettings.totp.on",
                        &[("date", &crate::format::day(at))],
                    ),
                );
                let button = Button::new("signin-totp-remove", t("factorSettings.totp.remove"))
                    .variant(ButtonVariant::Danger);
                match only {
                    Some(reason) => row
                        .reason(reason)
                        .action(tracked_box("signin-totp-remove", button.disabled(true))),
                    None => row.action(tracked_box(
                        "signin-totp-remove",
                        button.on_click(cx.listener(|this, _: &ClickEvent, window, cx| {
                            this.ask(
                                Purpose::Remove {
                                    kind: FactorKind::Totp,
                                    name: String::new(),
                                },
                                None,
                                window,
                                cx,
                            )
                        })),
                    )),
                }
            }
        };
        out.push(totp.into_any_element());
        // The passkeys.
        out.push(group_heading(t("factorSettings.passkeys")).into_any_element());
        if f.passkeys.is_empty() {
            out.push(
                div()
                    .text_size(dilla_desktop_ui::tokens::text::SM)
                    .child(t("factorSettings.passkeys.none"))
                    .into_any_element(),
            );
        }
        let n = f.passkeys.len();
        for (i, p) in f.passkeys.iter().enumerate() {
            let name = p.name.clone();
            let id = p.id.clone();
            let mut row = FactorRow::new(
                gpui::ElementId::Name(format!("signin-passkey-{i}").into()),
                p.name.clone(),
                crate::format::day(p.created),
            )
            .place(Place::of(i, n));
            let button = Button::new(
                gpui::ElementId::Name(format!("signin-passkey-remove-{i}").into()),
                t("factorSettings.passkeys.remove"),
            )
            .variant(ButtonVariant::Danger);
            row = match only {
                Some(reason) => row.reason(reason).action(button.disabled(true)),
                None => row.action(button.on_click(cx.listener(
                    move |this, _: &ClickEvent, window, cx| {
                        this.ask(
                            Purpose::Remove {
                                kind: FactorKind::Passkey(id.clone()),
                                name: name.clone(),
                            },
                            None,
                            window,
                            cx,
                        )
                    },
                ))),
            };
            out.push(row.into_any_element());
        }
        if self.instance.passkeys {
            out.push(
                tracked_box(
                    "signin-passkey-add",
                    Button::new("signin-passkey-add", t("factorSettings.passkeys.add")).on_click(
                        cx.listener(|this, _: &ClickEvent, window, cx| {
                            this.ask(Purpose::Passkey, None, window, cx)
                        }),
                    ),
                )
                .into_any_element(),
            );
        }
        // The backup codes.
        let left = match f.backup_codes_left {
            0 => t("factorSettings.codes.none").to_owned(),
            1 => t("factorSettings.codes.one").to_owned(),
            n => tf("factorSettings.codes.other", &[("count", &n.to_string())]),
        };
        out.push(
            FactorRow::new("signin-codes", t("factorSettings.codes"), left)
                .action(tracked_box(
                    "signin-codes-make",
                    Button::new("signin-codes-make", t("factorSettings.codes.make")).on_click(
                        cx.listener(|this, _: &ClickEvent, _, cx| {
                            this.purpose = Some(Purpose::Codes);
                            this.modal = Modal::Confirm(Purpose::Codes);
                            cx.notify();
                        }),
                    ),
                ))
                .into_any_element(),
        );
        out
    }

    fn modal(&mut self, cx: &mut Context<Self>) -> Option<AnyElement> {
        let this = cx.entity().downgrade();
        match self.modal.clone() {
            Modal::None => None,
            Modal::StepUp(dialog) => Some(dialog.into_any_element()),
            Modal::Add(view) => Some(view.into_any_element()),
            Modal::Confirm(purpose) => {
                let (title, body, confirm, busy) = match &purpose {
                    Purpose::Remove {
                        kind: FactorKind::Totp,
                        ..
                    } => (
                        t("factorSettings.removeTitle.totp").to_owned(),
                        t("factorSettings.removeBody"),
                        t("factorSettings.removeConfirm"),
                        t("factorSettings.removeBusy"),
                    ),
                    Purpose::Remove { name, .. } => (
                        tf("factorSettings.removeTitle.passkey", &[("name", name)]),
                        t("factorSettings.removeBody"),
                        t("factorSettings.removeConfirm"),
                        t("factorSettings.removeBusy"),
                    ),
                    _ => (
                        t("factorSettings.codes.makeTitle").to_owned(),
                        t("factorSettings.codes.makeBody"),
                        t("factorSettings.codes.makeConfirm"),
                        t("factorSettings.codes.makeBusy"),
                    ),
                };
                let is_codes = matches!(purpose, Purpose::Codes);
                let (confirm_to, close) = (this.clone(), this);
                Some(
                    ConfirmDialog::new(
                        "signin-confirm",
                        title,
                        body,
                        confirm,
                        busy,
                        t("factorSettings.cancel"),
                    )
                    .tone(if is_codes { Tone::Accent } else { Tone::Danger })
                    .busy(self.busy)
                    .error(self.error.clone().map(Into::into))
                    .on_confirm(move |window, cx| {
                        let _ = confirm_to.update(cx, |this, cx| {
                            if is_codes {
                                this.regenerate(window, cx)
                            } else {
                                this.remove(window, cx)
                            }
                        });
                    })
                    .on_close(move |_, cx| {
                        let _ = close.update(cx, |this, cx| this.end(cx));
                    })
                    .into_any_element(),
                )
            }
            Modal::Codes => {
                let all = copy_text(&self.codes);
                let close = this;
                Some(
                    Dialog::new(
                        "signin-codes-dialog",
                        t("factorSettings.codes.newTitle"),
                        t("dialog.close"),
                    )
                    .body(
                        div()
                            .flex()
                            .flex_col()
                            .gap(px(GAP))
                            .child(t("factorSettings.codes.newBody"))
                            .child(
                                CodeList::new(
                                    "signin-codes-list",
                                    t("factor.codes.label"),
                                    &self.codes,
                                )
                                .columns(code_list_columns(DIALOG_CONTENT))
                                .action(
                                    Button::new("signin-codes-copy", t("factor.codes.copy"))
                                        .on_click(move |_, _, cx| {
                                            cx.write_to_clipboard(ClipboardItem::new_string(
                                                all.clone(),
                                            ))
                                        }),
                                ),
                            ),
                    )
                    .on_close(move |_, cx| {
                        let _ = close.update(cx, |this, cx| this.end(cx));
                    })
                    .into_any_element(),
                )
            }
            Modal::Password => {
                let has = self.factors.as_ref().is_none_or(|f| f.has_password);
                let (save, close) = (this.clone(), this);
                let mut fields = div().flex().flex_col().gap(px(GAP));
                if has {
                    fields = fields.child(TextField::new(
                        "password-old",
                        t("settings.password.old"),
                        &self.old,
                    ));
                }
                fields = fields
                    .child(
                        TextField::new("password-new", t("settings.password.new"), &self.new)
                            .hint(t("settings.password.newHint")),
                    )
                    .child(TextField::new(
                        "password-again",
                        t("settings.password.confirm"),
                        &self.again,
                    ));
                Some(
                    ConfirmDialog::new(
                        "password-dialog",
                        t(if has {
                            "settings.password.titleChange"
                        } else {
                            "settings.password.titleSet"
                        }),
                        t(if has {
                            "settings.password.bodyChange"
                        } else {
                            "settings.password.bodySet"
                        }),
                        t(if has {
                            "settings.password.confirmChange"
                        } else {
                            "settings.password.confirmSet"
                        }),
                        t("settings.password.busy"),
                        t("settings.password.cancel"),
                    )
                    .tone(Tone::Accent)
                    .busy(self.busy)
                    .error(self.password_error.clone().map(Into::into))
                    .fields(fields, None)
                    .on_confirm(move |window, cx| {
                        let _ = save.update(cx, |this, cx| this.save_password(window, cx));
                    })
                    .on_close(move |_, cx| {
                        let _ = close.update(cx, |this, cx| this.end(cx));
                    })
                    .into_any_element(),
                )
            }
        }
    }
}

impl Render for SignInSection {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let rows = self.rows(cx);
        let modal = self.modal(cx);
        let retry = cx.listener(|this, _: &ClickEvent, window, cx| this.refresh(window, cx));
        div()
            .id("signin-section")
            .flex()
            .flex_col()
            .gap(px(GAP))
            .child(section_header(t("factorSettings.title"), None))
            .when_some(self.load_error.clone(), |d, e| {
                d.child(
                    Banner::new("signin-load", BannerTone::Danger, e)
                        .action(t("factorSettings.retry"), retry),
                )
            })
            .children(rows)
            .when_some(self.status.clone(), |d, s| {
                d.child(div().text_size(dilla_desktop_ui::tokens::text::SM).child(s))
            })
            .children(modal)
    }
}
