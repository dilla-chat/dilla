import { useEffect, useMemo, useRef, useState } from 'react';
import { Icon } from '../Icon/Icon.tsx';
import { highlight } from './highlight.ts';
import './CodeBlock.css';

export interface CodeBlockLabels {
  /** The copy button's text, and "Copied" for a moment after a copy. */
  copy: string; copied: string;
  /** The header's line count ("9 lines"). */
  lines(n: number): string;
}

export interface CodeBlockProps {
  /** The fence's language word; none shows as `text`. */
  lang?: string;
  code: string;
  /** Without labels the header shows the language only (no count, no copy). */
  labels?: CodeBlockLabels;
  /** The copy button is a stop only while the message row is the log's active row. */
  tabIndex?: number;
}

/** How long the button says "Copied" (the prototype's 1.6s). */
export const COPIED_MS = 1600;

/**
 * A fenced code block in a message (README §6.4, the prototype's card): the language and line count over a numbered,
 * highlighted body that scrolls sideways and never wraps. Copy writes the code to the clipboard and says "Copied" for a
 * moment; that text is a polite status. Every token is a text child, never HTML.
 */
export function CodeBlock({ lang, code, labels, tabIndex }: CodeBlockProps) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | null>(null);
  useEffect(() => () => { if (timer.current !== null) window.clearTimeout(timer.current); }, []);
  const rows = useMemo(() => highlight(lang, code), [lang, code]);
  const copy = () => {
    void navigator.clipboard?.writeText(code).catch(() => undefined);
    setCopied(true);
    if (timer.current !== null) window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => { setCopied(false); timer.current = null; }, COPIED_MS);
  };
  return (
    <div className="d-code-block" data-lang={lang ?? 'text'}>
      <div className="d-code-block__head">
        <span className="d-code-block__lang">{lang ?? 'text'}</span>
        {labels !== undefined ? <span className="d-code-block__count">{labels.lines(rows.length)}</span> : null}
        {labels !== undefined ? (
          <button type="button" className="d-code-block__copy" tabIndex={tabIndex} onClick={copy} data-copied={copied ? 'true' : undefined}>
            <Icon name={copied ? 'check' : 'copy'} size={12} />{copied ? labels.copied : labels.copy}
          </button>
        ) : null}
        {labels !== undefined ? <span className="d-sr-only" role="status">{copied ? labels.copied : ''}</span> : null}
      </div>
      <pre className="d-code-block__body"><code>{rows.map((toks, i) => (
        <span key={i} className="d-code-block__line">
          <span className="d-code-block__src">{toks.map((t, j) => <span key={j} data-tok={t.kind === 'text' ? undefined : t.kind}>{t.text}</span>)}</span>
        </span>
      ))}</code></pre>
    </div>
  );
}
