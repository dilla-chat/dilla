//! Arrow, Home and End movement over a list (`packages/ui/src/internal/roving.ts`, `arrow-select.ts`) and the keys that open
//! a context menu (`contextMenuTrigger.ts`). Pure: the caller moves focus and selects.

use gpui::{App, ElementId, FocusHandle, Keystroke, Window};

/// How a list answers the arrows.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Roving {
    /// A rail or a channel list (`useRovingFocus`): Up and Down step and stop at the ends, Home and End jump. Left and
    /// Right are not the list's.
    List,
    /// A radio group (`arrowIndex`): Left and Right wrap; with `both` Up and Down do too. Home and End jump.
    Radio { both: bool },
}

impl Roving {
    /// The index to move to from `from` among `len` items for `key` (a GPUI key name: `down`, `home`), or `None` when the
    /// key is not the list's. A key with Alt, Ctrl, Meta (platform) or Shift is never one: those belong to the shell.
    pub fn next(self, key: &Keystroke, from: usize, len: usize) -> Option<usize> {
        let m = &key.modifiers;
        if len == 0 || m.alt || m.control || m.platform || m.shift {
            return None;
        }
        let last = len - 1;
        match (self, key.key.as_str()) {
            (_, "home") => Some(0),
            (_, "end") => Some(last),
            (Self::List, "down") => Some((from + 1).min(last)),
            (Self::List, "up") => Some(from.saturating_sub(1)),
            (Self::Radio { .. }, "right") => Some((from + 1) % len),
            (Self::Radio { .. }, "left") => Some((from + len - 1) % len),
            (Self::Radio { both: true }, "down") => Some((from + 1) % len),
            (Self::Radio { both: true }, "up") => Some((from + len - 1) % len),
            _ => None,
        }
    }

    /// The index of the single tab stop: the item that last had focus, else the active one, else the first
    /// (`useRovingFocus`).
    pub fn stop(last_focused: Option<usize>, active: Option<usize>, len: usize) -> usize {
        [last_focused, active]
            .into_iter()
            .flatten()
            .find(|i| *i < len)
            .unwrap_or(0)
    }
}

/// `count` focus handles that live as long as the element `key` keeps rendering, so a list can move focus between its items
/// (the handles grow with the list and never shrink).
pub fn focus_handles(
    key: impl Into<ElementId>,
    count: usize,
    window: &mut Window,
    cx: &mut App,
) -> Vec<FocusHandle> {
    let state = window.use_keyed_state(key, cx, |_, _| Vec::<FocusHandle>::new());
    state.update(cx, |handles, cx| {
        while handles.len() < count {
            handles.push(cx.focus_handle());
        }
        handles[..count].to_vec()
    })
}

/// A menu's own arrow rule (ActionMenu: arrows wrap, Home and End jump): the next index among `len` items.
pub fn menu_next(key: &str, from: Option<usize>, len: usize) -> Option<usize> {
    if len == 0 {
        return None;
    }
    let last = len - 1;
    match key {
        "down" => Some(from.map_or(0, |i| (i + 1) % len)),
        "up" => Some(from.map_or(last, |i| (i + len - 1) % len)),
        "home" => Some(0),
        "end" => Some(last),
        _ => None,
    }
}

/// Whether `key` asks for a context menu: the Menu key or Shift+F10 (`contextMenuTrigger.ts`).
pub fn opens_context_menu(key: &Keystroke) -> bool {
    key.key == "menu" || (key.key == "f10" && key.modifiers.shift)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn k(s: &'static str) -> Keystroke {
        Keystroke::parse(s).unwrap()
    }

    #[test]
    fn a_list_steps_and_stops_at_its_ends() {
        let l = Roving::List;
        assert_eq!(l.next(&k("down"), 1, 3), Some(2));
        assert_eq!(l.next(&k("down"), 2, 3), Some(2));
        assert_eq!(l.next(&k("up"), 0, 3), Some(0));
        assert_eq!(l.next(&k("home"), 2, 3), Some(0));
        assert_eq!(l.next(&k("end"), 0, 3), Some(2));
        assert_eq!(l.next(&k("right"), 0, 3), None);
        assert_eq!(l.next(&k("shift-down"), 0, 3), None, "a chord is the shell's");
        assert_eq!(l.next(&k("alt-down"), 0, 3), None);
        assert_eq!(l.next(&k("down"), 0, 0), None);
    }

    #[test]
    fn a_radio_group_wraps() {
        let r = Roving::Radio { both: false };
        assert_eq!(r.next(&k("right"), 2, 3), Some(0));
        assert_eq!(r.next(&k("left"), 0, 3), Some(2));
        assert_eq!(r.next(&k("down"), 0, 3), None);
        let both = Roving::Radio { both: true };
        assert_eq!(both.next(&k("down"), 2, 3), Some(0));
        assert_eq!(both.next(&k("up"), 0, 3), Some(2));
    }

    #[test]
    fn the_tab_stop_is_the_last_focused_else_the_active_else_the_first() {
        assert_eq!(Roving::stop(Some(2), Some(1), 3), 2);
        assert_eq!(Roving::stop(None, Some(1), 3), 1);
        assert_eq!(Roving::stop(Some(5), Some(1), 3), 1);
        assert_eq!(Roving::stop(None, None, 3), 0);
    }

    #[test]
    fn a_menu_wraps_and_starts_at_either_end() {
        assert_eq!(menu_next("down", None, 3), Some(0));
        assert_eq!(menu_next("up", None, 3), Some(2));
        assert_eq!(menu_next("down", Some(2), 3), Some(0));
        assert_eq!(menu_next("up", Some(0), 3), Some(2));
        assert_eq!(menu_next("end", Some(0), 3), Some(2));
    }

    #[test]
    fn the_context_menu_keys() {
        assert!(opens_context_menu(&k("shift-f10")));
        assert!(opens_context_menu(&k("menu")));
        assert!(!opens_context_menu(&k("f10")));
        assert!(!opens_context_menu(&k("enter")));
    }
}
