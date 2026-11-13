//! Message markdown (port of `packages/web/src/screens/markdown.ts`, the same grammar rule for rule): a small, pure
//! parser. Its output is a tree, never HTML, and nothing here fetches anything.
//!
//! Supported: `**strong**`, `*em*`, `_em_`, `~~strike~~`, `` `code` ``, fenced code, `> quote` lines, and bare http(s)
//! and mailto links. A link always shows exactly the address it opens (the normalised href, with a non-ASCII host as
//! punycode). Everything else, including `[text](url)` and raw HTML, stays literal text.
//!
//! Cost: every scan that could repeat (the closer of an emphasis marker, the closer of a code span, the closing fence)
//! is answered from an index built once per call, so a parse is O(n log n); link attempts draw on a fixed work budget.

use std::collections::HashMap;
use std::sync::{Arc, OnceLock};

use super::cache::{Lru, Shared};
use super::mentions::{Part, split_mentions};

/// A parsed body before mentions are resolved to pills (the web's `Tree`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Tree {
    Text(String),
    /// A mention token's target: `everyone`, `here` or 32 hex digits.
    Ref(String),
    Strong(Vec<Tree>),
    Em(Vec<Tree>),
    Strike(Vec<Tree>),
    Quote(Vec<Tree>),
    Code(String),
    CodeBlock {
        text: String,
        lang: Option<String>,
    },
    Link {
        href: String,
        text: String,
    },
}

/// The parse and whether it holds a link (a body with a link has its bidi controls removed).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Parsed {
    pub tree: Vec<Tree>,
    pub links: bool,
}

/// One character, or one mention token (kept whole so code can give its literal form back).
#[derive(Clone, Debug, PartialEq, Eq)]
enum Item {
    C(char),
    M(String),
}

impl Item {
    fn ch(&self) -> Option<char> {
        match self {
            Self::C(c) => Some(*c),
            Self::M(_) => None,
        }
    }
    fn is(&self, c: char) -> bool {
        self.ch() == Some(c)
    }
}

const MAX_DEPTH: usize = 6;
const LINK_BUDGET: i64 = 100_000;

fn escapable(c: char) -> bool {
    "\\`*_~>[]()<#".contains(c)
}

/// JavaScript's `\s`.
fn js_space(c: char) -> bool {
    matches!(
        c,
        '\t' | '\n' | '\u{b}' | '\u{c}' | '\r' | ' ' | '\u{a0}' | '\u{1680}' | '\u{2000}'
            ..='\u{200a}'
                | '\u{2028}'
                | '\u{2029}'
                | '\u{202f}'
                | '\u{205f}'
                | '\u{3000}'
                | '\u{feff}'
    )
}

fn js_trim(s: &str) -> &str {
    s.trim_matches(js_space)
}

/// `[\p{L}\p{N}]`.
fn word_char(c: char) -> bool {
    c.is_alphabetic() || c.is_numeric()
}

/// Unicode format characters (`\p{Cf}`).
fn format_char(c: char) -> bool {
    matches!(c as u32,
        0xad | 0x600..=0x605 | 0x61c | 0x6dd | 0x70f | 0x890..=0x891 | 0x8e2 | 0x180e | 0x200b..=0x200f
        | 0x202a..=0x202e | 0x2060..=0x2064 | 0x2066..=0x206f | 0xfeff | 0xfff9..=0xfffb | 0x110bd | 0x110cd
        | 0x13430..=0x1343f | 0x1bca0..=0x1bca3 | 0x1d173..=0x1d17a | 0xe0001 | 0xe0020..=0xe007f)
}

/// Controls, every format character (bidi controls, zero-width, tag characters), line and paragraph separators, and the
/// blank fillers. None of them belongs in an address that is shown as its own text (the web's `HOSTILE`).
fn hostile(c: char) -> bool {
    c.is_control()
        || format_char(c)
        || matches!(
            c as u32,
            0x2028 | 0x2029 | 0x34f | 0x115f | 0x1160 | 0x180e | 0x2800 | 0x3164 | 0xffa0 | 0xe0000
                ..=0xe007f
        )
}

/// Bidi controls: removed from the text around a link so they cannot reorder the link's characters.
fn bidi(c: char) -> bool {
    matches!(c as u32, 0x61c | 0x200e | 0x200f | 0x202a..=0x202e | 0x2066..=0x2069)
}

fn unbidi(s: &str) -> String {
    s.chars().filter(|c| !bidi(*c)).collect()
}

fn raw(item: &Item) -> String {
    match item {
        Item::C(c) => c.to_string(),
        Item::M(m) => format!("<@{m}>"),
    }
}

fn raw_of(items: &[Item]) -> String {
    items.iter().map(raw).collect()
}

fn is_space(item: Option<&Item>) -> bool {
    match item {
        None => true,
        Some(Item::C(c)) => js_space(*c),
        Some(Item::M(_)) => false,
    }
}

fn is_word(item: Option<&Item>) -> bool {
    item.and_then(Item::ch).is_some_and(word_char)
}

/// `SCHEME`: `^(?:https?:\/\/(?=[^/\s])|mailto:)`, case-insensitive, on `head`.
fn scheme(head: &str) -> bool {
    let lower = head.to_ascii_lowercase();
    if lower.starts_with("mailto:") {
        return true;
    }
    for p in ["https://", "http://"] {
        if let Some(rest) = lower.strip_prefix(p) {
            return rest
                .chars()
                .next()
                .is_some_and(|c| c != '/' && !js_space(c));
        }
    }
    false
}

/// `MAILTO`: the bare address of `mailto:address`.
fn mailto(text: &str) -> Option<String> {
    let addr = text
        .get(..7)
        .filter(|p| p.eq_ignore_ascii_case("mailto:"))?;
    let rest = &text[addr.len()..];
    let (local, domain) = rest.split_once('@')?;
    let local_ok = !local.is_empty()
        && local
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || "._%+-".contains(c));
    let dom: Vec<char> = domain.chars().collect();
    let domain_ok = !dom.is_empty()
        && dom.first().is_some_and(char::is_ascii_alphanumeric)
        && dom.last().is_some_and(char::is_ascii_alphanumeric)
        && dom
            .iter()
            .all(|c| c.is_ascii_alphanumeric() || *c == '.' || *c == '-');
    (local_ok && domain_ok).then(|| format!("mailto:{rest}"))
}

/// A link target we accept, as the address it opens: http or https with a real host and no credentials, or a bare
/// `mailto:address` (no query, so no `attach`, `body` or `cc`). Anything else is `None`. A non-ASCII host comes back as
/// punycode.
pub fn safe_link(text: &str) -> Option<String> {
    let head: String = text.chars().take(9).collect();
    if !scheme(&head) || text.chars().any(hostile) {
        return None;
    }
    if text.len() >= 7 && text[..7].eq_ignore_ascii_case("mailto:") {
        return mailto(text);
    }
    let url = url::Url::parse(text).ok()?;
    if url.scheme() != "http" && url.scheme() != "https" {
        return None;
    }
    if url.host_str().is_none_or(str::is_empty)
        || !url.username().is_empty()
        || url.password().is_some_and(|p| !p.is_empty())
    {
        return None;
    }
    Some(url.as_str().to_owned())
}

#[derive(Default)]
struct Out {
    out: Vec<Tree>,
    buf: String,
}

impl Out {
    fn text(&mut self, s: &str) {
        self.buf.push_str(s);
    }
    fn push(&mut self, part: Tree) {
        self.flush();
        self.out.push(part);
    }
    fn flush(&mut self) {
        if !self.buf.is_empty() {
            self.out.push(Tree::Text(std::mem::take(&mut self.buf)));
        }
    }
    fn parts(mut self) -> Vec<Tree> {
        self.flush();
        self.out
    }
}

/// Per-call lookups, built in one pass, that replace the forward scans.
struct Index {
    /// Length of the run of equal characters that starts at i.
    run: Vec<usize>,
    /// Inside a code span, or escaped by a backslash.
    skip: Vec<bool>,
    /// The next end of a word (a space, `<`, `>` or a mention) at or after i.
    stop: Vec<usize>,
    ticks: HashMap<usize, Vec<usize>>,
    closers: HashMap<(char, usize), Vec<usize>>,
    budget: i64,
}

/// The first element of a sorted list that is >= `at`.
fn first_from(list: Option<&Vec<usize>>, at: usize) -> Option<usize> {
    let list = list?;
    let i = list.partition_point(|&v| v < at);
    list.get(i).copied()
}

fn same(a: &Item, b: &Item) -> bool {
    matches!((a, b), (Item::C(x), Item::C(y)) if x == y)
}

fn build_index(items: &[Item]) -> Index {
    let n = items.len();
    let mut run = vec![0usize; n + 1];
    let mut stop = vec![0usize; n + 1];
    stop[n] = n;
    for i in (0..n).rev() {
        let it = &items[i];
        run[i] = if i + 1 < n && same(&items[i + 1], it) {
            run[i + 1] + 1
        } else {
            1
        };
        stop[i] = match it {
            Item::M(_) => i,
            Item::C(c) if js_space(*c) || *c == '<' || *c == '>' => i,
            Item::C(_) => stop[i + 1],
        };
    }
    let mut ticks: HashMap<usize, Vec<usize>> = HashMap::new();
    for i in 0..n {
        if !items[i].is('`') || (i > 0 && items[i - 1].is('`')) {
            continue;
        }
        ticks.entry(run[i]).or_default().push(i);
    }
    let mut skip = vec![false; n];
    let mut i = 0;
    while i < n {
        match &items[i] {
            Item::C('\\') => {
                if items.get(i + 1).and_then(Item::ch).is_some_and(escapable) {
                    skip[i] = true;
                    skip[i + 1] = true;
                    i += 1;
                }
            }
            Item::C('`') => {
                let len = run[i];
                match first_from(ticks.get(&len), i + len) {
                    None => {
                        i += len - 1;
                    }
                    Some(end) => {
                        for s in &mut skip[i..end + len] {
                            *s = true;
                        }
                        i = end + len - 1;
                    }
                }
            }
            _ => {}
        }
        i += 1;
    }
    let mut closers: HashMap<(char, usize), Vec<usize>> = HashMap::new();
    let mut i = 0;
    while i < n {
        let marker = items[i].ch().filter(|c| matches!(c, '*' | '~' | '_'));
        let Some(c) = marker.filter(|_| !skip[i]) else {
            i += 1;
            continue;
        };
        let len = run[i];
        if i > 0 && !is_space(items.get(i - 1)) && (c != '_' || !is_word(items.get(i + len))) {
            closers.entry((c, len)).or_default().push(i);
        }
        i += len;
    }
    Index {
        run,
        skip,
        stop,
        ticks,
        closers,
        budget: LINK_BUDGET,
    }
}

/// The run of `marker`, exactly `len` long, that closes an opener whose marker ends at `from`.
fn find_close(idx: &Index, marker: char, len: usize, from: usize) -> Option<usize> {
    first_from(idx.closers.get(&(marker, len)), from + 1)
}

fn link_at(items: &[Item], idx: &mut Index, i: usize) -> Option<(Tree, usize)> {
    if let Some(Item::C(before)) = items.get(i.wrapping_sub(1)).filter(|_| i > 0)
        && !js_space(*before)
        && !"([{\"'".contains(*before)
    {
        return None;
    }
    let mut head = String::new();
    for it in items.iter().skip(i).take(9) {
        head.push(it.ch()?);
    }
    if !scheme(&head) {
        return None;
    }
    let end = idx.stop[i];
    idx.budget -= (end - i) as i64;
    if idx.budget < 0 {
        return None;
    }
    let mut chars: Vec<char> = items[i..end].iter().filter_map(Item::ch).collect();
    let opens = chars.iter().filter(|c| **c == '(').count();
    let mut closes = chars.iter().filter(|c| **c == ')').count();
    while let Some(&last) = chars.last() {
        if last == ')' && closes > opens {
            closes -= 1;
            chars.pop();
        } else if ".,;:!?'\"*_~>]}".contains(last) {
            chars.pop();
        } else {
            break;
        }
    }
    let text: String = chars.iter().collect();
    let href = safe_link(&text)?;
    Some((
        Tree::Link {
            text: href.clone(),
            href,
        },
        i + chars.len(),
    ))
}

fn wrap(kind: fn(Vec<Tree>) -> Tree, inner: &[Item], depth: usize) -> Tree {
    kind(inline(inner, depth + 1))
}

fn inline(items: &[Item], depth: usize) -> Vec<Tree> {
    let mut out = Out::default();
    let mut idx = build_index(items);
    let n_items = items.len();
    let mut i = 0;
    while i < n_items {
        let it = &items[i];
        let Item::C(c) = it else {
            if let Item::M(m) = it {
                out.push(Tree::Ref(m.clone()));
            }
            i += 1;
            continue;
        };
        let c = *c;
        if c == '\\' {
            match items.get(i + 1).and_then(Item::ch) {
                Some(next) if escapable(next) => {
                    out.text(&next.to_string());
                    i += 1;
                }
                _ => out.text("\\"),
            }
            i += 1;
            continue;
        }
        if c == '`' {
            let n = idx.run[i];
            match first_from(idx.ticks.get(&n), i + n) {
                Some(end) if end > i + n => {
                    out.push(Tree::Code(raw_of(&items[i + n..end])));
                    i = end + n;
                }
                _ => {
                    out.text(&"`".repeat(n));
                    i += n;
                }
            }
            continue;
        }
        if matches!(c, 'h' | 'H' | 'm' | 'M')
            && let Some((part, end)) = link_at(items, &mut idx, i)
        {
            out.push(part);
            i = end;
            continue;
        }
        if depth < MAX_DEPTH && matches!(c, '*' | '~' | '_') {
            let n = idx.run[i];
            let mut done = false;
            let after = items.get(i + n);
            let before = i.checked_sub(1).and_then(|p| items.get(p));
            if !is_space(after)
                && !(c == '_' && (n != 1 || is_word(before)))
                && !(c == '~' && n != 2)
            {
                if c == '*'
                    && n == 3
                    && let Some(close) = find_close(&idx, '*', 3, i + 3)
                {
                    out.push(Tree::Strong(vec![wrap(
                        Tree::Em,
                        &items[i + 3..close],
                        depth,
                    )]));
                    i = close + 3;
                    done = true;
                }
                if !done {
                    let len = if c == '*' { n.min(2) } else { n };
                    if let Some(close) = find_close(&idx, c, len, i + len) {
                        let kind: fn(Vec<Tree>) -> Tree = if c == '~' {
                            Tree::Strike
                        } else if len == 2 {
                            Tree::Strong
                        } else {
                            Tree::Em
                        };
                        out.push(wrap(kind, &items[i + len..close], depth));
                        i = close + len;
                        done = true;
                    }
                }
            }
            if !done {
                out.text(&c.to_string().repeat(n));
                i += n;
            }
            continue;
        }
        out.text(&c.to_string());
        i += 1;
    }
    let _ = &idx.skip;
    out.parts()
}

fn split_lines(items: &[Item]) -> Vec<Vec<Item>> {
    let mut out: Vec<Vec<Item>> = vec![Vec::new()];
    for it in items {
        if it.is('\n') {
            out.push(Vec::new());
        } else if let Some(last) = out.last_mut() {
            last.push(it.clone());
        }
    }
    out
}

fn join_lines(ls: &[Vec<Item>]) -> Vec<Item> {
    let mut out = Vec::new();
    for (n, l) in ls.iter().enumerate() {
        if n > 0 {
            out.push(Item::C('\n'));
        }
        out.extend(l.iter().cloned());
    }
    out
}

fn is_quote_text(t: &str) -> bool {
    t == ">" || t.starts_with("> ")
}

/// `FENCE`: three backticks and at most 32 of `[A-Za-z0-9+#._-]`, nothing else. The language, `None` when no match.
fn fence(line: &str) -> Option<&str> {
    let lang = line.strip_prefix("```")?;
    (lang.chars().count() <= 32
        && lang
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || "+#._-".contains(c)))
    .then_some(lang)
}

/// Parses a body (raw text with literal mention tokens).
pub fn parse(body: &str) -> Parsed {
    let items: Vec<Item> = split_mentions(body)
        .into_iter()
        .flat_map(|part| match part {
            Part::Text(t) => t.chars().map(Item::C).collect::<Vec<_>>(),
            Part::Mention(m) => vec![Item::M(m)],
        })
        .collect();
    let ls = split_lines(&items);
    let lt: Vec<String> = ls.iter().map(|l| js_trim(&raw_of(l)).to_owned()).collect();
    // The first line at or after k that is exactly a fence, found once for every opener.
    let mut next_fence: Vec<Option<usize>> = vec![None; ls.len() + 1];
    for k in (0..ls.len()).rev() {
        next_fence[k] = if lt[k] == "```" {
            Some(k)
        } else {
            next_fence[k + 1]
        };
    }
    let mut out: Vec<Tree> = Vec::new();
    let mut para: Vec<Vec<Item>> = Vec::new();
    fn flush(out: &mut Vec<Tree>, para: &mut Vec<Vec<Item>>) {
        if !para.is_empty() {
            out.extend(inline(&join_lines(para), 0));
        }
        para.clear();
    }
    // A block takes the line break that ends the text before it.
    fn block(out: &mut Vec<Tree>, para: &mut Vec<Vec<Item>>, part: Tree) {
        flush(out, para);
        if let Some(Tree::Text(t)) = out.last_mut()
            && t.ends_with('\n')
        {
            t.pop();
        }
        out.push(part);
    }
    let mut n = 0;
    while n < ls.len() {
        if let Some(lang) = fence(&lt[n])
            && let Some(close) = next_fence[n + 1]
        {
            let text = ls[n + 1..close]
                .iter()
                .map(|l| raw_of(l))
                .collect::<Vec<_>>()
                .join("\n");
            block(
                &mut out,
                &mut para,
                Tree::CodeBlock {
                    text,
                    lang: (!lang.is_empty()).then(|| lang.to_owned()),
                },
            );
            n = close + 1;
            continue;
        }
        if is_quote_text(&lt[n]) {
            let mut quoted: Vec<Vec<Item>> = Vec::new();
            while n < ls.len() && is_quote_text(&lt[n]) {
                let at = ls[n].iter().position(|i| i.is('>')).unwrap_or(0);
                let skip = if ls[n].get(at + 1).is_some_and(|i| i.is(' ')) {
                    2
                } else {
                    1
                };
                quoted.push(ls[n][(at + skip).min(ls[n].len())..].to_vec());
                n += 1;
            }
            block(
                &mut out,
                &mut para,
                Tree::Quote(inline(&join_lines(&quoted), 1)),
            );
            continue;
        }
        para.push(ls[n].clone());
        n += 1;
    }
    flush(&mut out, &mut para);
    let links = has_link(&out);
    Parsed {
        tree: if links {
            out.into_iter().map(strip).collect()
        } else {
            out
        },
        links,
    }
}

fn has_link(tree: &[Tree]) -> bool {
    tree.iter().any(|p| match p {
        Tree::Link { .. } => true,
        Tree::Strong(c) | Tree::Em(c) | Tree::Strike(c) | Tree::Quote(c) => has_link(c),
        _ => false,
    })
}

fn strip(p: Tree) -> Tree {
    match p {
        Tree::Text(t) => Tree::Text(unbidi(&t)),
        Tree::Code(t) => Tree::Code(unbidi(&t)),
        Tree::CodeBlock { text, lang } => Tree::CodeBlock {
            text: unbidi(&text),
            lang,
        },
        Tree::Strong(c) => Tree::Strong(c.into_iter().map(strip).collect()),
        Tree::Em(c) => Tree::Em(c.into_iter().map(strip).collect()),
        Tree::Strike(c) => Tree::Strike(c.into_iter().map(strip).collect()),
        Tree::Quote(c) => Tree::Quote(c.into_iter().map(strip).collect()),
        other => other,
    }
}

static TREES: OnceLock<Shared<Parsed>> = OnceLock::new();

fn trees() -> &'static Shared<Parsed> {
    TREES.get_or_init(|| Shared::new(Lru::web()))
}

/// [`parse`] remembered by the body text (least recently used goes first, within the web's limits), so a re-draw of an
/// unchanged message, or of a hostile one, does not parse again (markdown.ts `parseTree`).
pub fn parse_cached(body: &str) -> Arc<Parsed> {
    trees().memo(body, || parse(body))
}

/// What the parse memo holds: (entries, characters of keys).
pub fn cache_size() -> (usize, usize) {
    (trees().len(), trees().used_chars())
}

/// A body part as the row draws it: the tree with each mention resolved to its pill.
#[derive(Clone, Debug, PartialEq)]
pub enum Body {
    Text(String),
    Mention(Pill),
    Strong(Vec<Body>),
    Em(Vec<Body>),
    Strike(Vec<Body>),
    Quote(Vec<Body>),
    Code(String),
    CodeBlock { text: String, lang: Option<String> },
    Link { href: String },
}

/// A mention pill: `me` is the viewer, `broadcast` is @everyone and @here.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Pill {
    pub label: String,
    pub me: bool,
    pub broadcast: bool,
    /// A user mention's id (the pill opens that person's card).
    pub user: Option<String>,
}

/// The tree with each mention token turned into the pill `resolve` names (the web's `parseMarkdown`).
pub fn parse_markdown(body: &str, resolve: &dyn Fn(&str) -> Pill) -> Vec<Body> {
    let parsed = parse_cached(body);
    resolve_tree(&parsed.tree, resolve, parsed.links)
}

fn resolve_tree(tree: &[Tree], resolve: &dyn Fn(&str) -> Pill, links: bool) -> Vec<Body> {
    tree.iter()
        .map(|p| match p {
            Tree::Text(t) => Body::Text(t.clone()),
            Tree::Ref(target) => {
                let mut pill = resolve(target);
                // A label is one line: a name with a line break in it must not break the row's text.
                pill.label = pill.label.replace(['\n', '\r'], " ");
                if links {
                    pill.label = unbidi(&pill.label);
                }
                Body::Mention(pill)
            }
            Tree::Strong(c) => Body::Strong(resolve_tree(c, resolve, links)),
            Tree::Em(c) => Body::Em(resolve_tree(c, resolve, links)),
            Tree::Strike(c) => Body::Strike(resolve_tree(c, resolve, links)),
            Tree::Quote(c) => Body::Quote(resolve_tree(c, resolve, links)),
            Tree::Code(t) => Body::Code(t.clone()),
            Tree::CodeBlock { text, lang } => Body::CodeBlock {
                text: text.clone(),
                lang: lang.clone(),
            },
            Tree::Link { href, .. } => Body::Link { href: href.clone() },
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    const ID: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";

    fn t(s: &str) -> Tree {
        Tree::Text(s.to_owned())
    }
    fn tree(s: &str) -> Vec<Tree> {
        parse(s).tree
    }
    fn link(href: &str) -> Tree {
        Tree::Link {
            href: href.to_owned(),
            text: href.to_owned(),
        }
    }
    fn code(s: &str) -> Tree {
        Tree::Code(s.to_owned())
    }

    /// markdown.test.ts, the table of `parseMarkdown`.
    #[test]
    fn the_grammar_table() {
        let cases: Vec<(&str, &str, Vec<Tree>)> = vec![
            (
                "plain text is one text part",
                "hello there",
                vec![t("hello there")],
            ),
            ("strong", "**bold**", vec![Tree::Strong(vec![t("bold")])]),
            ("em with stars", "*it*", vec![Tree::Em(vec![t("it")])]),
            ("em with underscores", "_it_", vec![Tree::Em(vec![t("it")])]),
            ("strike", "~~gone~~", vec![Tree::Strike(vec![t("gone")])]),
            (
                "code span",
                "a `b *c*` d",
                vec![t("a "), code("b *c*"), t(" d")],
            ),
            (
                "double-tick code span holds a tick",
                "``a`b``",
                vec![code("a`b")],
            ),
            (
                "nesting",
                "**a *b* c**",
                vec![Tree::Strong(vec![t("a "), Tree::Em(vec![t("b")]), t(" c")])],
            ),
            (
                "strong and em together",
                "***x***",
                vec![Tree::Strong(vec![Tree::Em(vec![t("x")])])],
            ),
            (
                "strike inside strong",
                "**~~x~~**",
                vec![Tree::Strong(vec![Tree::Strike(vec![t("x")])])],
            ),
            ("unclosed strong stays literal", "**open", vec![t("**open")]),
            ("unclosed em stays literal", "a *b", vec![t("a *b")]),
            ("unclosed code stays literal", "a `b", vec![t("a `b")]),
            (
                "a lone star between spaces is text",
                "2 * 3 * 4",
                vec![t("2 * 3 * 4")],
            ),
            (
                "snake_case is not emphasis",
                "snake_case_name",
                vec![t("snake_case_name")],
            ),
            ("empty emphasis is text", "****", vec![t("****")]),
            (
                "a backslash escapes a marker",
                "\\*not\\*",
                vec![t("*not*")],
            ),
            (
                "a backslash escapes a backslash",
                "\\\\*x*",
                vec![t("\\"), Tree::Em(vec![t("x")])],
            ),
            (
                "a backslash before a letter stays",
                "C:\\dir",
                vec![t("C:\\dir")],
            ),
            ("a trailing backslash stays", "end\\", vec![t("end\\")]),
            (
                "fenced block",
                "```\nline *1*\nline 2\n```",
                vec![Tree::CodeBlock {
                    text: "line *1*\nline 2".into(),
                    lang: None,
                }],
            ),
            (
                "fenced block with a language tag",
                "```ts\nlet a;\n```",
                vec![Tree::CodeBlock {
                    text: "let a;".into(),
                    lang: Some("ts".into()),
                }],
            ),
            (
                "F4: an opening line that says more than a language is literal",
                "```hidden text a moderator never sees\nshown\n```",
                vec![code("hidden text a moderator never sees\nshown\n")],
            ),
            (
                "F4: text after an unclosed fence stays visible",
                "a\n```\nsecret\nmore",
                vec![t("a\n```\nsecret\nmore")],
            ),
            (
                "an unclosed fence is text",
                "```\nopen",
                vec![t("```\nopen")],
            ),
            (
                "text around a fence",
                "before\n```\nx\n```\nafter",
                vec![
                    t("before"),
                    Tree::CodeBlock {
                        text: "x".into(),
                        lang: None,
                    },
                    t("after"),
                ],
            ),
            (
                "quote",
                "> hi *you*",
                vec![Tree::Quote(vec![t("hi "), Tree::Em(vec![t("you")])])],
            ),
            (
                "quote of two lines",
                "> a\n> b",
                vec![Tree::Quote(vec![t("a\nb")])],
            ),
            (
                "quote then text",
                "> a\nb",
                vec![Tree::Quote(vec![t("a")]), t("b")],
            ),
            ("a > mid-line is text", "a > b", vec![t("a > b")]),
            (
                "markdown link syntax is not a link, the url inside is",
                "[click](https://evil.example)",
                vec![t("[click]("), link("https://evil.example/"), t(")")],
            ),
            (
                "raw html stays text",
                "<b>x</b><img src=x onerror=alert(1)>",
                vec![t("<b>x</b><img src=x onerror=alert(1)>")],
            ),
        ];
        for (name, input, expected) in cases {
            assert_eq!(tree(input), expected, "{name}: {input:?}");
        }
    }

    #[test]
    fn links() {
        assert_eq!(
            tree("see https://example.com/a?b=1#c now"),
            vec![t("see "), link("https://example.com/a?b=1#c"), t(" now")]
        );
        assert_eq!(
            tree("http://example.com"),
            vec![link("http://example.com/")]
        );
        assert_eq!(
            tree("mailto:a@example.com"),
            vec![link("mailto:a@example.com")]
        );
        // Trailing punctuation stays out of the link; a balanced parenthesis stays in.
        assert_eq!(
            tree("(https://example.com/x)."),
            vec![t("("), link("https://example.com/x"), t(").")]
        );
        assert_eq!(
            tree("https://example.com/a_(b)"),
            vec![link("https://example.com/a_(b)")]
        );
        assert_eq!(
            tree("**https://example.com**"),
            vec![Tree::Strong(vec![link("https://example.com/")])]
        );
        // No link inside code.
        assert_eq!(
            tree("`https://example.com`"),
            vec![code("https://example.com")]
        );
        assert_eq!(
            tree("```\nhttps://example.com\n```"),
            vec![Tree::CodeBlock {
                text: "https://example.com".into(),
                lang: None
            }]
        );
        assert!(
            !tree("xhttps://example.com")
                .iter()
                .any(|p| matches!(p, Tree::Link { .. }))
        );
    }

    #[test]
    fn refuses_unsafe_addresses() {
        for input in [
            "javascript:alert(1)",
            "JaVaScRiPt:alert(1)",
            "data:text/html,<script>alert(1)</script>",
            "vbscript:x",
            "file:///etc/passwd",
            "blob:https://example.com/x",
            "ftp://example.com",
            "tel:+4631",
            "//example.com",
            "https://user:pw@example.com",
            "https://user@example.com",
            "https://",
            "https:///x",
            "http://exa\u{202e}mple.com",
            "https://example.com/\u{200b}x",
            "mailto:",
            "mailto:a@b.example?attach=/etc/passwd",
            "mailto:a@b.example?body=x",
            "mailto:a@b.example,c@d.example",
            "mailto:?to=a@b.example",
            "mailto:nobody",
            "mailto:a@b.example/x",
        ] {
            assert_eq!(safe_link(input), None, "{input:?}");
            assert!(
                !tree(input).iter().any(|p| matches!(p, Tree::Link { .. })),
                "{input:?} parsed as a link"
            );
        }
    }

    #[test]
    fn invisible_characters_refuse_a_link() {
        for ch in [
            '\u{61c}',
            '\u{115f}',
            '\u{1160}',
            '\u{3164}',
            '\u{ffa0}',
            '\u{e0041}',
            '\u{e0000}',
            '\u{34f}',
            '\u{180e}',
            '\u{2800}',
            '\u{200d}',
            '\u{ad}',
            '\u{fff9}',
            '\u{2060}',
            '\u{202e}',
        ] {
            for url in [
                format!("https://good.example/{ch}x"),
                format!("https://good.example/x{ch}"),
                format!("https://go{ch}od.example/"),
            ] {
                assert_eq!(safe_link(&url), None, "{url:?}");
            }
        }
    }

    #[test]
    fn the_host_is_shown_as_ascii() {
        assert_eq!(
            tree("https://\u{430}pple.com/login"),
            vec![link("https://xn--pple-43d.com/login")]
        );
        assert_eq!(
            tree("https://g\u{f6}\u{f6}gle.com/"),
            vec![link("https://xn--ggle-5qaa.com/")]
        );
        assert_eq!(
            tree("https://example.com/\u{e5}"),
            vec![link("https://example.com/%C3%A5")]
        );
        assert_eq!(
            tree("mail mailto:Ada.L+x@example.com now"),
            vec![t("mail "), link("mailto:Ada.L+x@example.com"), t(" now")]
        );
    }

    #[test]
    fn mentions_are_pills_and_never_glue() {
        let r = |x: &str| Tree::Ref(x.to_owned());
        assert_eq!(tree(&format!("hi <@{ID}>")), vec![t("hi "), r(ID)]);
        assert_eq!(
            tree(&format!("**hi <@{ID}>**")),
            vec![Tree::Strong(vec![t("hi "), r(ID)])]
        );
        assert_eq!(
            tree("> <@everyone> *x*"),
            vec![Tree::Quote(vec![
                r("everyone"),
                t(" "),
                Tree::Em(vec![t("x")])
            ])]
        );
        assert_eq!(
            tree(&format!("*a <@{ID}> b*")),
            vec![Tree::Em(vec![t("a "), r(ID), t(" b")])]
        );
        // Code keeps the literal token.
        assert_eq!(tree(&format!("`<@{ID}>`")), vec![code(&format!("<@{ID}>"))]);
        assert_eq!(
            tree(&format!("```\n<@{ID}>\n```")),
            vec![Tree::CodeBlock {
                text: format!("<@{ID}>"),
                lang: None
            }]
        );
        // A mention is not a word, so it cannot glue a url.
        assert_eq!(
            tree(&format!("<@{ID}>https://example.com"))
                .iter()
                .filter(|p| matches!(p, Tree::Link { .. }))
                .count(),
            1
        );
    }

    #[test]
    fn bidi_controls_cannot_reorder_a_link() {
        let body = "\u{202e} https://evil.example/?moc.elgoog//:sptth";
        assert_eq!(
            tree(body),
            vec![t(" "), link("https://evil.example/?moc.elgoog//:sptth")]
        );
        // A body with no link keeps them.
        assert_eq!(
            tree("abc \u{200f} def \u{61c}"),
            vec![t("abc \u{200f} def \u{61c}")]
        );
        // Pill labels lose them in a body that holds a link.
        let parts = parse_markdown(&format!("<@{ID}> https://example.com"), &|_| Pill {
            label: "\u{202e}@bob".into(),
            me: false,
            broadcast: false,
            user: None,
        });
        assert!(matches!(&parts[0], Body::Mention(p) if p.label == "@bob"));
    }

    fn best_of(runs: usize, f: impl Fn()) -> std::time::Duration {
        f();
        (0..runs)
            .map(|_| {
                let t = std::time::Instant::now();
                f();
                t.elapsed()
            })
            .min()
            .expect("a run")
    }

    #[test]
    fn survives_deep_nesting_and_long_runs_of_markers() {
        for s in [
            "*".repeat(5000),
            "*a ".repeat(2000),
            format!("{}{}", "**a ".repeat(1000), "**".repeat(1000)),
            format!(
                "{}{}{}",
                "`".repeat(3000),
                "~".repeat(3000),
                ">".repeat(3000)
            ),
        ] {
            // Not just survived: each is parsed in a bounded time (a debug build, a loaded machine).
            let took = best_of(3, || drop(parse(&s)));
            assert!(
                took < std::time::Duration::from_secs(2),
                "{took:?} for {}",
                s.len()
            );
        }
    }

    /// markdown.test.ts F2: the parse is linear. One 32k parse costs less than 3x eight 4k parses (linear is about 1,
    /// quadratic 8), and 4000 characters parse in bounded time whatever they are. The absolute bounds are the web's 25 ms
    /// and 250 ms, times 10 for a debug build on a shared machine.
    #[test]
    #[allow(clippy::type_complexity)]
    fn the_parse_is_linear_on_hostile_input() {
        let cases: Vec<(&str, Box<dyn Fn(usize) -> String>)> = vec![
            (
                "a url then ) repeated",
                Box::new(|n| format!("http://a/{}", ")".repeat(n - 9))),
            ),
            ("(h repeated", Box::new(|n| "(h".repeat(n / 2))),
            (
                "a url then dots",
                Box::new(|n| format!("http://a{}", ".".repeat(n - 8))),
            ),
            ("unclosed fences", Box::new(|n| "```a\n".repeat(n / 5))),
            ("unclosed stars", Box::new(|n| "*a ".repeat(n / 3))),
            (
                "unclosed tildes and underscores",
                Box::new(|n| "~~a_a ".repeat(n / 6)),
            ),
            (
                "backtick runs of growing length",
                Box::new(|n| {
                    let mut s = String::new();
                    let mut k = 1;
                    while s.len() < n {
                        s.push_str(&"`".repeat(k));
                        s.push('a');
                        k += 1;
                    }
                    s.truncate(n);
                    s
                }),
            ),
            ("quotes", Box::new(|n| "> *a\n".repeat(n / 5))),
            ("links in brackets", Box::new(|n| "(http://a".repeat(n / 9))),
            (
                "many short links",
                Box::new(|n| "http://a.b/c ".repeat(n / 13)),
            ),
            (
                "links with parentheses",
                Box::new(|n| "https://a.b/(x)) ".repeat(n / 17)),
            ),
            (
                "links in emphasis",
                Box::new(|n| "*http://a.b/c* ".repeat(n / 15)),
            ),
        ];
        let ms = std::time::Duration::from_millis;
        for (name, make) in &cases {
            let (small, large) = (make(4000), make(32000));
            assert!(
                best_of(5, || drop(parse(&small))) < ms(250),
                "{name}: 4000 characters"
            );
            assert!(
                best_of(3, || drop(parse(&make(50_000)))) < ms(2500),
                "{name}: 50000 characters"
            );
            // Eight 4k parses against one 32k, in turns, the best of five of each.
            let mut eight = std::time::Duration::MAX;
            let mut one = std::time::Duration::MAX;
            for _ in 0..5 {
                let t = std::time::Instant::now();
                for _ in 0..8 {
                    drop(parse(&small));
                }
                eight = eight.min(t.elapsed());
                let t = std::time::Instant::now();
                drop(parse(&large));
                one = one.min(t.elapsed());
            }
            let growth = one.as_secs_f64() / eight.as_secs_f64().max(0.000_1);
            assert!(
                growth < 3.0,
                "{name}: 8x the input cost {growth:.1}x (eight 4k: {eight:?}, one 32k: {one:?})"
            );
        }
    }

    /// markdown.test.ts F2, last case: the memo hands back the same parse of a body, and a fresh parse of it is another.
    #[test]
    fn remembers_the_parse_of_a_body() {
        let body = format!("**x** https://example.com {}", ID);
        assert!(Arc::ptr_eq(&parse_cached(&body), &parse_cached(&body)));
        assert_eq!(parse_cached(&body).tree, parse(&body).tree);
    }

    /// N1: the memo is bounded: 200 bodies, 2 MB of body text, and a body over 4000 characters is not kept.
    #[test]
    fn the_parse_memo_is_bounded() {
        for k in 0..700 {
            let body = format!("{k} {} ", "y".repeat(3900));
            parse_cached(&body);
            let (entries, chars) = cache_size();
            assert!(entries <= super::super::cache::ENTRIES);
            assert!(chars <= super::super::cache::CHARS);
        }
        let long = format!("{}{}", "a".repeat(super::super::cache::BODY + 1), ID);
        assert!(!Arc::ptr_eq(&parse_cached(&long), &parse_cached(&long)));
        assert_eq!(parse_cached(&long).tree, parse(&long).tree);
    }

    /// markdown.test.ts: every link that comes out is canonical, safe and shown as its own href (the web's alphabet,
    /// with the bidi controls and the lookalike letter).
    #[test]
    fn fuzz_every_link_is_canonical_safe_and_its_own_text() {
        fn walk(parts: &[Tree]) {
            for p in parts {
                match p {
                    Tree::Link { href, text } => {
                        assert_eq!(safe_link(href).as_deref(), Some(href.as_str()));
                        assert_eq!(text, href);
                    }
                    Tree::Strong(c) | Tree::Em(c) | Tree::Strike(c) | Tree::Quote(c) => walk(c),
                    _ => {}
                }
            }
        }
        let alphabet: Vec<&str> = vec![
            "a", " ", "*", "_", "`", "\n", "(", ")", "http", "https://", "mailto:", "x@y.z", "a.b",
            "/", ":", ".", "\u{202e}", "\u{61c}", "\u{3164}", "\u{430}", "?", "#",
        ];
        let mut seed: u64 = 3;
        let mut rand = |n: usize| {
            seed = seed.wrapping_mul(1_103_515_245).wrapping_add(12345) & 0x7fff_ffff;
            ((seed >> 8) as usize) % n
        };
        for _ in 0..3000 {
            let len = rand(60);
            let input: String = (0..len).map(|_| alphabet[rand(alphabet.len())]).collect();
            walk(&parse(&input).tree);
        }
    }

    /// The parser only removes markers: what a reader sees is the input with some of `\ * _ ~ ` > `, line breaks, spaces
    /// and a fence's lines taken out, in the same order, whatever the input is.
    #[test]
    fn fuzz_the_visible_text_is_the_input_minus_markers() {
        fn visible(parts: &[Tree]) -> String {
            parts
                .iter()
                .map(|p| match p {
                    Tree::Text(t) | Tree::Code(t) => t.clone(),
                    Tree::Link { text, .. } => text.clone(),
                    Tree::CodeBlock { text, lang } => {
                        format!("{}{text}", lang.clone().unwrap_or_default())
                    }
                    Tree::Ref(r) => format!("<@{r}>"),
                    Tree::Strong(c) | Tree::Em(c) | Tree::Strike(c) | Tree::Quote(c) => visible(c),
                })
                .collect()
        }
        let marker = |c: char| "\\*_~`>\n ".contains(c);
        let mention = format!("<@{ID}>");
        let alphabet: Vec<&str> = vec![
            "a", "x", " ", "*", "_", "~~", "`", "```", "\n```\n", "```x\n", "\n", "\\", "> ", "[",
            "]", "(", ")", &mention, "**", "a b",
        ];
        let mut seed: u64 = 11;
        let mut rand = |n: usize| {
            seed = (seed.wrapping_mul(1_103_515_245).wrapping_add(12345)) & 0x7fff_ffff;
            ((seed >> 8) as usize) % n
        };
        for _ in 0..1500 {
            let len = rand(200);
            let input: String = (0..len).map(|_| alphabet[rand(alphabet.len())]).collect();
            let seen = visible(&parse(&input).tree);
            let chars: Vec<char> = input.chars().collect();
            let mut at = 0usize;
            let mut bad = "";
            'outer: for ch in seen.chars() {
                let mut found = None;
                for (k, c) in chars.iter().enumerate().skip(at) {
                    if *c == ch {
                        found = Some(k);
                        break;
                    }
                }
                let Some(k) = found else {
                    bad = "not in order";
                    break 'outer;
                };
                if chars[at..k].iter().any(|d| !marker(*d)) {
                    bad = "dropped a non-marker";
                    break 'outer;
                }
                at = k + 1;
            }
            if bad.is_empty() && chars[at..].iter().any(|d| !marker(*d)) {
                bad = "dropped a non-marker at the end";
            }
            assert_eq!(bad, "", "{input:?} -> {seen:?}");
        }
    }

    #[test]
    fn text_without_a_marker_is_exactly_itself() {
        for input in [
            "plain",
            "a b  c",
            "emoji 🎉 ok",
            "tabs\tand\nlines",
            "a.b/c:d@e <not a mention>",
        ] {
            assert_eq!(tree(input), vec![t(input)], "{input:?}");
        }
        assert_eq!(tree(""), Vec::<Tree>::new());
    }
}
