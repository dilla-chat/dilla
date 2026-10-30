import { Dialog } from '../Dialog/Dialog.tsx';
import { Icon } from '../Icon/Icon.tsx';
// The task's exact prop contract has no copy prop. Keep the pure catalog as the single source of poll copy.
import { utility as copy } from '../../../web/src/strings/utility.ts';
import './PollDialog.css';
export interface PollDraft { question: string; answers: string[]; duration: '1h' | '4h' | '8h' | '24h' | '3d' | '7d'; multi: boolean }
export interface PollDialogProps { open: boolean; channelName: string; bot: { name: string }; value: PollDraft; error: string | null;
  canPost: boolean; onChange(next: PollDraft): void; onPost(): void; onCancel(): void }
export function PollDialog({ open, channelName, bot, value, error, canPost, onChange, onPost, onCancel }: PollDialogProps) {
  const count = Array.from(value.question).length;
  return <div className="d-poll-dialog"><Dialog open={open} title={copy['poll.title'].replace('{channel}', channelName)} onClose={onCancel} closeLabel={copy['poll.dismiss']}
    lead={<><span className="d-poll-dialog__bot" title={bot.name}>{bot.name.slice(0, 2).toUpperCase()}</span><button className="d-poll-dialog__close" type="button" aria-label={copy['poll.dismiss']} onClick={onCancel}><Icon name="x" size={15} /></button></>}
    footer={<><span className="d-poll-dialog__footer">{copy['poll.footer']}</span><button type="button" onClick={onCancel}>{copy['poll.cancel']}</button><button className="d-poll-dialog__post" type="button" data-ready={canPost} onClick={onPost}>{copy['poll.post']}</button></>}>
    <label className="d-poll-dialog__question"><span>{copy['poll.question']}<span className="d-poll-dialog__counter" data-danger={count > 300}>{copy['poll.counter'].replace('{n}', String(count))}</span></span>
      <input aria-label={copy['poll.question']} data-autofocus value={value.question} onChange={e => onChange({ ...value, question: e.currentTarget.value })} placeholder={copy['poll.questionPlaceholder']} /></label>
    <div className="d-poll-dialog__answers"><div className="d-poll-dialog__label">{copy['poll.answers']}<span>{copy['poll.answerRange']}</span></div>
      {value.answers.map((answer, index) => <div className="d-poll-dialog__answer" key={index}><span>{index + 1}</span><input data-danger={Array.from(answer).length > 100} value={answer}
        aria-label={copy['poll.answerLabel'].replace('{number}', String(index + 1))} placeholder={copy[index === 0 ? 'poll.answerFirst' : index === 1 ? 'poll.answerSecond' : 'poll.answerAnother']}
        onChange={e => onChange({ ...value, answers: value.answers.map((a, i) => i === index ? e.currentTarget.value : a) })} />
        <button type="button" disabled={value.answers.length <= 2} aria-label={copy['poll.removeAnswer'].replace('{number}', String(index + 1))} onClick={() => { if (value.answers.length > 2) onChange({ ...value, answers: value.answers.filter((_, i) => i !== index) }); }}><Icon name="x" size={14} /></button></div>)}
      {value.answers.length < 10 && <button type="button" className="d-poll-dialog__add" onClick={() => onChange({ ...value, answers: [...value.answers, ''] })}><Icon name="plus" size={13} />{copy['poll.addAnswer']}</button>}
    </div>
    <div className="d-poll-dialog__settings"><div className="d-poll-dialog__duration"><span>{copy['poll.duration']}</span><div role="group" aria-label={copy['poll.duration']}>{(['1h', '4h', '8h', '24h', '3d', '7d'] as const).map(duration => <button type="button" key={duration} aria-pressed={value.duration === duration} onClick={() => onChange({ ...value, duration })}>{copy[`poll.duration.${duration}`]}</button>)}</div></div>
      <button className="d-poll-dialog__multi" type="button" role="switch" aria-checked={value.multi} onClick={() => onChange({ ...value, multi: !value.multi })}><span><span /></span>{copy['poll.multiple']}</button></div>
    {error && <div className="d-poll-dialog__error" role="alert"><Icon name="triangle-alert" size={13} />{error}</div>}
  </Dialog></div>;
}
