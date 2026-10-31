import { useLayoutEffect, useRef } from 'react';
import { Dialog } from '../Dialog/Dialog.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { graphemeInitials } from '../initials.ts';
import './PollDialog.css';
export interface PollDraft { question: string; answers: string[]; duration: '1h' | '4h' | '8h' | '24h' | '3d' | '7d'; multi: boolean }
export interface PollDialogLabels {
  title: string;
  titleWithoutChannel: string;
  close: string;
  cancel: string;
  post: string;
  question: string;
  questionPlaceholder: string;
  counter: string;
  answers: string;
  answerRange: string;
  answerLabel: string;
  removeAnswer: string;
  addAnswer: string;
  duration: string;
  multi: string;
  footer: string;
  answerPlaceholders: readonly [string, string, string];
  durations: Readonly<Record<PollDraft['duration'], string>>;
}
export interface PollDialogProps { open: boolean; channelName: string; bot: { name: string }; value: PollDraft; error: string | null;
  labels: PollDialogLabels; canPost: boolean; onChange(next: PollDraft): void; onPost(): void; onCancel(): void }
export function PollDialog({ open, channelName, bot, value, error, labels, canPost, onChange, onPost, onCancel }: PollDialogProps) {
  const answerRefs = useRef<(HTMLInputElement | null)[]>([]); const removedFocus = useRef<number | null>(null);
  useLayoutEffect(() => { if (removedFocus.current !== null) { answerRefs.current[removedFocus.current]?.focus(); removedFocus.current = null; } }, [value.answers.length]);
  const durations = ['1h', '4h', '8h', '24h', '3d', '7d'] as const;
  const count = Array.from(value.question).length;
  return <div className="d-poll-dialog"><Dialog open={open} title={channelName ? labels.title.replace('{channel}', channelName) : labels.titleWithoutChannel} showDefaultClose={false} onClose={onCancel} closeLabel={labels.close}
    lead={<><span className="d-poll-dialog__bot" title={bot.name}>{graphemeInitials(bot.name)}</span><button className="d-poll-dialog__close" type="button" aria-label={labels.close} onClick={onCancel}><Icon name="x" size={15} /></button></>}
    footer={<><span className="d-poll-dialog__footer">{labels.footer}</span><button type="button" onClick={onCancel}>{labels.cancel}</button><button className="d-poll-dialog__post" type="button" data-ready={canPost} onClick={onPost}>{labels.post}</button></>}>
    <label className="d-poll-dialog__question"><span>{labels.question}<span className="d-poll-dialog__counter" data-danger={count > 300}>{labels.counter.replace('{n}', String(count))}</span></span>
      <input aria-label={labels.question} data-autofocus value={value.question} onChange={e => onChange({ ...value, question: e.currentTarget.value })} placeholder={labels.questionPlaceholder} /></label>
    <div className="d-poll-dialog__answers"><div className="d-poll-dialog__label">{labels.answers}<span>{labels.answerRange}</span></div>
      {value.answers.map((answer, index) => <div className="d-poll-dialog__answer" key={index}><span>{index + 1}</span><input ref={node => { answerRefs.current[index] = node; }} data-danger={Array.from(answer).length > 100} value={answer}
        aria-label={labels.answerLabel.replace('{number}', String(index + 1))} placeholder={labels.answerPlaceholders[Math.min(index, 2)]}
        onChange={e => onChange({ ...value, answers: value.answers.map((a, i) => i === index ? e.currentTarget.value : a) })} />
        <button type="button" disabled={value.answers.length <= 2} aria-label={labels.removeAnswer.replace('{number}', String(index + 1))} onClick={() => { if (value.answers.length > 2) { removedFocus.current = Math.max(0, index - 1); onChange({ ...value, answers: value.answers.filter((_, i) => i !== index) }); } }}><Icon name="x" size={14} /></button></div>)}
      {value.answers.length < 10 && <button type="button" className="d-poll-dialog__add" onClick={() => onChange({ ...value, answers: [...value.answers, ''] })}><Icon name="plus" size={13} />{labels.addAnswer}</button>}
    </div>
    <div className="d-poll-dialog__settings"><div className="d-poll-dialog__duration"><span>{labels.duration}</span><div role="radiogroup" aria-label={labels.duration}>{durations.map((duration, index) => <button type="button" role="radio" key={duration} aria-checked={value.duration === duration} tabIndex={value.duration === duration ? 0 : -1} onKeyDown={event => {
        const next = event.key === 'ArrowRight' || event.key === 'ArrowDown' ? (index + 1) % durations.length
          : event.key === 'ArrowLeft' || event.key === 'ArrowUp' ? (index + durations.length - 1) % durations.length
          : event.key === 'Home' ? 0 : event.key === 'End' ? durations.length - 1 : null;
        if (next !== null) { event.preventDefault(); onChange({ ...value, duration: durations[next] }); (event.currentTarget.parentElement?.querySelectorAll<HTMLButtonElement>('[role="radio"]')[next])?.focus(); }
      }} onClick={() => onChange({ ...value, duration })}>{labels.durations[duration]}</button>)}</div></div>
      <button className="d-poll-dialog__multi" type="button" role="switch" aria-checked={value.multi} onClick={() => onChange({ ...value, multi: !value.multi })}><span><span /></span>{labels.multi}</button></div>
    {error && <div className="d-poll-dialog__error" role="alert"><Icon name="triangle-alert" size={13} />{error}</div>}
  </Dialog></div>;
}
