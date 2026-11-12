//! The settings' dirty-draft guard: a port of `packages/web/src/settings/edit-state.tsx`.
//!
//! One [`Workspace`] belongs to one settings frame. The open section registers its [`Section`] (save, reset, flush) and
//! publishes its [`Flags`]. Leaving the section (another nav row, closing the frame) goes through [`Workspace::request`]:
//! with nothing to lose the destination is returned at once; with an unsaved draft, a save in flight or an accepted
//! operation still pending, the unsaved-changes prompt opens and the destination waits. **Keep editing** drops the
//! destination and keeps the draft; **Discard changes** resets the section and returns the destination.
//!
//! The footer (`Unsaved changes` with Reset and Save) shows only while the section is dirty or busy; a failed save swaps its
//! status for `Changes could not be saved. Try again.` The copy is `settings.editing.*` (`strings`).

use crate::strings::t;

/// What the workspace needs to know of the open section, as plain values (the web's `SettingsEditFlags`).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Flags {
    pub dirty: bool,
    pub busy: bool,
    pub can_save: bool,
}

/// The open section's operations (the web's `SettingsEditState`).
pub trait Section {
    /// Saves the draft; `true` when it landed.
    fn save(&mut self) -> bool;
    /// Throws the draft away.
    fn reset(&mut self);
    /// A section that saves as you type (Profile) sends its last change before the workspace lets it go. `None` for a
    /// section with no such step; `Some(true)` when the change landed (leaving is then free).
    fn flush(&mut self) -> Option<bool> {
        None
    }
}

/// What a navigation request came to.
#[derive(Debug, PartialEq, Eq)]
pub enum Nav<T> {
    /// Nothing to lose: go to the destination now.
    Go(T),
    /// The prompt is open; the destination waits for the answer.
    Prompt,
}

/// The footer's content while it shows.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Footer {
    pub status: &'static str,
    /// Reset and Save are disabled while a save is in flight; Save also while the draft cannot be saved.
    pub reset_enabled: bool,
    pub save_enabled: bool,
}

/// The unsaved-changes prompt: a `ConfirmDialog` with the accent tone.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Prompt {
    pub title: &'static str,
    pub body: &'static str,
    pub confirm_label: &'static str,
    pub cancel_label: &'static str,
}

/// The dirty-draft guard of one settings frame. `T` is what a navigation request is for (a route).
pub struct Workspace<T> {
    section: Option<Box<dyn Section>>,
    flags: Option<Flags>,
    pending_ops: usize,
    prompt: bool,
    save_error: bool,
    /// Set by [`Workspace::clear`]: the open section keeps rendering its old draft until it is torn down, and what it
    /// publishes meanwhile is not registered (the web's `clearedOwnerRef`).
    cleared: bool,
    waiting: Vec<T>,
}

impl<T> Default for Workspace<T> {
    fn default() -> Self {
        Self {
            section: None,
            flags: None,
            pending_ops: 0,
            prompt: false,
            save_error: false,
            cleared: false,
            waiting: Vec::new(),
        }
    }
}

impl<T> Workspace<T> {
    pub fn new() -> Self {
        Self::default()
    }

    /// The open section registers its operations; the flags follow with [`Workspace::publish`].
    pub fn attach(&mut self, section: Box<dyn Section>) {
        self.section = Some(section);
    }

    pub fn publish(&mut self, flags: Flags) {
        if self.section.is_none() {
            return;
        }
        if self.cleared {
            // Still the cleared section's draft or save in flight: not registered again. A clean section is a new start.
            if flags.dirty || flags.busy {
                return;
            }
            self.cleared = false;
        }
        self.flags = Some(flags);
    }

    /// The sign-out reset (the web's `clear()`): the open section's draft is thrown away, the footer goes, the prompt closes
    /// and the navigations waiting on it are dropped (they resolve `false`: nobody goes anywhere).
    pub fn clear(&mut self) {
        if let Some(section) = self.section.as_mut() {
            section.reset();
        }
        self.cleared = self.section.is_some();
        self.flags = None;
        self.save_error = false;
        self.prompt = false;
        self.waiting.clear();
    }

    /// The section closed: its draft, flags and save error go with it.
    pub fn detach(&mut self) {
        self.section = None;
        self.flags = None;
        self.save_error = false;
        self.cleared = false;
    }

    /// An accepted operation that continues whatever section is open counts as pending (the web's
    /// `useSettingsPendingOperation`): it holds navigation behind the prompt until it ends.
    pub fn set_pending_operations(&mut self, count: usize) {
        self.pending_ops = count;
    }

    pub fn flags(&self) -> Option<Flags> {
        self.flags
    }

    fn dirty_or_busy(&self) -> bool {
        self.flags.is_some_and(|f| f.dirty || f.busy)
    }

    pub fn prompt_open(&self) -> bool {
        self.prompt
    }

    /// The footer, while the section is dirty or busy.
    pub fn footer(&self) -> Option<Footer> {
        let flags = self.flags.filter(|f| f.dirty || f.busy)?;
        Some(Footer {
            status: if self.save_error {
                t("settings.editing.saveFailed")
            } else {
                t("settings.editing.unsaved")
            },
            reset_enabled: !flags.busy,
            save_enabled: !flags.busy && flags.can_save,
        })
    }

    /// The prompt's content, while it is open. The body names the request in progress when there is one.
    pub fn prompt(&self) -> Option<Prompt> {
        self.prompt.then(|| Prompt {
            title: t("settings.editing.leaveTitle"),
            body: if self.flags.is_some_and(|f| f.busy) || self.pending_ops > 0 {
                t("settings.editing.leaveBusyBody")
            } else {
                t("settings.editing.leaveBody")
            },
            confirm_label: t("settings.editing.discard"),
            cancel_label: t("settings.editing.keepEditing"),
        })
    }

    /// A request to leave the section for `dest` (a nav row, the close button).
    pub fn request(&mut self, dest: T) -> Nav<T> {
        // A section that saves as you type sends its last change first; when it lands there is nothing to lose.
        let flushable = self.flags.is_some_and(|f| !f.dirty) && self.pending_ops == 0;
        if flushable && let Some(section) = self.section.as_mut() {
            match section.flush() {
                Some(true) => return Nav::Go(dest),
                Some(false) => return self.hold(dest),
                None => {}
            }
        }
        if !self.dirty_or_busy() && self.pending_ops == 0 {
            return Nav::Go(dest);
        }
        self.hold(dest)
    }

    fn hold(&mut self, dest: T) -> Nav<T> {
        self.prompt = true;
        self.waiting.push(dest);
        Nav::Prompt
    }

    /// Keep editing: the prompt closes, the draft stays, the waiting destinations are dropped.
    pub fn keep_editing(&mut self) {
        self.prompt = false;
        self.waiting.clear();
    }

    /// Discard changes: the section resets, the prompt closes and the waiting destinations come back, oldest first.
    pub fn discard(&mut self) -> Vec<T> {
        self.prompt = false;
        if let Some(section) = self.section.as_mut() {
            section.reset();
        }
        self.save_error = false;
        std::mem::take(&mut self.waiting)
    }

    /// The footer's Save. Ignored while a save is in flight or the draft cannot be saved.
    pub fn save(&mut self) {
        let Some(flags) = self.flags else { return };
        if flags.busy || !flags.can_save {
            return;
        }
        self.save_error = false;
        let saved = self.section.as_mut().is_some_and(|s| s.save());
        self.save_error = !saved;
    }

    /// The footer's Reset. Ignored while a save is in flight.
    pub fn reset(&mut self) {
        if self.flags.is_some_and(|f| f.busy) {
            return;
        }
        if let Some(section) = self.section.as_mut() {
            section.reset();
        }
        self.save_error = false;
    }
}

#[cfg(test)]
mod tests {
    use std::cell::RefCell;
    use std::rc::Rc;

    use super::*;

    #[derive(Default)]
    struct Log {
        saves: usize,
        resets: usize,
        flushes: usize,
    }

    struct Fake {
        log: Rc<RefCell<Log>>,
        save_lands: bool,
        flush: Option<bool>,
    }

    impl Section for Fake {
        fn save(&mut self) -> bool {
            self.log.borrow_mut().saves += 1;
            self.save_lands
        }
        fn reset(&mut self) {
            self.log.borrow_mut().resets += 1;
        }
        fn flush(&mut self) -> Option<bool> {
            self.log.borrow_mut().flushes += 1;
            self.flush
        }
    }

    fn open(
        flags: Flags,
        flush: Option<bool>,
        save_lands: bool,
    ) -> (Workspace<&'static str>, Rc<RefCell<Log>>) {
        let log = Rc::new(RefCell::new(Log::default()));
        let mut ws = Workspace::new();
        ws.attach(Box::new(Fake {
            log: log.clone(),
            save_lands,
            flush,
        }));
        ws.publish(flags);
        (ws, log)
    }

    const DIRTY: Flags = Flags {
        dirty: true,
        busy: false,
        can_save: true,
    };

    /// e2e/web/search-settings.spec.ts "a dirty Profile draft survives Keep editing and is discarded only after confirmation".
    #[test]
    fn dirty_settings_draft_needs_confirmation() {
        let (mut ws, log) = open(DIRTY, None, true);
        assert_eq!(ws.footer().map(|f| f.status), Some("Unsaved changes"));
        assert_eq!(ws.request("signin"), Nav::Prompt);
        let prompt = ws.prompt().expect("the prompt is open");
        assert_eq!(prompt.title, "Discard unsaved changes?");
        assert_eq!(prompt.cancel_label, "Keep editing");
        assert_eq!(prompt.confirm_label, "Discard changes");
        // Keep editing: the draft stays, nothing was reset, nothing is waiting.
        ws.keep_editing();
        assert!(!ws.prompt_open());
        assert_eq!(log.borrow().resets, 0);
        assert!(ws.footer().is_some());
        // Again, and this time Discard: the section resets and the destination comes back.
        assert_eq!(ws.request("signin"), Nav::Prompt);
        assert_eq!(ws.discard(), vec!["signin"]);
        assert_eq!(log.borrow().resets, 1);
        assert!(!ws.prompt_open());
    }

    #[test]
    fn a_clean_section_lets_go_at_once() {
        let (mut ws, _) = open(Flags::default(), None, true);
        assert!(ws.footer().is_none());
        assert_eq!(ws.request("next"), Nav::Go("next"));
        let mut empty: Workspace<&str> = Workspace::new();
        assert_eq!(empty.request("next"), Nav::Go("next"));
    }

    #[test]
    fn a_save_in_flight_or_a_pending_operation_holds_navigation_and_says_so() {
        let busy = Flags {
            dirty: false,
            busy: true,
            can_save: false,
        };
        let (mut ws, _) = open(busy, None, true);
        assert_eq!(ws.request("next"), Nav::Prompt);
        assert_eq!(
            ws.prompt().map(|p| p.body),
            Some(t("settings.editing.leaveBusyBody"))
        );
        let footer = ws.footer().expect("busy shows the footer");
        assert!(!footer.reset_enabled && !footer.save_enabled);

        let (mut ws, _) = open(Flags::default(), None, true);
        ws.set_pending_operations(1);
        assert_eq!(ws.request("next"), Nav::Prompt);
        assert_eq!(
            ws.prompt().map(|p| p.body),
            Some(t("settings.editing.leaveBusyBody"))
        );
        ws.keep_editing();
        ws.set_pending_operations(0);
        assert_eq!(ws.request("next"), Nav::Go("next"));

        let (mut ws, _) = open(DIRTY, None, true);
        ws.request("next");
        assert_eq!(
            ws.prompt().map(|p| p.body),
            Some(t("settings.editing.leaveBody"))
        );
    }

    #[test]
    fn a_flushing_section_leaves_when_its_last_change_lands_and_holds_otherwise() {
        let (mut ws, log) = open(Flags::default(), Some(true), true);
        assert_eq!(ws.request("next"), Nav::Go("next"));
        assert_eq!(log.borrow().flushes, 1);
        let (mut ws, _) = open(Flags::default(), Some(false), true);
        assert_eq!(ws.request("next"), Nav::Prompt);
    }

    #[test]
    fn save_and_reset_follow_the_flags_and_a_failed_save_says_so() {
        let (mut ws, log) = open(DIRTY, None, false);
        ws.save();
        assert_eq!(log.borrow().saves, 1);
        assert_eq!(
            ws.footer().map(|f| f.status),
            Some("Changes could not be saved. Try again.")
        );
        ws.reset();
        assert_eq!(log.borrow().resets, 1);
        assert_eq!(ws.footer().map(|f| f.status), Some("Unsaved changes"));
        // A draft that cannot be saved, or a save in flight, ignores Save and Reset.
        let (mut ws, log) = open(
            Flags {
                dirty: true,
                busy: false,
                can_save: false,
            },
            None,
            true,
        );
        ws.save();
        assert_eq!(log.borrow().saves, 0);
        let (mut ws, log) = open(
            Flags {
                dirty: true,
                busy: true,
                can_save: true,
            },
            None,
            true,
        );
        ws.save();
        ws.reset();
        assert_eq!((log.borrow().saves, log.borrow().resets), (0, 0));
    }

    #[test]
    fn clear_resets_the_draft_and_closes_the_prompt() {
        let (mut ws, log) = open(DIRTY, None, false);
        ws.save();
        assert_eq!(ws.request("signin"), Nav::Prompt);
        ws.clear();
        assert_eq!(log.borrow().resets, 1, "the section was reset");
        assert!(
            ws.footer().is_none() && ws.flags().is_none(),
            "no footer, no flags"
        );
        assert!(!ws.prompt_open(), "the prompt closed");
        assert!(
            ws.discard().is_empty(),
            "the waiting navigation was dropped, not granted"
        );
        // The old draft's flags, published again before the section is torn down, are not registered.
        ws.publish(DIRTY);
        assert!(ws.footer().is_none());
        assert_eq!(ws.request("next"), Nav::Go("next"));
        // A clean section registers again.
        ws.publish(Flags::default());
        ws.publish(DIRTY);
        assert!(ws.footer().is_some());
        assert_eq!(
            ws.footer().map(|f| f.status),
            Some("Unsaved changes"),
            "the failed-save status is gone"
        );
    }

    #[test]
    fn detaching_drops_the_draft_state() {
        let (mut ws, _) = open(DIRTY, None, true);
        ws.detach();
        assert!(ws.footer().is_none());
        assert_eq!(ws.request("next"), Nav::Go("next"));
    }
}
