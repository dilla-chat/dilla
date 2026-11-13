//! The Profile section's draft: a port of the state logic of `packages/web/src/screens/settings/Profile.tsx`, without any view.
//!
//! The form holds three fields. Save sends only the fields that changed ('' clears one), trimmed; a text the instance would
//! refuse is named next to its field as it is typed. A field changed on another device replaces the form's copy while it is
//! not being edited. Profile saves as you type (the prototype's way): a valid change is saved [`SAVE_AFTER_MS`] after the last
//! keystroke, and the settings workspace only holds the section (its unsaved-changes prompt) while a change cannot be saved
//! (invalid, a failed save, or an empty display name).

use std::cell::RefCell;
use std::rc::Rc;

use dilla_client::features::profile::{
    BIO_MAX_CHARS, DISPLAY_MAX_CHARS, ProfilePatch, ProfileTextProblem, STATUS_MAX_CHARS,
    profile_text_problem, profile_text_value,
};
use dilla_client::state::profile::MyProfile;

use crate::edit_state::{Flags, Section};

/// A valid change is saved this long after the last keystroke.
pub const SAVE_AFTER_MS: u64 = 700;

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Fields {
    pub display: String,
    pub status_msg: String,
    pub bio: String,
}

impl Fields {
    /// The stored profile as the form's baseline: a display name falls back to the username (`user.display || username`).
    pub fn saved(me: Option<&MyProfile>, username: &str) -> Self {
        match me {
            Some(me) => Self {
                display: if me.display.is_empty() {
                    username.to_owned()
                } else {
                    me.display.clone()
                },
                status_msg: me.status_msg.clone(),
                bio: me.bio.clone(),
            },
            None => Self {
                display: username.to_owned(),
                ..Self::default()
            },
        }
    }
}

/// Why a save did not land: a refusal of the text, the instance's meter, or anything else.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Failure {
    Save,
    Text,
    Wait,
}

impl Failure {
    /// The refusal code a save came back with.
    pub fn of_code(code: &str) -> Self {
        match code {
            "E_INVALID_REQUEST" | "E_BAD_INPUT" => Self::Text,
            "E_RATE_LIMITED" => Self::Wait,
            _ => Self::Save,
        }
    }

    /// The `strings.rs` key of the banner.
    pub fn string_key(self) -> &'static str {
        match self {
            Self::Text => "profile.error.chars",
            Self::Wait => "profile.error.wait",
            Self::Save => "profile.error.save",
        }
    }
}

/// What the form's fields amount to against the baseline.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Evaluation {
    /// The fields to send.
    pub changed: ProfilePatch,
    pub problem_display: Option<ProfileTextProblem>,
    pub problem_status: Option<ProfileTextProblem>,
    pub problem_bio: Option<ProfileTextProblem>,
    /// The display name was emptied: it is a change, but not one that can be sent.
    pub blank_display: bool,
    pub dirty: bool,
    pub invalid: bool,
}

#[derive(Clone, Debug, Default)]
pub struct ProfileDraft {
    /// The stored profile the form follows.
    pub saved: Fields,
    /// What the last successful save wrote: the baseline until the account says the same.
    pub sent: Option<Fields>,
    pub fields: Fields,
    pub pending: bool,
    pub failed: Option<Failure>,
    pub done: bool,
    pub display_error: bool,
    /// The fields of the last save that landed, so a save that waited for it does not send the same text twice.
    last_written: Option<Fields>,
    /// The fields of the save in flight and what the stored profile is once it lands (snapshotted when it starts, so text
    /// typed while it runs is still a change afterwards).
    inflight: Option<(Fields, Fields)>,
}

fn edited(typed: &str, baseline: &str, next: &str, problem: Option<ProfileTextProblem>) -> bool {
    // Typing spaces at the end of the stored text is not a change; any other difference, or text the rule refuses, is.
    typed != baseline && (next != baseline || problem.is_some())
}

impl ProfileDraft {
    pub fn new(saved: Fields) -> Self {
        Self {
            fields: saved.clone(),
            saved,
            ..Self::default()
        }
    }

    pub fn baseline(&self) -> &Fields {
        self.sent.as_ref().unwrap_or(&self.saved)
    }

    /// The account moved under the form (another device, or this save landing): each field follows it unless it is being
    /// edited, that is while it still equals the old stored value.
    pub fn follow_account(&mut self, now: Fields) {
        if now == self.saved {
            return;
        }
        let before = std::mem::replace(&mut self.saved, now.clone());
        self.sent = None;
        let follow = |current: &mut String, was: &str, now: &str| {
            if current == was {
                now.clone_into(current);
            }
        };
        follow(&mut self.fields.display, &before.display, &now.display);
        follow(
            &mut self.fields.status_msg,
            &before.status_msg,
            &now.status_msg,
        );
        follow(&mut self.fields.bio, &before.bio, &now.bio);
    }

    /// Any edit clears the saved mark and the failure.
    pub fn edit(&mut self, fields: Fields) {
        self.fields = fields;
        self.done = false;
        self.failed = None;
        self.display_error = false;
    }

    pub fn evaluate(&self) -> Evaluation {
        let f = &self.fields;
        let b = self.baseline();
        let next = Fields {
            display: profile_text_value(&f.display, false),
            status_msg: profile_text_value(&f.status_msg, false),
            bio: profile_text_value(&f.bio, true),
        };
        let rule_display = profile_text_problem(&f.display, DISPLAY_MAX_CHARS, false);
        let rule_status = profile_text_problem(&f.status_msg, STATUS_MAX_CHARS, false);
        let rule_bio = profile_text_problem(&f.bio, BIO_MAX_CHARS, true);
        let e_display = edited(&f.display, &b.display, &next.display, rule_display);
        let e_status = edited(&f.status_msg, &b.status_msg, &next.status_msg, rule_status);
        let e_bio = edited(&f.bio, &b.bio, &next.bio, rule_bio);
        let changed = ProfilePatch {
            display: e_display.then(|| next.display.clone()),
            status_msg: e_status.then(|| next.status_msg.clone()),
            bio: e_bio.then(|| next.bio.clone()),
        };
        let problem_display = e_display.then_some(rule_display).flatten();
        let problem_status = e_status.then_some(rule_status).flatten();
        let problem_bio = e_bio.then_some(rule_bio).flatten();
        Evaluation {
            dirty: !changed.is_empty(),
            invalid: problem_display.is_some() || problem_status.is_some() || problem_bio.is_some(),
            blank_display: e_display && next.display.is_empty(),
            changed,
            problem_display,
            problem_status,
            problem_bio,
        }
    }

    /// The stored profile once `changed` lands: the baseline with each edited field as typed.
    pub fn written(&self) -> Fields {
        let e = self.evaluate();
        let mut w = self.baseline().clone();
        if e.changed.display.is_some() {
            w.display.clone_from(&self.fields.display);
        }
        if e.changed.status_msg.is_some() {
            w.status_msg.clone_from(&self.fields.status_msg);
        }
        if e.changed.bio.is_some() {
            w.bio.clone_from(&self.fields.bio);
        }
        w
    }

    /// A change that cannot be saved holds the section.
    pub fn unsaveable(&self) -> bool {
        let e = self.evaluate();
        e.dirty && (e.invalid || self.failed.is_some() || e.blank_display)
    }

    pub fn flags(&self) -> Flags {
        let e = self.evaluate();
        Flags {
            dirty: self.unsaveable(),
            busy: self.pending,
            can_save: e.dirty && !self.pending && !e.invalid,
        }
    }

    /// Whether the autosave timer should run now (a valid change and no failure).
    pub fn autosaves(&self) -> bool {
        let e = self.evaluate();
        e.dirty && !e.invalid && self.failed.is_none()
    }

    /// Starts a save: the patch to send, or `None` when there is nothing to send (the form is clean, the last save already
    /// wrote this text) or it cannot be sent (invalid, or an emptied display name, which also shows its error).
    pub fn begin_save(&mut self) -> Option<ProfilePatch> {
        if self.pending {
            return None;
        }
        if self.last_written.as_ref() == Some(&self.fields) {
            return None;
        }
        let e = self.evaluate();
        if !e.dirty || e.invalid {
            return None;
        }
        if e.blank_display {
            self.display_error = true;
            return None;
        }
        self.inflight = Some((self.fields.clone(), self.written()));
        self.pending = true;
        self.failed = None;
        Some(e.changed)
    }

    /// The save in flight landed: what it sent is the baseline and the last text written, whatever was typed meanwhile.
    /// `true` when the form is still a valid change, so the autosave timer must be armed again (a timer that fired while this
    /// save ran found it pending and was dropped).
    pub fn save_landed(&mut self) -> bool {
        self.pending = false;
        if let Some((sent, written)) = self.inflight.take() {
            self.last_written = Some(sent);
            self.sent = Some(written);
        }
        self.done = true;
        self.autosaves()
    }

    pub fn save_failed(&mut self, code: &str) {
        self.pending = false;
        self.inflight = None;
        self.failed = Some(Failure::of_code(code));
    }

    /// Reset: the form goes back to the baseline.
    pub fn reset(&mut self) {
        self.fields = self.baseline().clone();
        self.failed = None;
        self.done = false;
        self.display_error = false;
    }

    /// The preview card as it would read after the save.
    pub fn preview(&self, username: &str) -> Fields {
        let display = profile_text_value(&self.fields.display, false);
        Fields {
            display: if display.is_empty() {
                username.to_owned()
            } else {
                display
            },
            status_msg: profile_text_value(&self.fields.status_msg, false),
            bio: profile_text_value(&self.fields.bio, true),
        }
    }
}

/// The draft as the workspace's open section: leaving the section goes through it.
///
/// `start_save` asks the view to send the form now and answers whether the form had nothing left to send (the workspace's
/// `flush`); the actual save runs on the client, so `save` here only reports that one was started.
pub struct DraftSection {
    pub draft: Rc<RefCell<ProfileDraft>>,
    pub start_save: Rc<dyn Fn() -> bool>,
}

impl Section for DraftSection {
    fn save(&mut self) -> bool {
        (self.start_save)()
    }

    fn reset(&mut self) {
        self.draft.borrow_mut().reset();
    }

    fn flush(&mut self) -> Option<bool> {
        let d = self.draft.borrow();
        // Nothing to send, or only a change that cannot be sent: the workspace decides from the flags.
        let sendable = d.autosaves();
        drop(d);
        if sendable {
            Some((self.start_save)())
        } else {
            None
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::edit_state::{Nav, Workspace};

    fn draft() -> ProfileDraft {
        ProfileDraft::new(Fields {
            display: "Ada".into(),
            status_msg: String::new(),
            bio: String::new(),
        })
    }

    #[test]
    fn a_clean_form_has_nothing_to_send() {
        let mut d = draft();
        assert!(!d.evaluate().dirty);
        assert!(d.begin_save().is_none());
        // Spaces at the end of the stored text are not a change.
        d.edit(Fields {
            display: "Ada  ".into(),
            ..d.fields.clone()
        });
        assert!(!d.evaluate().dirty);
    }

    #[test]
    fn only_the_changed_fields_are_sent_trimmed() {
        let mut d = draft();
        d.edit(Fields {
            status_msg: "  at the desk ".into(),
            ..d.fields.clone()
        });
        let patch = d.begin_save().expect("a change");
        assert_eq!(patch.display, None);
        assert_eq!(patch.status_msg.as_deref(), Some("at the desk"));
        assert_eq!(patch.bio, None);
        assert!(d.pending);
        // One save at a time.
        assert!(d.begin_save().is_none());
        d.save_landed();
        assert!(d.done && !d.pending);
        assert!(!d.evaluate().dirty, "the sent text is the baseline now");
        assert!(d.begin_save().is_none(), "the same text is not sent twice");
    }

    /// A "b" typed while "a" is in flight is a change after "a" lands, and the timer is armed again.
    #[test]
    fn text_typed_while_a_save_runs_is_saved_after_it() {
        let mut d = draft();
        let with = |d: &ProfileDraft, bio: &str| Fields {
            bio: bio.into(),
            ..d.fields.clone()
        };
        let a = with(&d, "a");
        d.edit(a);
        assert!(d.begin_save().is_some());
        let ab = with(&d, "ab");
        d.edit(ab);
        assert!(d.save_landed(), "still dirty: the timer is armed again");
        assert_eq!(d.baseline().bio, "a", "what was sent is the baseline");
        let patch = d.begin_save().expect("the b is sent");
        assert_eq!(patch.bio.as_deref(), Some("ab"));
    }

    /// The autosave timer fires while a save is pending: nothing starts, and landing says another save is due.
    #[test]
    fn a_timer_that_fires_during_a_save_is_not_lost() {
        let mut d = draft();
        let a = Fields {
            bio: "a".into(),
            ..d.fields.clone()
        };
        d.edit(a);
        assert!(d.begin_save().is_some());
        let ab = Fields {
            bio: "ab".into(),
            ..d.fields.clone()
        };
        d.edit(ab);
        assert!(d.begin_save().is_none(), "the timer found a save pending");
        assert!(d.save_landed());
        // A landing with nothing left to send arms nothing.
        let mut clean = draft();
        let x = Fields {
            bio: "x".into(),
            ..clean.fields.clone()
        };
        clean.edit(x);
        assert!(clean.begin_save().is_some());
        assert!(!clean.save_landed());
    }

    #[test]
    fn a_refused_text_names_its_field_and_holds_the_section() {
        let mut d = draft();
        d.edit(Fields {
            display: "Unsaved\tdraft".into(),
            ..d.fields.clone()
        });
        let e = d.evaluate();
        assert_eq!(e.problem_display, Some(ProfileTextProblem::Chars));
        assert!(e.invalid && d.unsaveable());
        assert!(d.flags().dirty && !d.flags().can_save);
        assert!(d.begin_save().is_none());
        assert!(!d.autosaves());
    }

    #[test]
    fn an_emptied_display_name_is_a_change_that_cannot_be_sent() {
        let mut d = draft();
        d.edit(Fields {
            display: String::new(),
            ..d.fields.clone()
        });
        assert!(d.evaluate().blank_display && d.unsaveable());
        assert!(d.begin_save().is_none());
        assert!(d.display_error);
    }

    #[test]
    fn a_field_follows_the_account_unless_it_is_being_edited() {
        let mut d = draft();
        d.edit(Fields {
            bio: "typing".into(),
            ..d.fields.clone()
        });
        d.follow_account(Fields {
            display: "Ada L".into(),
            status_msg: "new".into(),
            bio: "elsewhere".into(),
        });
        assert_eq!(d.fields.display, "Ada L");
        assert_eq!(d.fields.status_msg, "new");
        assert_eq!(d.fields.bio, "typing", "the edited field stays");
    }

    #[test]
    fn a_refusal_maps_to_its_banner() {
        assert_eq!(Failure::of_code("E_BAD_INPUT"), Failure::Text);
        assert_eq!(Failure::of_code("E_RATE_LIMITED"), Failure::Wait);
        assert_eq!(Failure::of_code("E_NETWORK"), Failure::Save);
        let mut d = draft();
        d.edit(Fields {
            bio: "x".into(),
            ..d.fields.clone()
        });
        let _ = d.begin_save();
        d.save_failed("E_RATE_LIMITED");
        assert_eq!(d.failed, Some(Failure::Wait));
        assert!(d.unsaveable(), "a failed save holds the section");
        assert!(!d.autosaves());
    }

    /// e2e/web/search-settings.spec.ts "a dirty Profile draft survives Keep editing and is discarded only after confirmation".
    #[test]
    fn dirty_profile_draft_keep_editing_then_discard() {
        let draft = Rc::new(RefCell::new(draft()));
        let mut ws: Workspace<&str> = Workspace::new();
        ws.attach(Box::new(DraftSection {
            draft: draft.clone(),
            start_save: Rc::new(|| false),
        }));
        // Profile saves as you type, so only a change it cannot save (a character dilla refuses) holds it.
        let typed = Fields {
            display: "Unsaved\tdraft".into(),
            ..draft.borrow().fields.clone()
        };
        draft.borrow_mut().edit(typed.clone());
        let flags = draft.borrow().flags();
        ws.publish(flags);
        assert_eq!(ws.footer().map(|f| f.status), Some("Unsaved changes"));

        assert_eq!(ws.request("signin"), Nav::Prompt);
        let prompt = ws.prompt().expect("the prompt is open");
        assert_eq!(prompt.title, "Discard unsaved changes?");
        // Keep editing: the draft is kept as typed.
        ws.keep_editing();
        assert!(!ws.prompt_open());
        assert_eq!(draft.borrow().fields, typed);

        // Again, and this time Discard: only after the confirmation is the draft thrown away, and the destination comes back.
        assert_eq!(ws.request("signin"), Nav::Prompt);
        assert_eq!(
            draft.borrow().fields,
            typed,
            "the prompt alone discards nothing"
        );
        assert_eq!(ws.discard(), vec!["signin"]);
        assert_eq!(draft.borrow().fields.display, "Ada");
    }

    #[test]
    fn a_valid_change_flushes_before_the_section_is_left() {
        let draft = Rc::new(RefCell::new(draft()));
        let started = Rc::new(RefCell::new(0));
        let mut ws: Workspace<&str> = Workspace::new();
        let counter = started.clone();
        ws.attach(Box::new(DraftSection {
            draft: draft.clone(),
            start_save: Rc::new(move || {
                *counter.borrow_mut() += 1;
                true
            }),
        }));
        let typed = Fields {
            bio: "hello".into(),
            ..draft.borrow().fields.clone()
        };
        draft.borrow_mut().edit(typed);
        let flags = draft.borrow().flags();
        assert!(!flags.dirty, "a valid change does not hold the section");
        ws.publish(flags);
        assert_eq!(ws.request("next"), Nav::Go("next"));
        assert_eq!(
            *started.borrow(),
            1,
            "the last change was sent on the way out"
        );
    }
}
