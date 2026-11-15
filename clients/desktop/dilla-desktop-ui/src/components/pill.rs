//! `Pill` (packages/ui/src/Pill): the unread and mention count pill. The drawing is `Badge`'s (the desktop's name for it
//! from Task 5); this module is the web's name for it and the pill's accessible label.

pub use super::badge::{Badge as Pill, BadgeKind as PillKind, capped};

/// The hidden label that names the count (`Pill.tsx`): `3 unread`, `1 mention`, `2 mentions`.
pub fn accessible_label(kind: PillKind, count: u32) -> String {
    match kind {
        PillKind::Unread => format!("{count} unread"),
        PillKind::Mention if count == 1 => format!("{count} mention"),
        PillKind::Mention => format!("{count} mentions"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_hidden_label_names_the_real_count() {
        assert_eq!(accessible_label(PillKind::Unread, 120), "120 unread");
        assert_eq!(accessible_label(PillKind::Mention, 1), "1 mention");
        assert_eq!(accessible_label(PillKind::Mention, 2), "2 mentions");
        assert_eq!(capped(120), "99+");
    }
}
