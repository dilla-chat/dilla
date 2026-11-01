/**
 * A tiny line tokenizer for code blocks (README §6.4), ported rule for rule from the Signal prototype's `hl()`: yaml,
 * json, js/ts and bash. Each line is cut into tokens by trying every rule and taking the earliest match (the first rule
 * wins a tie). YAML keys match with an optional leading indent and an optional `- ` list prefix. Text only: the result is
 * rendered as React text children, never HTML.
 */
export type TokenKind = 'key' | 'str' | 'num' | 'com' | 'bool' | 'kw' | 'punct' | 'text';
export interface Token { kind: TokenKind; text: string }

type Rule = readonly [RegExp, TokenKind];

const RULES: Record<'yaml' | 'json' | 'js' | 'bash', readonly Rule[]> = {
  yaml: [[/#.*$/, 'com'], [/^\s*(?:-\s+)?[\w.-]+(?=\s*:)/, 'key'], [/"(?:[^"\\]|\\.)*"|'[^']*'/, 'str'], [/\b(true|false|null|yes|no)\b/, 'bool'],
    [/-?\b\d+(\.\d+)?\b/, 'num'], [/[:\-[\]{},|>]/, 'punct']],
  json: [[/"(?:[^"\\]|\\.)*"(?=\s*:)/, 'key'], [/"(?:[^"\\]|\\.)*"/, 'str'], [/\b(true|false|null)\b/, 'bool'], [/-?\b\d+(\.\d+)?([eE][+-]?\d+)?\b/, 'num'],
    [/[{}[\],:]/, 'punct']],
  js: [[/\/\/.*$/, 'com'], [/"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|`(?:[^`\\]|\\.)*`/, 'str'],
    [/\b(const|let|var|function|return|if|else|for|while|import|from|export|default|await|async|new|class|extends|try|catch|throw|of|in)\b/, 'kw'],
    [/\b(true|false|null|undefined|this)\b/, 'bool'], [/\b\d+(\.\d+)?\b/, 'num'], [/[A-Za-z_$][\w$]*(?=\()/, 'key'], [/[{}()[\];,.=<>+\-*/!&|?:]/, 'punct']],
  bash: [[/#.*$/, 'com'], [/"(?:[^"\\]|\\.)*"|'[^']*'/, 'str'], [/\$\{?\w+\}?/, 'num'], [/(^|(?<=\|\s*))[a-z][\w-]*/, 'key'], [/\s--?[\w-]+/, 'bool'], [/[|&;<>]/, 'punct']],
};

const ALIASES: Record<string, keyof typeof RULES> = {
  yaml: 'yaml', yml: 'yaml', json: 'json', js: 'js', javascript: 'js', ts: 'js', typescript: 'js', bash: 'bash', sh: 'bash', shell: 'bash',
};

/** The rule set for a fence's language word; an unknown language is plain text. */
function rulesFor(lang: string | undefined): readonly Rule[] {
  const set = ALIASES[(lang ?? '').toLowerCase()];
  return set === undefined ? [] : RULES[set];
}

/** One line's tokens. An empty line is one space of text, so its row keeps its height. */
export function highlightLine(line: string, rules: readonly Rule[]): Token[] {
  const out: Token[] = [];
  let rest = line;
  let guard = 0;
  while (rest.length > 0 && guard++ < 400) {
    let best: { i: number; text: string; kind: TokenKind } | null = null;
    for (const [re, kind] of rules) {
      const m = re.exec(rest);
      if (m !== null && (best === null || m.index < best.i)) best = { i: m.index, text: m[0], kind };
    }
    if (best === null || best.text === '') { out.push({ kind: 'text', text: rest }); break; }
    if (best.i > 0) out.push({ kind: 'text', text: rest.slice(0, best.i) });
    out.push({ kind: best.kind, text: best.text });
    rest = rest.slice(best.i + best.text.length);
  }
  // The guard stops a pathological line (a minified file); what is left is shown as plain text, never dropped.
  if (rest.length > 0 && guard > 400) out.push({ kind: 'text', text: rest });
  if (out.length === 0) out.push({ kind: 'text', text: ' ' });
  return out;
}

/** Every line of `code`, tokenized for `lang`. */
export function highlight(lang: string | undefined, code: string): Token[][] {
  const rules = rulesFor(lang);
  return code.split('\n').map(line => highlightLine(line, rules));
}
