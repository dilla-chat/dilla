import { readFileSync } from 'node:fs';
import { act, render, screen } from '@testing-library/react';
import { describe, it, expect } from 'vitest';
import { MessageRow, type BodyPart } from './MessageRow.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const row = (body: readonly BodyPart[], extra: { active?: boolean } = {}) =>
  render(<MessageRow author="ada" time="21:02" state="ok" body={body} {...extra} />);
const text = (t: string): BodyPart => ({ kind: 'text', text: t });

describe('MessageRow markdown parts', () => {
  it('renders strong, em, strike and code as elements, nested', () => {
    const { container } = row([
      { kind: 'strong', children: [text('a '), { kind: 'em', children: [text('b')] }] },
      text(' '), { kind: 'strike', children: [text('c')] }, text(' '), { kind: 'code', text: 'd<b>' },
    ]);
    const body = container.querySelector('.d-message-row__body') as HTMLElement;
    expect(body.querySelector('strong')?.innerHTML).toBe('a <em>b</em>');
    expect(body.querySelector('s')?.textContent).toBe('c');
    expect(body.querySelector('code')?.textContent).toBe('d<b>');
    expect(body.querySelector('b')).toBeNull();
  });

  it('renders a code block as pre > code and a quote as a blockquote, both outside any paragraph', () => {
    const { container } = row([
      { kind: 'quote', children: [text('said '), { kind: 'strong', children: [text('it')] }] },
      { kind: 'codeblock', text: 'let a = "<script>";' },
    ]);
    const body = container.querySelector('.d-message-row__body') as HTMLElement;
    expect(body.querySelector('blockquote')?.textContent).toBe('said it');
    expect(body.querySelector('pre > code')?.textContent).toBe('let a = "<script>";');
    expect(body.querySelector('script')).toBeNull();
    expect(body.closest('p')).toBeNull();
  });

  it('renders mentions inside formatting as pills', () => {
    const { container } = row([{ kind: 'strong', children: [text('hi '), { kind: 'mention', label: '@ada', me: true, broadcast: false }] }]);
    expect(container.querySelector('strong .d-mention')?.textContent).toBe('@ada');
  });

  it('renders a link as its own text with the hardened attributes', () => {
    row([{ kind: 'link', href: 'https://example.com/a', text: 'https://example.com/a' }]);
    const a = screen.getByRole('link', { name: 'https://example.com/a' });
    expect(a).toHaveAttribute('href', 'https://example.com/a');
    expect(a).toHaveAttribute('target', '_blank');
    expect(a).toHaveAttribute('rel', 'noopener noreferrer nofollow');
    expect(a).toHaveClass('d-link');
  });

  it('defends itself: a link part with any other scheme renders as plain text, never an anchor', () => {
    const { container } = row([
      { kind: 'link', href: 'javascript:alert(1)', text: 'javascript:alert(1)' },
      { kind: 'link', href: 'data:text/html,x', text: 'data' },
      { kind: 'link', href: ' https://example.com', text: 'padded' },
      { kind: 'link', href: 'HTTPS://example.com/', text: 'upper' },
    ]);
    expect(container.querySelector('a[href^="javascript"], a[href^="data"]')).toBeNull();
    expect(container.querySelectorAll('a')).toHaveLength(1);
    expect(container.querySelector('.d-message-row__body')?.textContent).toBe('javascript:alert(1)datapaddedHTTPS://example.com/');
  });

  it('puts a link on the roving tab stop like the pills: not a stop unless the row is active', () => {
    const { rerender } = row([{ kind: 'link', href: 'https://example.com/', text: 'https://example.com/' }]);
    expect(screen.getByRole('link').tabIndex).toBe(-1);
    rerender(<MessageRow author="ada" time="21:02" state="ok" active body={[{ kind: 'link', href: 'https://example.com/', text: 'https://example.com/' }]} />);
    // Not a stop until focus is inside the row (same rule as the author name and pills).
    expect(screen.getByRole('link').tabIndex).toBe(-1);
    act(() => { screen.getByRole('article').focus(); });
    expect(screen.getByRole('link').tabIndex).toBe(0);
  });

  it('has no serious axe violations with every kind', async () => {
    const { container } = row([
      { kind: 'strong', children: [text('s')] }, text(' '), { kind: 'em', children: [text('e')] }, text(' '), { kind: 'strike', children: [text('x')] },
      text(' '), { kind: 'code', text: 'c' }, text(' '), { kind: 'link', href: 'https://example.com/', text: 'https://example.com/' },
      { kind: 'quote', children: [text('q')] }, { kind: 'codeblock', text: 'b' },
    ]);
    await expectNoAxeViolations(container);
  });
});

describe('MessageRow markdown hardening (fix round 1)', () => {
  const css = () => readFileSync(`${process.cwd()}/src/MessageRow/MessageRow.css`, 'utf8');
  it('F1: a link is a left-to-right isolate and shows exactly its href, whatever its text part says', () => {
    row([text('‮ '), { kind: 'link', href: 'https://evil.example/?moc.elgoog//:sptth', text: 'something else' }]);
    const a = screen.getByRole('link');
    expect(a).toHaveAttribute('dir', 'ltr');
    expect(a.textContent).toBe('https://evil.example/?moc.elgoog//:sptth');
  });
  it('F1: the stylesheet isolates the anchor', () => {
    expect(/\.d-message-row__body \.d-link \{([^}]*)\}/.exec(css())?.[1] ?? '').toMatch(/unicode-bidi: isolate/);
  });
  it('F3: a code span cannot be mistaken for a mention pill', () => {
    const code = /\.d-message-row__code \{([^}]*)\}/.exec(css())?.[1] ?? '';
    const pill = /\.d-mention \{([^}]*)\}/.exec(css())?.[1] ?? '';
    expect(code).toMatch(/border: 1px solid var\(--hairline-2\)/);
    expect(code).toMatch(/background: transparent/);
    expect(code).toMatch(/border-radius: 0/);
    expect(pill).toMatch(/background: var\(--surface-hi\)/);
    expect(pill).not.toMatch(/border:/);
  });
  it('F4: a code block with a language shows it', () => {
    const { container } = row([{ kind: 'codeblock', text: 'x', lang: 'ts' }]);
    expect(container.querySelector('.d-code-block')).toHaveAttribute('data-lang', 'ts');
    expect(container.querySelector('.d-code-block__lang')?.textContent).toBe('ts');
  });
  // Task 7, Ruling AF (README §6.4): the code card, one line element per line, numbers generated by CSS (never in the text).
  it('renders a fenced block as the code card, a line per line, between its text runs as paragraphs', () => {
    const { container } = row([text('see:'), { kind: 'codeblock', text: 'a: 1\nb: 2', lang: 'yaml' }, text('done')]);
    const body = container.querySelector('.d-message-row__body') as HTMLElement;
    expect(body).toHaveAttribute('data-code', 'true');
    expect(Array.from(body.children).map(c => c.className)).toEqual(['d-message-row__text', 'd-code-block', 'd-message-row__text']);
    expect(Array.from(body.querySelectorAll('.d-code-block__line')).map(l => l.textContent)).toEqual(['a: 1', 'b: 2']);
    expect(body.querySelector('.d-code-block [data-tok="key"]')?.textContent).toBe('a');
  });
  it('a mailto link with a query is not an anchor', () => {
    const { container } = row([{ kind: 'link', href: 'mailto:a@b.example?attach=/etc/passwd', text: 'x' }]);
    expect(container.querySelector('a')).toBeNull();
  });
});
