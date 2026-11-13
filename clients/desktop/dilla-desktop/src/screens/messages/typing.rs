//! The typing indicator (packages/ui `TypingIndicator`): "Björn is typing…" in the accent in the composer's foot, "Several
//! people are typing…" for more than one; nothing while nobody types. Names repeat once.

use dilla_client::state::{AppState, ChannelId};

use crate::strings::{t, tf};

/// The text for the others typing in `channel` at `now_ms` (unix ms), `None` when nobody does.
pub fn text(state: &AppState, channel: ChannelId, now_ms: u64) -> Option<String> {
    let rows = state.messages.typing.get(&channel)?;
    let mut names: Vec<&str> = Vec::new();
    for r in rows.iter().filter(|r| r.expires_at_ms > now_ms) {
        if !names.contains(&r.name.as_str()) {
            names.push(r.name.as_str());
        }
    }
    match names.as_slice() {
        [] => None,
        [one] => Some(tf("typing.one", &[("name", one)])),
        _ => Some(t("typing.many").to_owned()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::UserId;
    use dilla_client::state::messages::Typer;

    fn state_with(names: &[(&str, u8, u64)]) -> (AppState, ChannelId) {
        let channel = ChannelId([5; 16]);
        let mut s = AppState::default();
        s.messages.typing.insert(
            channel,
            names
                .iter()
                .map(|(n, u, exp)| Typer {
                    user: UserId([*u; 16]),
                    name: (*n).to_owned(),
                    expires_at_ms: *exp,
                })
                .collect(),
        );
        (s, channel)
    }

    #[test]
    fn one_person_is_named_several_are_not() {
        let (s, c) = state_with(&[("Ada", 1, 100)]);
        assert_eq!(text(&s, c, 50).as_deref(), Some("Ada is typing…"));
        let (s, c) = state_with(&[("Ada", 1, 100), ("Mira", 2, 100)]);
        assert_eq!(
            text(&s, c, 50).as_deref(),
            Some("Several people are typing…")
        );
    }

    #[test]
    fn the_same_name_twice_is_one_person() {
        let (s, c) = state_with(&[("Ada", 1, 100), ("Ada", 2, 100)]);
        assert_eq!(text(&s, c, 50).as_deref(), Some("Ada is typing…"));
    }

    #[test]
    fn an_expired_row_and_an_empty_list_say_nothing() {
        let (s, c) = state_with(&[("Ada", 1, 100)]);
        assert_eq!(text(&s, c, 100), None);
        assert_eq!(text(&AppState::default(), c, 0), None);
        let (s, c) = state_with(&[]);
        assert_eq!(text(&s, c, 0), None);
    }
}
