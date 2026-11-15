//! Literal mention tokens and the composer's mention matching (ports of `packages/client-core/src/state/mentions.ts` and
//! the mention helpers of `packages/web/src/screens/shell-model.ts`).

use dilla_client::state::Member;

use crate::strings::t;

/// A body cut at its mention tokens (`splitMentions`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Part {
    Text(String),
    /// `everyone`, `here` or 32 lower-case hex digits.
    Mention(String),
}

/// `<@everyone>`, `<@here>` and `<@` + 32 hex digits + `>`: the web's `TOKEN`.
pub fn split_mentions(body: &str) -> Vec<Part> {
    let mut out = Vec::new();
    let bytes = body.as_bytes();
    let mut start = 0;
    let mut at = 0;
    while at + 3 <= bytes.len() {
        if &bytes[at..at + 2] == b"<@" {
            let rest = &body[at + 2..];
            let target = ["everyone", "here"]
                .iter()
                .find(|w| rest.starts_with(**w) && rest[w.len()..].starts_with('>'))
                .map(|w| (*w).to_owned())
                .or_else(|| {
                    let id = rest.get(..32)?;
                    (id.bytes()
                        .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
                        && rest[32..].starts_with('>'))
                    .then(|| id.to_owned())
                });
            if let Some(target) = target {
                if at > start {
                    out.push(Part::Text(body[start..at].to_owned()));
                }
                let len = 2 + target.len() + 1;
                out.push(Part::Mention(target));
                at += len;
                start = at;
                continue;
            }
        }
        at += 1;
    }
    if start < body.len() {
        out.push(Part::Text(body[start..].to_owned()));
    }
    out
}

/// A member a mention can name.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MentionMember {
    pub user_id: String,
    pub username: String,
}

/// A role a mention can name: its handle is the role's name as `roleHandle` makes it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MentionRole {
    pub role_id: String,
    pub handle: String,
}

/// A role as the composer's popup sees it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RoleInfo {
    pub id: String,
    pub name: String,
    pub position: u32,
    pub mentionable: bool,
}

/// The text after `@` that names a role: NFC, lower case, whitespace runs as one dash. (No NFC table in std: the names
/// the instance accepts are already NFC, so only the case and the runs are folded here.)
pub fn role_handle(name: &str) -> String {
    name.to_lowercase()
        .split_whitespace()
        .collect::<Vec<_>>()
        .join("-")
}

fn boundary(chars: &[char], at: usize) -> bool {
    at == 0 || chars[at - 1].is_whitespace() || "([{\"'".contains(chars[at - 1])
}

fn name_char(c: char) -> bool {
    c.is_alphabetic() || c.is_numeric() || c == '_' || c == '-' || is_mark(c)
}

/// `\p{M}`, for the ranges that matter in names (the combining blocks).
fn is_mark(c: char) -> bool {
    matches!(c as u32, 0x300..=0x36f | 0x483..=0x489 | 0x591..=0x5bd | 0x610..=0x61a | 0x64b..=0x65f | 0x900..=0x903
        | 0x93a..=0x94f | 0x1ab0..=0x1aff | 0x1dc0..=0x1dff | 0x20d0..=0x20ff | 0x3099..=0x309a | 0xfe20..=0xfe2f)
}

fn word_char(c: char) -> bool {
    name_char(c) || c == '.'
}

/// CJK has no spaces, so a mention ends where such text starts.
fn cjk(c: char) -> bool {
    matches!(c as u32, 0x4e00..=0x9fff | 0x3400..=0x4dbf | 0x20000..=0x2a6df | 0xf900..=0xfaff | 0x3040..=0x309f
        | 0x30a0..=0x30ff | 0xac00..=0xd7af | 0x1100..=0x11ff | 0x3130..=0x318f)
}

/// A candidate ends a mention at the end of the text, before CJK text, at a dot run that no name continues past, or at
/// any other non-name character, so `@ada@bob` still reads `@ada`.
fn ends_mention(chars: &[char], at: usize) -> bool {
    let Some(&ch) = chars.get(at) else {
        return true;
    };
    if cjk(ch) {
        return true;
    }
    if ch == '.' {
        let mut dots = at;
        while dots < chars.len() && chars[dots] == '.' {
            dots += 1;
        }
        return chars.get(dots).is_none_or(|c| !name_char(*c) || cjk(*c));
    }
    !name_char(ch)
}

/// Longest match wins; on equal length broadcast beats member, member beats role (first listed wins ties).
pub fn encode_mentions(
    text: &str,
    members: &[MentionMember],
    broadcast: bool,
    roles: &[MentionRole],
) -> String {
    if !text.contains('@') {
        return text.to_owned();
    }
    let mut candidates: Vec<(Vec<char>, String)> = Vec::new();
    if broadcast {
        candidates.push((chars_of("everyone"), "everyone".into()));
        candidates.push((chars_of("here"), "here".into()));
    }
    for m in members {
        candidates.push((chars_of(&m.username.to_lowercase()), m.user_id.clone()));
    }
    for r in roles {
        candidates.push((chars_of(&r.handle.to_lowercase()), r.role_id.clone()));
    }
    let chars: Vec<char> = text.chars().collect();
    let lower: Vec<char> = chars.iter().flat_map(|c| c.to_lowercase()).collect();
    // Lower-casing may change the length of a character (rare); fall back to per-char folding then.
    let aligned = lower.len() == chars.len();
    let fold = |at: usize, len: usize| -> Vec<char> {
        if aligned {
            lower[at..(at + len).min(lower.len())].to_vec()
        } else {
            chars[at..(at + len).min(chars.len())]
                .iter()
                .flat_map(|c| c.to_lowercase())
                .collect()
        }
    };
    let mut out = String::new();
    let mut i = 0;
    while i < chars.len() {
        if chars[i] != '@' || !boundary(&chars, i) {
            out.push(chars[i]);
            i += 1;
            continue;
        }
        let mut best: Option<&(Vec<char>, String)> = None;
        for c in &candidates {
            if c.0.is_empty() || best.is_some_and(|b| c.0.len() <= b.0.len()) {
                continue;
            }
            if i + 1 + c.0.len() <= chars.len()
                && fold(i + 1, c.0.len()) == c.0
                && ends_mention(&chars, i + 1 + c.0.len())
            {
                best = Some(c);
            }
        }
        if let Some((handle, target)) = best {
            out.push_str(&format!("<@{target}>"));
            i += 1 + handle.len();
        } else {
            out.push(chars[i]);
            i += 1;
        }
    }
    out
}

fn chars_of(s: &str) -> Vec<char> {
    s.chars().collect()
}

/// What the caret is on: the `@` it follows and the text between (`mentionQuery`). Offsets are in characters.
pub fn mention_query(text: &str, caret: usize) -> Option<(usize, String)> {
    let chars: Vec<char> = text.chars().collect();
    if caret > chars.len() {
        return None;
    }
    let mut at = caret;
    while at > 0 && word_char(chars[at - 1]) {
        at -= 1;
    }
    if at == 0 || chars[at - 1] != '@' || !boundary(&chars, at - 1) || caret - at > 32 {
        return None;
    }
    Some((at - 1, chars[at..caret].iter().collect()))
}

/// The user or one of their roles named, or @everyone, or @here (`mentionsMe`).
pub fn mentions_me(body: &str, user_hex: &str, role_hexes: &[String]) -> bool {
    body.contains(&format!("<@{user_hex}>"))
        || body.contains("<@everyone>")
        || body.contains("<@here>")
        || role_hexes.iter().any(|r| body.contains(&format!("<@{r}>")))
}

/// Roles offered in the popup: above the base position, and mentionable or broadcast-allowed.
pub fn offered_roles(roles: &[RoleInfo], broadcast: bool) -> Vec<RoleInfo> {
    roles
        .iter()
        .filter(|r| r.position > 0 && (broadcast || r.mentionable))
        .cloned()
        .collect()
}

/// For an edit: the offered roles plus every role the message already mentions, so no token is lost by saving.
pub fn edit_roles(roles: &[RoleInfo], broadcast: bool, body: &str) -> Vec<RoleInfo> {
    roles
        .iter()
        .filter(|r| {
            r.position > 0 && (broadcast || r.mentionable || body.contains(&format!("<@{}>", r.id)))
        })
        .cloned()
        .collect()
}

/// Roles in the order they are offered (position, highest first, then name), without any whose handle is taken by
/// @everyone, @here, a member or a role before it.
fn reachable<'a>(roles: &'a [RoleInfo], usernames: &[&str]) -> Vec<(&'a RoleInfo, String)> {
    let mut taken: Vec<String> = vec!["everyone".into(), "here".into()];
    taken.extend(usernames.iter().map(|u| u.to_lowercase()));
    let mut sorted: Vec<&RoleInfo> = roles.iter().collect();
    sorted.sort_by(|a, b| {
        b.position
            .cmp(&a.position)
            .then_with(|| a.name.cmp(&b.name))
    });
    let mut out = Vec::new();
    for role in sorted {
        let handle = role_handle(&role.name);
        if handle.is_empty() || taken.contains(&handle) {
            continue;
        }
        taken.push(handle.clone());
        out.push((role, handle));
    }
    out
}

/// The roles a body's text can name.
pub fn mention_roles(roles: &[RoleInfo], usernames: &[&str]) -> Vec<MentionRole> {
    reachable(roles, usernames)
        .into_iter()
        .map(|(r, handle)| MentionRole {
            role_id: r.id.clone(),
            handle,
        })
        .collect()
}

/// One line of the mention popup.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MentionOption {
    /// `m-<user>`, `r-<role>` or `b-everyone`.
    pub id: String,
    pub primary: String,
    pub secondary: String,
    pub insert: String,
}

/// The people (and roles, and @everyone) the popup offers for `query`: at most eight members by username prefix, then by
/// display-name prefix, then the roles that fit, then @everyone.
pub fn mention_options(
    query: &str,
    candidates: &[Member],
    broadcast: bool,
    roles: &[RoleInfo],
) -> Vec<MentionOption> {
    let q = query.to_lowercase();
    let by_name =
        |a: &&Member, b: &&Member| a.username.to_lowercase().cmp(&b.username.to_lowercase());
    let by_display =
        |a: &&Member, b: &&Member| a.display.to_lowercase().cmp(&b.display.to_lowercase());
    let mut username: Vec<&Member> = candidates
        .iter()
        .filter(|m| m.username.to_lowercase().starts_with(&q))
        .collect();
    username.sort_by(by_name);
    let mut display: Vec<&Member> = candidates
        .iter()
        .filter(|m| {
            !m.username.to_lowercase().starts_with(&q) && m.display.to_lowercase().starts_with(&q)
        })
        .collect();
    display.sort_by(by_display);
    let mut result: Vec<MentionOption> = username
        .into_iter()
        .chain(display)
        .take(8)
        .map(|m| MentionOption {
            id: format!("m-{}", m.user_id.hex()),
            primary: if m.display.is_empty() {
                m.username.clone()
            } else {
                m.display.clone()
            },
            secondary: format!("@{}", m.username),
            insert: format!("@{} ", m.username),
        })
        .collect();
    let names: Vec<&str> = candidates.iter().map(|m| m.username.as_str()).collect();
    let room = 8usize.saturating_sub(result.len());
    for (r, handle) in reachable(roles, &names)
        .into_iter()
        .filter(|(_, h)| h.starts_with(&q))
        .take(room)
    {
        result.push(MentionOption {
            id: format!("r-{}", r.id),
            primary: crate::strings::tf("mention.role", &[("name", &r.name)]),
            secondary: t("mention.role.secondary").to_owned(),
            insert: format!("@{handle} "),
        });
    }
    if broadcast && "everyone".starts_with(&q) {
        result.push(MentionOption {
            id: "b-everyone".into(),
            primary: "@everyone".into(),
            secondary: t("shell.composer.mentionEveryone").to_owned(),
            insert: "@everyone ".into(),
        });
    }
    result
}

/// A body as the edit field shows it: each token as the `@name` that encodes back to it (`editableText`).
pub fn editable_text(
    body: &str,
    members: &[Member],
    self_user: Option<(&str, &str)>,
    roles: &[RoleInfo],
) -> String {
    split_mentions(body)
        .into_iter()
        .map(|part| match part {
            Part::Text(t) => t,
            Part::Mention(target) if target == "everyone" || target == "here" => {
                format!("@{target}")
            }
            Part::Mention(target) => {
                if let Some(m) = members
                    .iter()
                    .find(|m| m.user_id.hex() == target && !m.username.is_empty())
                {
                    return format!("@{}", m.username);
                }
                if let Some((id, name)) = self_user
                    && id == target
                {
                    return format!("@{name}");
                }
                if let Some(r) = roles.iter().find(|r| r.id == target) {
                    return format!("@{}", role_handle(&r.name));
                }
                format!("<@{target}>")
            }
        })
        .collect()
}

/// shell-model.ts `plainBody`: the text a row shows, each mention token as the label the log draws for it (`@everyone`,
/// a member's display name else username, the reader's own username, a known role as "@{name}", a role a member holds as
/// "@role", else the unknown-mention label). `self_user` is the reader's `(user id hex, username)`; `roles` the
/// community's roles whose names are read.
pub fn plain_body(
    body: &str,
    members: &[Member],
    self_user: Option<(&str, &str)>,
    roles: &[RoleInfo],
) -> String {
    split_mentions(body)
        .into_iter()
        .map(|part| match part {
            Part::Text(text) => text,
            Part::Mention(target) if target == "everyone" || target == "here" => {
                format!("@{target}")
            }
            Part::Mention(target) => {
                if let Some(m) = members.iter().find(|m| m.user_id.hex() == target) {
                    let name = if m.display.is_empty() {
                        &m.username
                    } else {
                        &m.display
                    };
                    return format!("@{name}");
                }
                if let Some((id, name)) = self_user
                    && id == target
                {
                    return format!("@{name}");
                }
                if let Some(r) = roles.iter().find(|r| r.id == target) {
                    return crate::strings::tf("mention.role", &[("name", &r.name)]);
                }
                if members.iter().any(|m| {
                    m.role_ids
                        .iter()
                        .any(|r| dilla_client::features::server_admin::hex_id(r) == target)
                }) {
                    return t("shell.message.mentionRole").to_owned();
                }
                crate::strings::tf(
                    "shell.message.mentionUnknown",
                    &[("id", &target[..target.len().min(8)])],
                )
            }
        })
        .collect()
}

/// The first `max` characters of `value`, line breaks as spaces (`excerpt`).
pub fn excerpt(value: &str, max: usize) -> String {
    value
        .chars()
        .map(|c| if c == '\r' || c == '\n' { ' ' } else { c })
        .take(max)
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::UserId;

    const ID: &str = "0123456789abcdef0123456789abcdef";

    fn member(user: u8, username: &str, display: &str) -> Member {
        Member {
            user_id: UserId([user; 16]),
            username: username.into(),
            display: display.into(),
            kind: 0,
            role_ids: Vec::new(),
            status_msg: String::new(),
            bio: String::new(),
        }
    }

    fn mm(id: &str, username: &str) -> MentionMember {
        MentionMember {
            user_id: id.into(),
            username: username.into(),
        }
    }

    #[test]
    fn tokens_split_a_body() {
        assert_eq!(
            split_mentions(&format!("hi <@{ID}> and <@everyone>!")),
            vec![
                Part::Text("hi ".into()),
                Part::Mention(ID.into()),
                Part::Text(" and ".into()),
                Part::Mention("everyone".into()),
                Part::Text("!".into()),
            ]
        );
        // Upper-case hex, a short id and an unclosed token stay text.
        for body in [
            "<@ABCDEF>",
            "<@everyone",
            "<@here ",
            &format!("<@{}>", ID.to_uppercase()),
        ] {
            assert_eq!(
                split_mentions(body),
                vec![Part::Text(body.to_owned())],
                "{body}"
            );
        }
    }

    #[test]
    fn the_longest_name_wins_and_the_name_must_end() {
        let members = [mm("a", "ada"), mm("b", "adam")];
        assert_eq!(
            encode_mentions("hi @adam!", &members, false, &[]),
            "hi <@b>!"
        );
        // The second @ follows a name character, not a boundary: it stays text, as on the web.
        assert_eq!(
            encode_mentions("hi @ada@adam", &members, false, &[]),
            "hi <@a>@adam"
        );
        // The name runs on: not a mention.
        assert_eq!(
            encode_mentions("hi @adas", &members, false, &[]),
            "hi @adas"
        );
        // A trailing dot ends it; a dot inside a name does not.
        assert_eq!(encode_mentions("@ada.", &members, false, &[]), "<@a>.");
        assert_eq!(encode_mentions("@ada.b", &members, false, &[]), "@ada.b");
        // Only at a boundary.
        assert_eq!(
            encode_mentions("mail@ada", &members, false, &[]),
            "mail@ada"
        );
        assert_eq!(encode_mentions("(@ada)", &members, false, &[]), "(<@a>)");
        // Case-insensitive.
        assert_eq!(encode_mentions("@ADA", &members, false, &[]), "<@a>");
    }

    #[test]
    fn broadcast_beats_member_and_needs_the_right() {
        let members = [mm("x", "everyone")];
        assert_eq!(
            encode_mentions("@everyone", &members, true, &[]),
            "<@everyone>"
        );
        assert_eq!(encode_mentions("@everyone", &members, false, &[]), "<@x>");
        assert_eq!(encode_mentions("@here", &[], true, &[]), "<@here>");
        assert_eq!(encode_mentions("@here", &[], false, &[]), "@here");
        let roles = [MentionRole {
            role_id: "r".into(),
            handle: "mods".into(),
        }];
        assert_eq!(encode_mentions("@mods", &[], false, &roles), "<@r>");
    }

    #[test]
    fn a_mention_ends_before_cjk_text() {
        let members = [mm("a", "ada")];
        assert_eq!(
            encode_mentions("@ada你好", &members, false, &[]),
            "<@a>你好"
        );
    }

    #[test]
    fn the_query_is_what_the_caret_follows() {
        assert_eq!(mention_query("hi @ad", 6), Some((3, "ad".into())));
        assert_eq!(mention_query("hi @", 4), Some((3, String::new())));
        assert_eq!(mention_query("hi @ad", 3), None);
        assert_eq!(mention_query("mail@ad", 7), None, "not at a boundary");
        assert_eq!(mention_query("@a b", 4), None, "a space ends it");
        assert_eq!(mention_query("x", 9), None, "a caret past the text");
        assert_eq!(
            mention_query(&format!("@{}", "a".repeat(33)), 34),
            None,
            "at most 32 characters"
        );
    }

    #[test]
    fn mentions_me_names_the_user_a_role_or_everyone() {
        assert!(mentions_me(&format!("hey <@{ID}>"), ID, &[]));
        assert!(mentions_me("<@everyone>", ID, &[]));
        assert!(mentions_me("<@here>", ID, &[]));
        assert!(mentions_me("<@rr>", ID, &["rr".into()]));
        assert!(!mentions_me("nothing", ID, &["rr".into()]));
    }

    #[test]
    fn options_list_username_prefixes_then_display_prefixes() {
        let people = [
            member(1, "bob", "Alice B"),
            member(2, "ana", "Zed"),
            member(3, "alan", "Q"),
        ];
        let o = mention_options("al", &people, false, &[]);
        let ids: Vec<&str> = o.iter().map(|o| o.id.as_str()).collect();
        // alan (username prefix) first, then bob (display "Alice B").
        assert_eq!(
            ids,
            [
                format!("m-{}", UserId([3; 16]).hex()),
                format!("m-{}", UserId([1; 16]).hex())
            ]
        );
        assert_eq!(o[0].insert, "@alan ");
        assert_eq!(o[1].primary, "Alice B");
        assert_eq!(o[1].secondary, "@bob");
    }

    #[test]
    fn options_end_with_roles_then_everyone() {
        let roles = [RoleInfo {
            id: "r1".into(),
            name: "Raid Team".into(),
            position: 3,
            mentionable: true,
        }];
        let o = mention_options("", &[member(1, "ada", "")], true, &roles);
        let ids: Vec<&str> = o.iter().map(|o| o.id.as_str()).collect();
        assert_eq!(ids.len(), 3);
        assert_eq!(o[1].id, "r-r1");
        assert_eq!(o[1].insert, "@raid-team ");
        assert_eq!(o[2].id, "b-everyone");
        // Without the right, a role that is not mentionable and @everyone are not offered.
        let hidden = [RoleInfo {
            mentionable: false,
            ..roles[0].clone()
        }];
        assert_eq!(
            mention_options("", &[], false, &offered_roles(&hidden, false)).len(),
            0
        );
    }

    #[test]
    fn at_most_eight_options() {
        let people: Vec<Member> = (0..12)
            .map(|n| member(n, &format!("u{n:02}"), ""))
            .collect();
        assert_eq!(
            mention_options("", &people, true, &[]).len(),
            9,
            "eight people and @everyone"
        );
    }

    #[test]
    fn editing_shows_names_and_keeps_unknown_tokens() {
        let people = [member(7, "ada", "")];
        let body = format!("<@{}> <@everyone> <@{ID}> <@here>", UserId([7; 16]).hex());
        assert_eq!(
            editable_text(&body, &people, None, &[]),
            format!("@ada @everyone <@{ID}> @here")
        );
        assert_eq!(
            editable_text(&format!("<@{ID}>"), &[], Some((ID, "me")), &[]),
            "@me"
        );
        // The round trip: the edit text encodes back to the body.
        let text = editable_text(&body, &people, None, &[]);
        let back = encode_mentions(&text, &[mm(&UserId([7; 16]).hex(), "ada")], true, &[]);
        assert_eq!(back, body);
    }

    #[test]
    fn excerpts_flatten_lines() {
        assert_eq!(excerpt("a\nb\r\nc", 4), "a b ");
    }

    /// shell-model.ts `plainBody` (the moderated removal's excerpt): mention tokens read as the log's labels.
    #[test]
    fn a_plain_body_reads_mentions_as_their_labels() {
        let people = [member(7, "ada", "Ada L"), member(8, "bjorn", "")];
        let (ada, bjorn) = (UserId([7; 16]).hex(), UserId([8; 16]).hex());
        let body = format!("hi <@{ada}> and <@{bjorn}>, <@everyone> <@{ID}>");
        assert_eq!(
            plain_body(&body, &people, None, &[]),
            "hi @Ada L and @bjorn, @everyone @01234567"
        );
        assert_eq!(
            plain_body(&format!("<@{ID}>"), &[], Some((ID, "me")), &[]),
            "@me"
        );
    }

    /// Review A I2 (shell-model.ts `mentionPill`'s role arms): a known role reads as "@{name}", a role only a member
    /// holds (its name not read) as "@role", and only then is a token unknown, so the removal's excerpt is the web's.
    #[test]
    fn a_plain_body_reads_role_mentions_as_the_web_does() {
        let known = "aa".repeat(16);
        let held = "bb".repeat(16);
        let mut holder = member(9, "cato", "");
        holder.role_ids = vec![[0xbb; 16]];
        let roles = [RoleInfo {
            id: known.clone(),
            name: "Moderators".into(),
            position: 1,
            mentionable: true,
        }];
        assert_eq!(
            plain_body(
                &format!("ping <@{known}> <@{held}> <@{ID}> spam"),
                &[holder],
                None,
                &roles
            ),
            "ping @Moderators @role @01234567 spam"
        );
    }
}
