import type { PollDialogLabels } from '@dilla/ui';
import { utility as s } from '../../strings/utility.ts';
export const pollDialogLabels: PollDialogLabels = {
  title: s['poll.title'],
  titleWithoutChannel: s['poll.titleWithoutChannel'],
  close: s['poll.dismiss'],
  cancel: s['poll.cancel'],
  post: s['poll.post'],
  question: s['poll.question'],
  questionPlaceholder: s['poll.questionPlaceholder'],
  counter: s['poll.counter'],
  answers: s['poll.answers'],
  answerRange: s['poll.answerRange'],
  answerLabel: s['poll.answerLabel'],
  removeAnswer: s['poll.removeAnswer'],
  addAnswer: s['poll.addAnswer'],
  duration: s['poll.duration'],
  multi: s['poll.multiple'],
  footer: s['poll.footer'],
  answerPlaceholders: [s['poll.answerFirst'], s['poll.answerSecond'], s['poll.answerAnother']],
  durations: { '1h': s['poll.duration.1h'], '4h': s['poll.duration.4h'], '8h': s['poll.duration.8h'], '24h': s['poll.duration.24h'], '3d': s['poll.duration.3d'], '7d': s['poll.duration.7d'] },
};
