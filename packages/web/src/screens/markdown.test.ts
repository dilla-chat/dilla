import { describe, expect, it } from 'vitest';
import type { BodyPart } from '@dilla/ui';
import { CACHE_LIMITS, cacheSize, parseMarkdown, parseTree, parseTreeUncached, safeLink } from './markdown.ts';

const ID = 'a'.repeat(32);
const resolve = (target: string): BodyPart => ({ kind: 'mention', label: `@${target.slice(0, 3)}`, me: false, broadcast: false });
const parse = (s: string): BodyPart[] => parseMarkdown(s, resolve);
const text = (t: string): BodyPart => ({ kind: 'text', text: t });

/** Everything a reader sees, flat: text, code, link text and pill labels. */
function visible(parts: readonly BodyPart[]): string {
  return parts.map(p => {
    switch (p.kind) {
      case 'text': case 'code': case 'link': return p.text;
      case 'codeblock': return (p.lang ?? '') + p.text;
      case 'mention': return p.label;
      default: return visible(p.children);
    }
  }).join('');
}

describe('parseMarkdown', () => {
  it.each<[string, string, BodyPart[]]>([
    ['plain text is one text part', 'hello there', [text('hello there')]],
    ['strong', '**bold**', [{ kind: 'strong', children: [text('bold')] }]],
    ['em with stars', '*it*', [{ kind: 'em', children: [text('it')] }]],
    ['em with underscores', '_it_', [{ kind: 'em', children: [text('it')] }]],
    ['strike', '~~gone~~', [{ kind: 'strike', children: [text('gone')] }]],
    ['code span', 'a `b *c*` d', [text('a '), { kind: 'code', text: 'b *c*' }, text(' d')]],
    ['double-tick code span holds a tick', '``a`b``', [{ kind: 'code', text: 'a`b' }]],
    ['nesting', '**a *b* c**', [{ kind: 'strong', children: [text('a '), { kind: 'em', children: [text('b')] }, text(' c')] }]],
    ['strong and em together', '***x***', [{ kind: 'strong', children: [{ kind: 'em', children: [text('x')] }] }]],
    ['strike inside strong', '**~~x~~**', [{ kind: 'strong', children: [{ kind: 'strike', children: [text('x')] }] }]],
    ['unclosed strong stays literal', '**open', [text('**open')]],
    ['unclosed em stays literal', 'a *b', [text('a *b')]],
    ['unclosed code stays literal', 'a `b', [text('a `b')]],
    ['a lone star between spaces is text', '2 * 3 * 4', [text('2 * 3 * 4')]],
    ['snake_case is not emphasis', 'snake_case_name', [text('snake_case_name')]],
    ['empty emphasis is text', '****', [text('****')]],
    ['a backslash escapes a marker', '\\*not\\*', [text('*not*')]],
    ['a backslash escapes a backslash', '\\\\*x*', [text('\\'), { kind: 'em', children: [text('x')] }]],
    ['a backslash before a letter stays', 'C:\\dir', [text('C:\\dir')]],
    ['a trailing backslash stays', 'end\\', [text('end\\')]],
    ['fenced block', '```\nline *1*\nline 2\n```', [{ kind: 'codeblock', text: 'line *1*\nline 2' }]],
    ['fenced block with a language tag', '```ts\nlet a;\n```', [{ kind: 'codeblock', text: 'let a;', lang: 'ts' }]],
    ['F4: an opening line that says more than a language is literal', '```hidden text a moderator never sees\nshown\n```', [{ kind: 'code', text: 'hidden text a moderator never sees\nshown\n' }]],
    ['F4: text after an unclosed fence stays visible', 'a\n```\nsecret\nmore', [text('a\n```\nsecret\nmore')]],
    ['an unclosed fence is text', '```\nopen', [text('```\nopen')]],
    ['text around a fence', 'before\n```\nx\n```\nafter', [text('before'), { kind: 'codeblock', text: 'x' }, text('after')]],
    ['quote', '> hi *you*', [{ kind: 'quote', children: [text('hi '), { kind: 'em', children: [text('you')] }] }]],
    ['quote of two lines', '> a\n> b', [{ kind: 'quote', children: [text('a\nb')] }]],
    ['quote then text', '> a\nb', [{ kind: 'quote', children: [text('a')] }, text('b')]],
    ['a > mid-line is text', 'a > b', [text('a > b')]],
    ['markdown link syntax is not a link, the url inside is', '[click](https://evil.example)', [text('[click]('), { kind: 'link', href: 'https://evil.example/', text: 'https://evil.example/' }, text(')')]],
    ['raw html stays text', '<b>x</b><img src=x onerror=alert(1)>', [text('<b>x</b><img src=x onerror=alert(1)>')]],
  ])('%s', (_name, input, expected) => {
    expect(parse(input)).toEqual(expected);
  });

  describe('links', () => {
    it('links a bare https url as its own text', () => {
      expect(parse('see https://example.com/a?b=1#c now')).toEqual([
        text('see '), { kind: 'link', href: 'https://example.com/a?b=1#c', text: 'https://example.com/a?b=1#c' }, text(' now'),
      ]);
    });
    it('links http and mailto', () => {
      expect(parse('http://example.com')).toEqual([{ kind: 'link', href: 'http://example.com/', text: 'http://example.com/' }]);
      expect(parse('mailto:a@example.com')).toEqual([{ kind: 'link', href: 'mailto:a@example.com', text: 'mailto:a@example.com' }]);
    });
    it('leaves trailing punctuation out of the link', () => {
      expect(parse('(https://example.com/x).')).toEqual([text('('), { kind: 'link', href: 'https://example.com/x', text: 'https://example.com/x' }, text(').')]);
      expect(parse('https://example.com/a_(b)')).toEqual([{ kind: 'link', href: 'https://example.com/a_(b)', text: 'https://example.com/a_(b)' }]);
    });
    it('links inside formatting', () => {
      expect(parse('**https://example.com**')).toEqual([{ kind: 'strong', children: [{ kind: 'link', href: 'https://example.com/', text: 'https://example.com/' }] }]);
    });
    it.each([
      'javascript:alert(1)', 'JaVaScRiPt:alert(1)', 'data:text/html,<script>alert(1)</script>', 'vbscript:x', 'file:///etc/passwd',
      'blob:https://example.com/x', 'ftp://example.com', 'tel:+4631', '//example.com', 'https://user:pw@example.com', 'https://user@example.com',
      'https://', 'https:///x', 'http://exa\u202Emple.com', 'https://example.com/\u200bx', 'mailto:',
    ])('refuses %s', input => {
      expect(safeLink(input)).toBeNull();
      expect(parse(input).some(p => p.kind === 'link')).toBe(false);
      expect(visible(parse(input))).toBe(input);
    });
    it('does not link a url glued to a word', () => {
      expect(parse('xhttps://example.com').some(p => p.kind === 'link')).toBe(false);
    });
    it('does not link or format inside code', () => {
      expect(parse('`https://example.com`')).toEqual([{ kind: 'code', text: 'https://example.com' }]);
      expect(parse('```\nhttps://example.com\n```')).toEqual([{ kind: 'codeblock', text: 'https://example.com' }]);
    });
    it('only ever hands out http, https or mailto hrefs', () => {
      for (const p of parse('javascript:alert(1) https://a.example mailto:x@y.example data:x')) {
        if (p.kind === 'link') expect(/^(https?:|mailto:)/.test(p.href)).toBe(true);
      }
    });
  });

  describe('mentions', () => {
    it('keeps a mention a pill', () => {
      expect(parse(`hi <@${ID}>`)).toEqual([text('hi '), resolve(ID)]);
    });
    it('keeps a mention inside formatting', () => {
      expect(parse(`**hi <@${ID}>**`)).toEqual([{ kind: 'strong', children: [text('hi '), resolve(ID)] }]);
      expect(parse(`> <@everyone> *x*`)).toEqual([{ kind: 'quote', children: [resolve('everyone'), text(' '), { kind: 'em', children: [text('x')] }] }]);
    });
    it('formatting can span a mention', () => {
      expect(parse(`*a <@${ID}> b*`)).toEqual([{ kind: 'em', children: [text('a '), resolve(ID), text(' b')] }]);
    });
    it('does not parse a mention in a code span or block', () => {
      expect(parse(`\`<@${ID}>\``)).toEqual([{ kind: 'code', text: `<@${ID}>` }]);
      expect(parse(`\`\`\`\n<@${ID}>\n\`\`\``)).toEqual([{ kind: 'codeblock', text: `<@${ID}>` }]);
    });
    it('a mention is not a word, so it cannot glue a url', () => {
      expect(parse(`<@${ID}>https://example.com`).filter(p => p.kind === 'link')).toHaveLength(1);
    });
  });

  it('survives deep nesting and long runs of markers', () => {
    expect(() => parse('*'.repeat(5000))).not.toThrow();
    expect(() => parse('*a '.repeat(2000))).not.toThrow();
    expect(() => parse('**a '.repeat(1000) + '**'.repeat(1000))).not.toThrow();
    expect(() => parse('`'.repeat(3000) + '~'.repeat(3000) + '>'.repeat(3000))).not.toThrow();
  });

  // The parser only removes markers: what a reader sees is the input with some of `\ * _ ~ ` > `, line breaks, spaces and a
  // fence's lines taken out, in the same order, whatever the input is. (No address in the alphabet: a link shows its
  // normalised href, which is the next test.)
  const MARKERS = new Set([...'\\*_~`>\n ']);
  function fuzz(alphabet: readonly string[], runs: number, maxLen: number, seed0: number): void {
    let seed = seed0;
    const rand = (n: number): number => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return (seed >> 8) % n; };
    for (let run = 0; run < runs; run++) {
      const input = Array.from({ length: rand(maxLen) }, () => alphabet[rand(alphabet.length)]).join('');
      const parts = parseMarkdown(input, t => ({ kind: 'mention', label: `<@${t}>`, me: false, broadcast: false }));
      const seen = visible(parts);
      let at = 0;
      let bad = '';
      for (const ch of seen) {
        const found = input.indexOf(ch, at);
        if (found < 0) { bad = 'not in order'; break; }
        if ([...input.slice(at, found)].some(d => !MARKERS.has(d))) { bad = 'dropped a non-marker'; break; }
        at = found + ch.length;
      }
      if (bad === '' && [...input.slice(at)].some(d => !MARKERS.has(d))) bad = 'dropped a non-marker at the end';
      expect(bad, `${JSON.stringify(input)} -> ${JSON.stringify(seen)}`).toBe('');
    }
  }
  it('fuzz: the visible text is always the input minus markers', () => {
    fuzz(['a', 'b', ' ', '*', '_', '~', '`', '\\', '>', '\n', ':', '/', '.', '@', `<@${ID}>`, '<', '[', ']', '(', ')', 'x'], 4000, 40, 7);
  });
  it('F5: fuzz with long inputs and fences, backticks, newlines and brackets', () => {
    fuzz(['a', 'x', ' ', '*', '_', '~~', '`', '```', '\n```\n', '```x\n', '\n', '\\', '> ', '[', ']', '(', ')', `<@${ID}>`, '**', 'a b'], 1500, 200, 11);
  });
  it('fuzz: every link that comes out is canonical, safe and shown as its own href', () => {
    const alphabet = ['a', ' ', '*', '_', '`', '\n', '(', ')', 'http', 'https://', 'mailto:', 'x@y.z', 'a.b', '/', ':', '.', '\u202e', '\u061c', '\u3164', 'а', '?', '#'];
    let seed = 3;
    const rand = (n: number): number => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return (seed >> 8) % n; };
    const walk = (ps: readonly BodyPart[]): void => ps.forEach(p => {
      if (p.kind === 'link') { expect(safeLink(p.href)).toBe(p.href); expect(p.text).toBe(p.href); }
      if ('children' in p) walk(p.children);
    });
    for (let run = 0; run < 3000; run++) walk(parse(Array.from({ length: rand(60) }, () => alphabet[rand(alphabet.length)]).join('')));
  });

  describe('F1: bidi controls cannot reorder a link', () => {
    const BODY = '\u202e https://evil.example/?moc.elgoog//:sptth';
    it('the link keeps its typed order and the override is gone from the text around it', () => {
      const parts = parse(BODY);
      expect(parts).toEqual([text(' '), { kind: 'link', href: 'https://evil.example/?moc.elgoog//:sptth', text: 'https://evil.example/?moc.elgoog//:sptth' }]);
    });
    it('removes every bidi control from text, code, blocks and pill labels of a body that holds a link', () => {
      const controls = '\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069\u200e\u200f\u061c';
      const parts = parseMarkdown(`${controls}a \`${controls}b\` **${controls}c** <@${ID}> https://example.com`, () => ({ kind: 'mention', label: `${controls}@bob`, me: false, broadcast: false }));
      expect(JSON.stringify(parts)).not.toMatch(/[\u202a-\u202e\u2066-\u2069\u200e\u200f\u061c]/);
    });
    it('leaves bidi controls alone in a body with no link (a plain Arabic sentence keeps its marks)', () => {
      expect(visible(parse('abc \u200f def \u061c'))).toBe('abc \u200f def \u061c');
    });
  });

  describe('F2: the parse is linear', () => {
    const best = (input: string): number => {
      parseTreeUncached(input);
      let min = Infinity;
      for (let k = 0; k < 5; k++) { const t0 = performance.now(); parseTreeUncached(input); min = Math.min(min, performance.now() - t0); }
      return min;
    };
    const cases: Record<string, (n: number) => string> = {
      'a url then ) repeated': n => 'http://a/' + ')'.repeat(n - 9),
      '(h repeated': n => '(h'.repeat(n / 2),
      'a url then dots': n => 'http://a' + '.'.repeat(n - 8),
      'unclosed fences': n => '```a\n'.repeat(n / 5),
      'unclosed stars': n => '*a '.repeat(n / 3),
      'unclosed tildes and underscores': n => '~~a_a '.repeat(n / 6),
      'backtick runs of growing length': n => { let s = ''; let k = 1; while (s.length < n) { s += '`'.repeat(k) + 'a'; k++; } return s.slice(0, n); },
      'quotes': n => '> *a\n'.repeat(n / 5),
      'links in brackets': n => '(http://a'.repeat(n / 9),
      'many short links': n => 'http://a.b/c '.repeat(n / 13),
      'links with parentheses': n => 'https://a.b/(x)) '.repeat(n / 17),
      'links in emphasis': n => '*http://a.b/c* '.repeat(n / 15),
    };
    /**
     * One 32k parse against eight 4k parses, timed in turns, the best of seven of each. Both sides parse the same number of
     * characters over about the same time, so a busy runner slows both alike. The old form divided one 32k parse by one 4k
     * parse floored at 1 ms: a 4k parse takes under 1 ms, so the ratio was the 32k time in ms and the check was "32k in
     * under 16 ms", which a loaded CI runner missed on a linear parse (2026-10-30, unclosed fences: 18.9).
     */
    const growth = (small: string, large: string): number => {
      parseTreeUncached(small); parseTreeUncached(large);
      let eight = Infinity;
      let one = Infinity;
      for (let k = 0; k < 7; k++) {
        let t0 = performance.now();
        for (let i = 0; i < 8; i++) parseTreeUncached(small);
        eight = Math.min(eight, performance.now() - t0);
        t0 = performance.now();
        parseTreeUncached(large);
        one = Math.min(one, performance.now() - t0);
      }
      return one / Math.max(eight, 0.1);
    };
    for (const [name, make] of Object.entries(cases)) {
      it(`${name}: grows less than 24-fold from 4k to 32k characters, 4000 in under 25 ms, 50000 in under 250 ms`, () => {
        expect(best(make(4000))).toBeLessThan(25);
        expect(best(make(50000))).toBeLessThan(250);
        // Eight times the input must cost less than 24 times the time: one 32k parse under three times eight 4k parses
        // (linear is 1, measured 1.1 to 1.5 for these cases; quadratic is 8).
        expect(growth(make(4000), make(32000))).toBeLessThan(3);
      });
    }
    it('remembers the parse of a body, so a re-render does not parse again', () => {
      const body = '**x** https://example.com ' + Math.random();
      expect(parseTree(body)).toBe(parseTree(body));
      expect(parseTreeUncached(body)).not.toBe(parseTree(body));
    });
  });

  describe('N1: the memo is bounded', () => {
    it('keeps at most 200 bodies, dropping the least recently used first', () => {
      const first = 'first body ' + Math.random();
      const kept = parseTree(first);
      for (let k = 0; k < CACHE_LIMITS.entries - 1; k++) { parseTree(`filler ${k} ${Math.random()}`); parseTree(first); }
      for (let k = 0; k < 150; k++) parseTree(`more ${k} ${Math.random()}`);
      expect(cacheSize().entries).toBeLessThanOrEqual(CACHE_LIMITS.entries);
      expect(parseTree(first)).toBe(kept);
    });
    it('keeps at most 2 MB of body text', () => {
      for (let k = 0; k < 40; k++) parseTree(`${k} ${Math.random()} `.padEnd(3900, 'x'));
      expect(cacheSize().chars).toBeLessThanOrEqual(CACHE_LIMITS.chars);
      for (let k = 0; k < 700; k++) parseTree(`${k} ${Math.random()} `.padEnd(3900, 'y'));
      expect(cacheSize().chars).toBeLessThanOrEqual(CACHE_LIMITS.chars);
      expect(cacheSize().entries).toBeLessThanOrEqual(CACHE_LIMITS.entries);
    });
    it('does not cache a body over 4000 characters', () => {
      const long = 'a'.repeat(CACHE_LIMITS.body + 1) + Math.random();
      const before = cacheSize();
      expect(parseTree(long).tree).toEqual(parseTreeUncached(long).tree);
      expect(cacheSize()).toEqual(before);
    });
  });

  describe('F6: invisible and format characters', () => {
    it.each([
      ['U+061C', '\u061c'], ['U+115F', '\u115f'], ['U+1160', '\u1160'], ['U+3164', '\u3164'], ['U+FFA0', '\uffa0'], ['tag U+E0041', '\u{e0041}'],
      ['U+E0000', '\u{e0000}'], ['U+034F', '\u034f'], ['U+180E', '\u180e'], ['U+2800', '\u2800'], ['ZWJ', '\u200d'], ['soft hyphen', '\u00ad'],
      ['U+FFF9', '\ufff9'], ['word joiner', '\u2060'], ['U+202E', '\u202e'],
    ])('refuses a link holding %s', (_n, ch) => {
      for (const url of [`https://good.example/${ch}x`, `https://good.example/x${ch}`, `https://go${ch}od.example/`]) {
        expect(safeLink(url)).toBeNull();
        expect(parse(url).some(p => p.kind === 'link')).toBe(false);
      }
    });
  });

  describe('F7: the host is shown as ASCII', () => {
    it('shows and opens the punycode host of a lookalike', () => {
      expect(parse('https://\u0430pple.com/login')).toEqual([{ kind: 'link', href: 'https://xn--pple-43d.com/login', text: 'https://xn--pple-43d.com/login' }]);
      expect(parse('https://g\u00f6\u00f6gle.com/')).toEqual([{ kind: 'link', href: 'https://xn--ggle-5qaa.com/', text: 'https://xn--ggle-5qaa.com/' }]);
    });
    it('shows a non-ASCII path percent-encoded, as it is opened', () => {
      expect(parse('https://example.com/\u00e5')).toEqual([{ kind: 'link', href: 'https://example.com/%C3%A5', text: 'https://example.com/%C3%A5' }]);
    });
  });

  describe('mailto links are the bare address only', () => {
    it('links the bare address', () => {
      expect(parse('mail mailto:Ada.L+x@example.com now')).toEqual([text('mail '), { kind: 'link', href: 'mailto:Ada.L+x@example.com', text: 'mailto:Ada.L+x@example.com' }, text(' now')]);
    });
    it.each(['mailto:a@b.example?attach=/etc/passwd', 'mailto:a@b.example?body=x', 'mailto:a@b.example?cc=c@d.example', 'mailto:a@b.example,c@d.example', 'mailto:?to=a@b.example', 'mailto:nobody', 'mailto:a@b.example/x'])('does not link %s', input => {
      expect(safeLink(input)).toBeNull();
      expect(parse(input).some(p => p.kind === 'link')).toBe(false);
    });
  });

  it('fuzz: text without any marker comes back as exactly itself', () => {
    for (const input of ['', 'plain', 'a b  c', 'emoji 🎉 ok', 'tabs\tand\nlines', 'a.b/c:d@e <not a mention>']) {
      expect(visible(parse(input))).toBe(input);
    }
  });
});
