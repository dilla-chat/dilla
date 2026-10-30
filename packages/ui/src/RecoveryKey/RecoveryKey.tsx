import { useId } from 'react';
import { Button } from '../Button/Button.tsx';
import { Icon } from '../Icon/Icon.tsx';
import './RecoveryKey.css';

export interface RecoveryKeyProps {
  groups: readonly string[];
  label: string;
  acknowledgeLabel: string;
  acknowledged: boolean;
  onAcknowledge(value: boolean): void;
  printLabel: string;
  onPrint(): void;
}

/**
 * The recovery key as selectable text: one list item per group, numbered by a
 * CSS counter that is neither copied nor read. There is no copy control of any
 * kind (protocol/03 "Recovery"), and no handler interferes with selecting or
 * copying the text; the person may print it or write it down.
 */
export function RecoveryKey({ groups, label, acknowledgeLabel, acknowledged, onAcknowledge, printLabel, onPrint }: RecoveryKeyProps) {
  const labelId = useId();
  const ackId = useId();
  // Wave B task 3b, the prototype's key step: the grid (its name is for assistive technology only; the prototype draws no
  // label), the row of actions under it (Print stands where the prototype has Copy and Save as file: protocol/03 forbids a
  // copy control), then the 16px check box and its 12px label. The native checkbox covers the whole row, transparent, so
  // the row is its target; the drawn box follows its state.
  return (
    <div className="d-recovery-key">
      <p id={labelId} className="d-recovery-key__label d-sr-only">{label}</p>
      <ol role="list" className="d-recovery-key__grid" aria-labelledby={labelId}>
        {groups.map((g, i) => <li key={i} className="d-recovery-key__group">{g}</li>)}
      </ol>
      <div className="d-recovery-key__actions">
        <Button className="d-recovery-key__print" onClick={onPrint}><Icon name="printer" size={13} />{printLabel}</Button>
      </div>
      <div className="d-recovery-key__ack">
        <input id={ackId} type="checkbox" className="d-recovery-key__box" checked={acknowledged}
          onChange={e => onAcknowledge(e.currentTarget.checked)} />
        <span className="d-recovery-key__tick" aria-hidden="true"><Icon name="check" size={11} stroke={3} /></span>
        <label htmlFor={ackId}>{acknowledgeLabel}</label>
      </div>
    </div>
  );
}
