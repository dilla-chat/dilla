/**
 * The block-YAML subset .github/workflows/ci.yml is written in, parsed without a dependency (scripts/ci-local.mjs runs
 * before `npm ci` has to, and scripts/check-ci-workflow.mjs runs it in the docs job): block mappings and sequences,
 * `- key: value` items, `|`/`|-`/`>`/`>-` block scalars, `[a, b]` flow sequences, one-line `{ k: v, … }` flow mappings
 * of scalars (a matrix include leg), single- and double-quoted scalars and comments. Every scalar stays a string.
 * Anchors, aliases, tags, nested flow collections and multi-document streams are refused with the line they are on,
 * never guessed at (check-ci-workflow.mjs forbids anchors in ci.yml as well).
 */

class YamlError extends Error {
  constructor(message, line) {
    super(`ci.yml:${line + 1}: ${message}`);
  }
}

const indentOf = (raw) => raw.length - raw.trimStart().length;
const isSkippable = (raw) => raw.trim() === '' || raw.trimStart().startsWith('#');

/** A plain or quoted scalar with any trailing comment removed. */
function scalar(text, line) {
  const t = text.trim();
  if (t === '') return null;
  if (t.startsWith("'")) {
    const m = /^'((?:[^']|'')*)'\s*(?:#.*)?$/.exec(t);
    if (!m) throw new YamlError(`unterminated single-quoted scalar: ${t}`, line);
    return m[1].replaceAll("''", "'");
  }
  if (t.startsWith('"')) {
    const m = /^"((?:[^"\\]|\\.)*)"\s*(?:#.*)?$/.exec(t);
    if (!m) throw new YamlError(`unterminated double-quoted scalar: ${t}`, line);
    return m[1].replace(/\\(.)/g, (_, c) => ({ n: '\n', t: '\t', '"': '"', '\\': '\\', '/': '/' })[c] ?? `\\${c}`);
  }
  if (t.startsWith('[')) return flowSequence(t, line);
  if (t.startsWith('{')) return flowMapping(t, line);
  if (/^[&*!{]/.test(t) || /^[|>]/.test(t)) throw new YamlError(`unsupported YAML construct: ${t}`, line);
  // A plain scalar ends at " #".
  const hash = t.search(/\s#/);
  return (hash < 0 ? t : t.slice(0, hash)).trimEnd();
}

/** Items of a one-line flow collection body, split at top-level commas; nested collections are refused. */
function flowItems(body, t, line) {
  const items = [];
  let current = '';
  let quote = null;
  for (const ch of body) {
    if (quote !== null) {
      current += ch;
      if (ch === quote) quote = null;
    } else if (ch === "'" || ch === '"') {
      quote = ch;
      current += ch;
    } else if (ch === ',') {
      items.push(current);
      current = '';
    } else if (ch === '[' || ch === '{') {
      throw new YamlError(`nested flow collections are unsupported: ${t}`, line);
    } else current += ch;
  }
  if (current.trim() !== '' || items.length > 0) items.push(current);
  return items;
}

function flowMapping(t, line) {
  const m = /^\{(.*)\}\s*(?:#.*)?$/.exec(t);
  if (!m) throw new YamlError(`unsupported flow mapping: ${t}`, line);
  const out = {};
  for (const item of flowItems(m[1], t, line)) {
    if (item.trim() === '') continue;
    const kv = /^\s*((?:[^\s:'"][^:]*?)|'[^']*'|"[^"]*"):\s+(.*)$/.exec(item);
    if (!kv) throw new YamlError(`unsupported flow mapping entry "${item.trim()}": ${t}`, line);
    const key = scalar(kv[1], line);
    if (Object.hasOwn(out, key)) throw new YamlError(`duplicate key "${key}"`, line);
    out[key] = scalar(kv[2], line);
  }
  return out;
}

function flowSequence(t, line) {
  const m = /^\[(.*)\]\s*(?:#.*)?$/.exec(t);
  if (!m) throw new YamlError(`unsupported flow sequence: ${t}`, line);
  const items = [];
  let current = '';
  let quote = null;
  for (const ch of m[1]) {
    if (quote !== null) {
      current += ch;
      if (ch === quote) quote = null;
    } else if (ch === "'" || ch === '"') {
      quote = ch;
      current += ch;
    } else if (ch === ',') {
      items.push(current);
      current = '';
    } else if (ch === '[' || ch === '{') {
      throw new YamlError(`nested flow collections are unsupported: ${t}`, line);
    } else current += ch;
  }
  if (current.trim() !== '' || items.length > 0) items.push(current);
  return items.map((item) => scalar(item, line)).filter((item) => item !== null);
}

const KEY = /^((?:[^\s:#'"][^:#]*?)|'[^']*'|"[^"]*"):(?:\s+(.*))?$/;

export function parseYaml(text) {
  const lines = text.replace(/\r\n/g, '\n').split('\n');
  // Sequence items that open a mapping (`- key: value`) are re-read as that mapping at the item's content column.
  const overrides = new Map();
  const content = (i) => (overrides.has(i) ? overrides.get(i).content : lines[i].trimStart());
  const indent = (i) => (overrides.has(i) ? overrides.get(i).indent : indentOf(lines[i]));
  let i = 0;

  const skip = () => {
    while (i < lines.length && !overrides.has(i) && isSkippable(lines[i])) i++;
  };

  function blockScalar(header, parentIndent, line) {
    const m = /^([|>])([+-]?)\s*(?:#.*)?$/.exec(header);
    if (!m) throw new YamlError(`unsupported block scalar header: ${header}`, line);
    const body = [];
    let blockIndent = null;
    while (i < lines.length) {
      const raw = lines[i];
      if (raw.trim() === '') {
        body.push('');
        i++;
        continue;
      }
      const ind = indentOf(raw);
      if (ind <= parentIndent) break;
      if (blockIndent === null) blockIndent = ind;
      if (ind < blockIndent) throw new YamlError('block scalar line is indented less than its first line', i);
      body.push(raw.slice(blockIndent));
      i++;
    }
    while (body.length > 0 && body[body.length - 1] === '') body.pop();
    let out;
    if (m[1] === '|') out = body.join('\n');
    else {
      out = '';
      for (const [n, l] of body.entries()) {
        if (n === 0) out = l;
        else if (l === '') out += '\n';
        else out += out.endsWith('\n') || out === '' ? l : ` ${l}`;
      }
    }
    return m[2] === '-' ? out : `${out}\n`;
  }

  function value(rest, keyIndent, line) {
    if (rest === undefined || rest === null || rest.trim() === '' || rest.trim().startsWith('#')) {
      skip();
      if (i >= lines.length) return null;
      const next = indent(i);
      if (next > keyIndent) return block(next);
      if (next === keyIndent && content(i).startsWith('- ')) return sequence(next);
      return null;
    }
    const t = rest.trim();
    if (/^[|>]/.test(t)) return blockScalar(t, keyIndent, line);
    return scalar(t, line);
  }

  function mapping(at) {
    const out = {};
    for (;;) {
      skip();
      if (i >= lines.length || indent(i) !== at || content(i).startsWith('- ')) break;
      const line = i;
      const m = KEY.exec(content(i));
      if (!m) throw new YamlError(`expected "key: value", got: ${content(i)}`, line);
      const key = scalar(m[1], line);
      if (Object.hasOwn(out, key)) throw new YamlError(`duplicate key "${key}"`, line);
      overrides.delete(i);
      i++;
      out[key] = value(m[2], at, line);
    }
    if (i < lines.length && indent(i) > at) throw new YamlError(`unexpected indentation: ${content(i)}`, i);
    return out;
  }

  function sequence(at) {
    const out = [];
    for (;;) {
      skip();
      if (i >= lines.length || indent(i) !== at || !(content(i).startsWith('- ') || content(i) === '-')) break;
      const line = i;
      const rest = content(i) === '-' ? '' : content(i).slice(2);
      const restIndent = at + 2 + (rest.length - rest.trimStart().length);
      if (KEY.test(rest.trim()) && !/^['"{]/.test(rest.trim())) {
        overrides.set(i, { indent: restIndent, content: rest.trim() });
        out.push(mapping(restIndent));
      } else {
        overrides.delete(i);
        i++;
        out.push(value(rest, at, line));
      }
    }
    return out;
  }

  function block(at) {
    skip();
    return content(i).startsWith('- ') || content(i) === '-' ? sequence(at) : mapping(at);
  }

  skip();
  if (i < lines.length && /^(---|%)/.test(lines[i])) throw new YamlError('directives and document markers are unsupported', i);
  const doc = i < lines.length ? block(indent(i)) : null;
  skip();
  if (i < lines.length) throw new YamlError(`unparsed content: ${lines[i].trim()}`, i);
  return doc;
}
