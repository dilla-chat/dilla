//! A tiny line tokenizer for code blocks (port of `packages/ui/src/CodeBlock/highlight.ts`, README §6.4, ported rule for
//! rule from the Signal prototype's `hl()`): yaml, json, js/ts and bash. Each line is cut into tokens by trying every
//! rule and taking the earliest match (the first rule wins a tie). The rules are the web's regular expressions written by
//! hand (the lookaheads and the one lookbehind included), so the crate takes no regex dependency.

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TokenKind {
    Key,
    Str,
    Num,
    Com,
    Bool,
    Kw,
    Punct,
    Text,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Token {
    pub kind: TokenKind,
    pub text: String,
}

/// The leftmost match of a rule in the rest of the line, as a character range.
type Rule = fn(&[char]) -> Option<(usize, usize)>;

fn word(c: char) -> bool {
    c.is_ascii_alphanumeric() || c == '_'
}

fn js_space(c: char) -> bool {
    (c.is_whitespace() && c != '\u{85}') || c == '\u{feff}'
}

/// `\b` at `at`.
fn boundary(s: &[char], at: usize) -> bool {
    let before = at > 0 && word(s[at - 1]);
    let after = s.get(at).is_some_and(|c| word(*c));
    before != after
}

fn leftmost(s: &[char], at: impl Fn(usize) -> Option<usize>) -> Option<(usize, usize)> {
    (0..s.len()).find_map(|p| at(p).map(|e| (p, e)))
}

fn is_line_end(c: char) -> bool {
    matches!(c, '\n' | '\r' | '\u{2028}' | '\u{2029}')
}

/// `<marker>.*$`: from the first marker with no line terminator after it.
fn comment(s: &[char], marker: &[char]) -> Option<(usize, usize)> {
    leftmost(s, |p| {
        (s[p..].starts_with(marker) && !s[p..].iter().any(|c| is_line_end(*c))).then_some(s.len())
    })
}

fn hash_comment(s: &[char]) -> Option<(usize, usize)> {
    comment(s, &['#'])
}

fn slash_comment(s: &[char]) -> Option<(usize, usize)> {
    comment(s, &['/', '/'])
}

/// `q(?:[^q\\]|\\.)*q` at `p`: the end of the string, or `None`.
fn quoted_escaped(s: &[char], p: usize, q: char) -> Option<usize> {
    if s.get(p) != Some(&q) {
        return None;
    }
    let mut i = p + 1;
    while i < s.len() {
        match s[i] {
            c if c == q => return Some(i + 1),
            '\\' => {
                if s.get(i + 1).is_none_or(|c| is_line_end(*c)) {
                    return None;
                }
                i += 2;
            }
            _ => i += 1,
        }
    }
    None
}

/// `'[^']*'` at `p`.
fn single_plain(s: &[char], p: usize) -> Option<usize> {
    if s.get(p) != Some(&'\'') {
        return None;
    }
    s[p + 1..]
        .iter()
        .position(|c| *c == '\'')
        .map(|e| p + 1 + e + 1)
}

fn yaml_str(s: &[char]) -> Option<(usize, usize)> {
    leftmost(s, |p| {
        quoted_escaped(s, p, '"').or_else(|| single_plain(s, p))
    })
}

fn js_str(s: &[char]) -> Option<(usize, usize)> {
    leftmost(s, |p| {
        quoted_escaped(s, p, '"')
            .or_else(|| quoted_escaped(s, p, '\''))
            .or_else(|| quoted_escaped(s, p, '`'))
    })
}

fn json_str(s: &[char]) -> Option<(usize, usize)> {
    leftmost(s, |p| quoted_escaped(s, p, '"'))
}

/// `(?=\s*:)` at `at`.
fn colon_ahead(s: &[char], at: usize) -> bool {
    let mut i = at;
    while s.get(i).is_some_and(|c| js_space(*c)) {
        i += 1;
    }
    s.get(i) == Some(&':')
}

fn json_key(s: &[char]) -> Option<(usize, usize)> {
    leftmost(s, |p| {
        quoted_escaped(s, p, '"').filter(|e| colon_ahead(s, *e))
    })
}

/// `^\s*(?:-\s+)?[\w.-]+(?=\s*:)`.
fn yaml_key(s: &[char]) -> Option<(usize, usize)> {
    let mut i = 0;
    while s.get(i).is_some_and(|c| js_space(*c)) {
        i += 1;
    }
    let run = |from: usize| -> Option<usize> {
        let mut j = from;
        while s.get(j).is_some_and(|c| word(*c) || *c == '.' || *c == '-') {
            j += 1;
        }
        (j > from && colon_ahead(s, j)).then_some(j)
    };
    // With the list prefix first, as the regex tries it.
    if s.get(i) == Some(&'-') && s.get(i + 1).is_some_and(|c| js_space(*c)) {
        let mut j = i + 1;
        while s.get(j).is_some_and(|c| js_space(*c)) {
            j += 1;
        }
        if let Some(e) = run(j) {
            return Some((0, e));
        }
    }
    run(i).map(|e| (0, e))
}

/// `\b(a|b|...)\b` for the first word of the list that fits at `p`.
fn keyword_at(s: &[char], p: usize, words: &[&str]) -> Option<usize> {
    if !boundary(s, p) {
        return None;
    }
    words.iter().find_map(|w| {
        let end = p + w.chars().count();
        (end <= s.len() && s[p..end].iter().copied().eq(w.chars()) && boundary(s, end))
            .then_some(end)
    })
}

fn words_rule(s: &[char], words: &[&str]) -> Option<(usize, usize)> {
    leftmost(s, |p| keyword_at(s, p, words))
}

fn yaml_bool(s: &[char]) -> Option<(usize, usize)> {
    words_rule(s, &["true", "false", "null", "yes", "no"])
}

fn json_bool(s: &[char]) -> Option<(usize, usize)> {
    words_rule(s, &["true", "false", "null"])
}

fn js_bool(s: &[char]) -> Option<(usize, usize)> {
    words_rule(s, &["true", "false", "null", "undefined", "this"])
}

fn js_kw(s: &[char]) -> Option<(usize, usize)> {
    words_rule(
        s,
        &[
            "const", "let", "var", "function", "return", "if", "else", "for", "while", "import",
            "from", "export", "default", "await", "async", "new", "class", "extends", "try",
            "catch", "throw", "of", "in",
        ],
    )
}

/// `\d+(\.\d+)?` from `q` with the closing `\b` (and an exponent when asked): the end.
fn number_at(s: &[char], q: usize, exponent: bool) -> Option<usize> {
    if !boundary(s, q) {
        return None;
    }
    let digits = |from: usize| {
        let mut j = from;
        while s.get(j).is_some_and(char::is_ascii_digit) {
            j += 1;
        }
        j
    };
    let e = digits(q);
    if e == q {
        return None;
    }
    // Longest first: the fraction, then the exponent, each given up when the closing \b fails.
    let mut ends = vec![e];
    if s.get(e) == Some(&'.') {
        let f = digits(e + 1);
        if f > e + 1 {
            ends.insert(0, f);
        }
    }
    let mut candidates = Vec::new();
    for base in ends {
        if exponent && s.get(base).is_some_and(|c| *c == 'e' || *c == 'E') {
            let mut k = base + 1;
            if s.get(k).is_some_and(|c| *c == '+' || *c == '-') {
                k += 1;
            }
            let x = digits(k);
            if x > k {
                candidates.push(x);
            }
        }
        candidates.push(base);
    }
    candidates.into_iter().find(|end| boundary(s, *end))
}

fn number(s: &[char], negative: bool, exponent: bool) -> Option<(usize, usize)> {
    leftmost(s, |p| {
        if negative && s[p] == '-' {
            number_at(s, p + 1, exponent).or_else(|| number_at(s, p, exponent))
        } else {
            number_at(s, p, exponent)
        }
    })
}

fn yaml_num(s: &[char]) -> Option<(usize, usize)> {
    number(s, true, false)
}

fn json_num(s: &[char]) -> Option<(usize, usize)> {
    number(s, true, true)
}

fn js_num(s: &[char]) -> Option<(usize, usize)> {
    number(s, false, false)
}

fn set(s: &[char], chars: &str) -> Option<(usize, usize)> {
    s.iter()
        .position(|c| chars.contains(*c))
        .map(|p| (p, p + 1))
}

fn yaml_punct(s: &[char]) -> Option<(usize, usize)> {
    set(s, ":-[]{},|>")
}

fn json_punct(s: &[char]) -> Option<(usize, usize)> {
    set(s, "{}[],:")
}

fn js_punct(s: &[char]) -> Option<(usize, usize)> {
    set(s, "{}()[];,.=<>+-*/!&|?:")
}

fn bash_punct(s: &[char]) -> Option<(usize, usize)> {
    set(s, "|&;<>")
}

/// `[A-Za-z_$][\w$]*(?=\()`.
fn js_key(s: &[char]) -> Option<(usize, usize)> {
    leftmost(s, |p| {
        if !(s[p].is_ascii_alphabetic() || s[p] == '_' || s[p] == '$') {
            return None;
        }
        let mut j = p + 1;
        while s.get(j).is_some_and(|c| word(*c) || *c == '$') {
            j += 1;
        }
        (s.get(j) == Some(&'(')).then_some(j)
    })
}

/// `\$\{?\w+\}?`.
fn bash_var(s: &[char]) -> Option<(usize, usize)> {
    leftmost(s, |p| {
        if s[p] != '$' {
            return None;
        }
        let run = |from: usize| {
            let mut j = from;
            while s.get(j).is_some_and(|c| word(*c)) {
                j += 1;
            }
            j
        };
        let braced = s.get(p + 1) == Some(&'{');
        let mut end = if braced { run(p + 2) } else { 0 };
        if !braced || end == p + 2 {
            end = run(p + 1);
            if end == p + 1 {
                return None;
            }
        }
        if s.get(end) == Some(&'}') {
            end += 1;
        }
        Some(end)
    })
}

/// `(^|(?<=\|\s*))[a-z][\w-]*`.
fn bash_key(s: &[char]) -> Option<(usize, usize)> {
    leftmost(s, |p| {
        let mut b = p;
        while b > 0 && js_space(s[b - 1]) {
            b -= 1;
        }
        let ok = p == 0 || (b > 0 && s[b - 1] == '|');
        if !ok || !s[p].is_ascii_lowercase() {
            return None;
        }
        let mut j = p + 1;
        while s.get(j).is_some_and(|c| word(*c) || *c == '-') {
            j += 1;
        }
        Some(j)
    })
}

/// `\s--?[\w-]+`.
fn bash_flag(s: &[char]) -> Option<(usize, usize)> {
    leftmost(s, |p| {
        if !js_space(s[p]) || s.get(p + 1) != Some(&'-') {
            return None;
        }
        let run = |from: usize| {
            let mut j = from;
            while s.get(j).is_some_and(|c| word(*c) || *c == '-') {
                j += 1;
            }
            (j > from).then_some(j)
        };
        if s.get(p + 2) == Some(&'-')
            && let Some(e) = run(p + 3)
        {
            return Some(e);
        }
        run(p + 2)
    })
}

fn rules_for(lang: Option<&str>) -> &'static [(Rule, TokenKind)] {
    use TokenKind::{Bool, Com, Key, Kw, Num, Punct, Str};
    const YAML: &[(Rule, TokenKind)] = &[
        (hash_comment, Com),
        (yaml_key, Key),
        (yaml_str, Str),
        (yaml_bool, Bool),
        (yaml_num, Num),
        (yaml_punct, Punct),
    ];
    const JSON: &[(Rule, TokenKind)] = &[
        (json_key, Key),
        (json_str, Str),
        (json_bool, Bool),
        (json_num, Num),
        (json_punct, Punct),
    ];
    const JS: &[(Rule, TokenKind)] = &[
        (slash_comment, Com),
        (js_str, Str),
        (js_kw, Kw),
        (js_bool, Bool),
        (js_num, Num),
        (js_key, Key),
        (js_punct, Punct),
    ];
    const BASH: &[(Rule, TokenKind)] = &[
        (hash_comment, Com),
        (yaml_str, Str),
        (bash_var, Num),
        (bash_key, Key),
        (bash_flag, Bool),
        (bash_punct, Punct),
    ];
    match lang.unwrap_or("").to_lowercase().as_str() {
        "yaml" | "yml" => YAML,
        "json" => JSON,
        "js" | "javascript" | "ts" | "typescript" => JS,
        "bash" | "sh" | "shell" => BASH,
        _ => &[],
    }
}

/// One line's tokens. An empty line is one space of text, so its row keeps its height.
pub fn highlight_line(line: &str, rules: &[(Rule, TokenKind)]) -> Vec<Token> {
    let chars: Vec<char> = line.chars().collect();
    let mut out = Vec::new();
    let mut start = 0;
    let mut guard = 0;
    let text = |a: usize, b: usize| chars[a..b].iter().collect::<String>();
    while start < chars.len() && guard < 400 {
        guard += 1;
        let rest = &chars[start..];
        let mut best: Option<(usize, usize, TokenKind)> = None;
        for (rule, kind) in rules {
            if let Some((a, b)) = rule(rest)
                && best.is_none_or(|(i, _, _)| a < i)
            {
                best = Some((a, b, *kind));
            }
        }
        match best {
            Some((a, b, kind)) if b > a => {
                if a > 0 {
                    out.push(Token {
                        kind: TokenKind::Text,
                        text: text(start, start + a),
                    });
                }
                out.push(Token {
                    kind,
                    text: text(start + a, start + b),
                });
                start += b;
            }
            _ => {
                out.push(Token {
                    kind: TokenKind::Text,
                    text: text(start, chars.len()),
                });
                start = chars.len();
                break;
            }
        }
    }
    // The guard stops a pathological line (a minified file); what is left is shown as plain text, never dropped.
    if start < chars.len() {
        out.push(Token {
            kind: TokenKind::Text,
            text: text(start, chars.len()),
        });
    }
    if out.is_empty() {
        out.push(Token {
            kind: TokenKind::Text,
            text: " ".into(),
        });
    }
    out
}

/// Every line of `code`, tokenized for `lang`.
pub fn highlight(lang: Option<&str>, code: &str) -> Vec<Vec<Token>> {
    let rules = rules_for(lang);
    code.split('\n').map(|l| highlight_line(l, rules)).collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use TokenKind::{Bool, Com, Key, Kw, Num, Punct, Str};

    fn kinds(lang: &str, line: &str) -> Vec<(TokenKind, String)> {
        highlight(Some(lang), line)[0]
            .iter()
            .filter(|t| t.kind != TokenKind::Text)
            .map(|t| (t.kind, t.text.clone()))
            .collect()
    }

    fn k(items: &[(TokenKind, &str)]) -> Vec<(TokenKind, String)> {
        items.iter().map(|(a, b)| (*a, (*b).to_owned())).collect()
    }

    fn text(lang: &str, line: &str) -> String {
        highlight(Some(lang), line)[0]
            .iter()
            .map(|t| t.text.as_str())
            .collect()
    }

    #[test]
    fn yaml_keys_match_with_a_leading_indent_and_a_list_prefix() {
        assert_eq!(kinds("yaml", "raid:"), k(&[(Key, "raid"), (Punct, ":")]));
        assert_eq!(
            kinds("yaml", "  when: \"20:00\""),
            k(&[(Key, "  when"), (Punct, ":"), (Str, "\"20:00\"")])
        );
        assert_eq!(
            kinds("yaml", "  - name: rope"),
            k(&[(Key, "  - name"), (Punct, ":")])
        );
        assert_eq!(
            kinds("yaml", "    - rope      # two"),
            k(&[(Punct, "-"), (Com, "# two")])
        );
        assert_eq!(
            kinds("yml", "troll: true"),
            k(&[(Key, "troll"), (Punct, ":"), (Bool, "true")])
        );
    }

    #[test]
    fn json_a_quoted_key_is_a_key_a_quoted_value_a_string() {
        assert_eq!(
            kinds("json", "\"party\": [\"Ada\", 6]"),
            k(&[
                (Key, "\"party\""),
                (Punct, ":"),
                (Punct, "["),
                (Str, "\"Ada\""),
                (Punct, ","),
                (Num, "6"),
                (Punct, "]"),
            ])
        );
    }

    #[test]
    fn js_and_ts_keywords_and_function_names() {
        assert_eq!(
            kinds("ts", "const a = load(1);"),
            k(&[
                (Kw, "const"),
                (Punct, "="),
                (Key, "load"),
                (Punct, "("),
                (Num, "1"),
                (Punct, ")"),
                (Punct, ";"),
            ])
        );
    }

    #[test]
    fn bash_the_command_flags_variables_and_comments() {
        assert_eq!(
            kinds("sh", "npm test --watch $HOME # go"),
            k(&[
                (Key, "npm"),
                (Bool, " --watch"),
                (Num, "$HOME"),
                (Com, "# go")
            ])
        );
        assert_eq!(
            kinds("bash", "a | grep -v x"),
            k(&[(Key, "a"), (Punct, "|"), (Bool, " -v")])
        );
    }

    #[test]
    fn numbers_take_a_fraction_an_exponent_and_a_sign() {
        assert_eq!(kinds("json", "-1.5e3"), k(&[(Num, "-1.5e3")]));
        assert_eq!(
            kinds("yaml", "x: 1.5x")[..3].to_vec(),
            k(&[(Key, "x"), (Punct, ":"), (Num, "1")])
        );
    }

    #[test]
    fn a_line_over_the_guard_keeps_every_character() {
        let line = format!(
            "{{{}}}",
            (0..300)
                .map(|i| format!("\"k{i}\":{i}"))
                .collect::<Vec<_>>()
                .join(",")
        );
        assert!(line.len() > 2000);
        assert_eq!(text("json", &line), line);
    }

    #[test]
    fn an_empty_line_is_one_space_and_an_unknown_language_plain_text() {
        let lines = highlight(Some("yaml"), "a: 1\n\nb: 2");
        assert_eq!(
            lines[1],
            vec![Token {
                kind: TokenKind::Text,
                text: " ".into()
            }]
        );
        assert_eq!(
            highlight(Some("rust"), "fn main() {}")[0],
            vec![Token {
                kind: TokenKind::Text,
                text: "fn main() {}".into()
            }]
        );
        assert_eq!(highlight(None, "x")[0][0].text, "x");
    }

    #[test]
    fn every_line_gives_back_its_text() {
        for (lang, line) in [
            ("yaml", "  - name: \"a b\" # c"),
            ("js", "let s = 'x' + `y` // z"),
            ("bash", "echo \"$A\" | wc -l && ls"),
            ("json", "{\"a\": [1, 2.5, true, null]}"),
        ] {
            assert_eq!(text(lang, line), line);
        }
    }
}
