// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { highlight } from './highlight.ts';

const kinds = (lang: string, line: string) => highlight(lang, line)[0]!.filter(t => t.kind !== 'text').map(t => [t.kind, t.text]);
const text = (lang: string, line: string) => highlight(lang, line)[0]!.map(t => t.text).join('');

describe('the code block tokenizer (README §6.4, the prototype hl())', () => {
  it('yaml: keys match with a leading indent and with a "- " list prefix', () => {
    expect(kinds('yaml', 'raid:')).toEqual([['key', 'raid'], ['punct', ':']]);
    expect(kinds('yaml', '  when: "20:00"')).toEqual([['key', '  when'], ['punct', ':'], ['str', '"20:00"']]);
    expect(kinds('yaml', '  - name: rope')).toEqual([['key', '  - name'], ['punct', ':']]);
    expect(kinds('yaml', '    - rope      # two')).toEqual([['punct', '-'], ['com', '# two']]);
    expect(kinds('yml', 'troll: true')).toEqual([['key', 'troll'], ['punct', ':'], ['bool', 'true']]);
  });
  it('json: a quoted key is a key, a quoted value a string', () => {
    expect(kinds('json', '"party": ["Ada", 6]')).toEqual([['key', '"party"'], ['punct', ':'], ['punct', '['], ['str', '"Ada"'], ['punct', ','], ['num', '6'], ['punct', ']']]);
  });
  it('js and ts: keywords and function names', () => {
    expect(kinds('ts', 'const a = load(1);')).toEqual([['kw', 'const'], ['punct', '='], ['key', 'load'], ['punct', '('], ['num', '1'], ['punct', ')'], ['punct', ';']]);
  });
  it('bash: the command, flags, variables and comments', () => {
    expect(kinds('sh', 'npm test --watch $HOME # go')).toEqual([['key', 'npm'], ['bool', ' --watch'], ['num', '$HOME'], ['com', '# go']]);
  });
  it('renders every character of a line over the 400-token guard (a minified file)', () => {
    const line = JSON.stringify(Object.fromEntries(Array.from({ length: 300 }, (_, i) => [`k${String(i)}`, i])));
    expect(line.length).toBeGreaterThan(2000);
    expect(text('json', line)).toBe(line);
  });
  it('keeps an empty line as one space, and an unknown language as plain text', () => {
    expect(highlight('yaml', 'a: 1\n\nb: 2')[1]).toEqual([{ kind: 'text', text: ' ' }]);
    expect(highlight('rust', 'fn main() {}')[0]).toEqual([{ kind: 'text', text: 'fn main() {}' }]);
  });
});
