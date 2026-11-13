//! The reaction set (packages/web/src/emoji.ts, "the prototype's fixed, ordered emoji set"): the 32 emoji the picker
//! offers, each named by a `shell.emoji.NN` string, and the two rules for showing an emoji received from someone else.

use crate::strings::t;

/// The prototype's 32, in order. The `n`th is named by `shell.emoji.{n+1:02}`.
pub const REACTION_EMOJI: [&str; 32] = [
    "👍", "👎", "😂", "❤️", "🔥", "🎉", "😮", "😢", "😡", "🙏", "👀", "💯", "✅", "❌", "🛡️", "⚔️",
    "🏹", "🪓", "🍻", "🍕", "🎮", "🎵", "🌊", "⛵", "🐉", "👑", "💀", "🤝", "😎", "🤔", "🫡", "🧭",
];

/// The string key naming the `index`th emoji of the set.
pub fn name_key(index: usize) -> String {
    format!("shell.emoji.{:02}", index + 1)
}

/// `reactionDisplay`: the emoji as a person may see it: the control, bidi and line-separator characters a sender could
/// use to impersonate other text are removed, then it is trimmed.
pub fn reaction_display(emoji: &str) -> String {
    let kept: String = emoji
        .chars()
        .filter(|c| {
            !matches!(
                *c,
                '\u{0000}'..='\u{001f}'
                    | '\u{007f}'..='\u{009f}'
                    | '\u{061c}'
                    | '\u{200e}'..='\u{200f}'
                    | '\u{2028}'..='\u{202e}'
                    | '\u{2066}'..='\u{206f}'
            )
        })
        .collect();
    kept.trim().to_owned()
}

/// `reactionName`: the set's name for an emoji, else the emoji itself (cleaned), else "unknown reaction".
pub fn reaction_name(emoji: &str) -> String {
    if let Some(i) = REACTION_EMOJI.iter().position(|e| *e == emoji) {
        return t(&name_key(i)).to_owned();
    }
    let shown = reaction_display(emoji);
    if shown.is_empty() {
        t("shell.message.unknownReaction").to_owned()
    } else {
        shown
    }
}

/// What a chip draws for `emoji`: [`reaction_display`], or "unknown reaction" when nothing is left of it.
pub fn chip_display(emoji: &str) -> String {
    let shown = reaction_display(emoji);
    if shown.is_empty() {
        t("shell.message.unknownReaction").to_owned()
    } else {
        shown
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// packages/web/src/emoji.test.ts, "is the prototype's 32, in order, each named by its key".
    #[test]
    fn the_set_is_the_prototypes_32_named_by_their_keys() {
        let names: Vec<&str> = (0..32).map(|i| t(&name_key(i))).collect();
        assert_eq!(
            names,
            [
                "thumbs up",
                "thumbs down",
                "joy",
                "heart",
                "fire",
                "tada",
                "open mouth",
                "cry",
                "rage",
                "pray",
                "eyes",
                "100",
                "check",
                "x",
                "shield",
                "crossed swords",
                "bow and arrow",
                "axe",
                "beers",
                "pizza",
                "video game",
                "music",
                "wave",
                "sailboat",
                "dragon",
                "crown",
                "skull",
                "handshake",
                "sunglasses",
                "thinking",
                "salute",
                "compass"
            ]
        );
        assert_eq!(REACTION_EMOJI[3], "❤️");
        assert_eq!(REACTION_EMOJI[31], "🧭");
    }

    /// emoji.test.ts, "names an emoji outside the set by itself".
    #[test]
    fn an_emoji_outside_the_set_is_named_by_itself() {
        assert_eq!(reaction_name("\u{1F680}"), "🚀");
        assert_eq!(reaction_name("❤️"), "heart");
        assert_eq!(reaction_name("\u{1F984}"), "\u{1F984}");
    }

    /// emoji.test.ts, "removes received controls while retaining ordinary unknown emoji".
    #[test]
    fn received_controls_are_removed() {
        assert_eq!(reaction_name("\nAdmin: pinned"), "Admin: pinned");
        assert_eq!(reaction_name("\u{202e}gpj.exe"), "gpj.exe");
        assert_eq!(reaction_name("\u{2066}go\u{2069}"), "go");
        assert_eq!(
            reaction_name("fake\u{2028}status\u{2029}line"),
            "fakestatusline"
        );
        assert_eq!(reaction_name("\n\u{202e}"), "unknown reaction");
        assert_eq!(reaction_name("🦄"), "🦄");
    }
}
