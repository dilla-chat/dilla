import { Fragment, useId } from 'react';
import { Button } from '../Button/Button.tsx';
import './CodeList.css';

export interface CodeListProps {
  codes: readonly string[];
  /** Optional in-memory disclosure; collapsed codes are absent from the DOM. */
  disclosure?: { expanded: boolean; onToggle(): void; count: string; show: string; hide: string };
  label: string;
  /** Receives the codes exactly as shown, one per line, in the form client-core's normaliseBackupCode accepts. */
  onCopy(text: string): void;
  onPrint(): void;
  labels: { copy: string; print: string };
}

const GROUP = 5;

/** The separators dropped and the case raised, as client-core's normaliseBackupCode does. */
function normalise(code: string): string {
  return code.replace(/[-\s]/g, '').toUpperCase();
}

/** "JBSWY3DPEHPK3PXPJBSWY3DPEH" becomes ["JBSWY-", "3DPEH-", …, "H"]: groups of five, each but the last ending in a hyphen. */
function groups(code: string): string[] {
  const n = normalise(code);
  const out: string[] = [];
  for (let i = 0; i < n.length; i += GROUP) out.push(n.slice(i, i + GROUP) + (i + GROUP < n.length ? '-' : ''));
  return out;
}

/**
 * One-time codes as selectable text in a real ordered list. Each code is shown once, normalised and grouped in
 * fives joined by hyphens (the server's own spelling). A group never breaks inside; a code that does not fit on
 * a line wraps between groups. The columns are as many as fit, so a code never overflows its cell.
 */
export function CodeList({ codes, label, onCopy, onPrint, labels, disclosure }: CodeListProps) {
  const labelId = useId();
  const listId = useId();
  const shown = codes.map(c => groups(c));
  const actions = (
    <div className="d-code-list__actions">
      <Button onClick={() => { onCopy(shown.map(gs => gs.join('')).join('\n')); }}>{labels.copy}</Button>
      <Button onClick={onPrint}>{labels.print}</Button>
      {disclosure ? <Button aria-expanded={disclosure.expanded} aria-controls={listId} onClick={disclosure.onToggle}>
        {disclosure.expanded ? disclosure.hide : disclosure.show}
      </Button> : null}
    </div>
  );
  return (
    <div className="d-code-list">
      {disclosure ? <div className="d-code-list__summary"><span>{disclosure.count}</span>{actions}</div> : null}
      {!disclosure || disclosure.expanded ? <>
        <p id={labelId} className="d-code-list__label d-label">{label}</p>
        <ol id={listId} role="list" className="d-code-list__grid" aria-labelledby={labelId}>
          {shown.map((gs, i) => (
            <li key={i} className="d-code-list__code">
              <span className="d-code-list__text">
                {gs.map((g, j) => (
                  <Fragment key={j}>
                    {j > 0 ? <wbr /> : null}
                    <span className="d-code-list__group">{g}</span>
                  </Fragment>
                ))}
              </span>
            </li>
          ))}
        </ol>
      </> : null}
      {disclosure ? null : actions}
    </div>
  );
}
