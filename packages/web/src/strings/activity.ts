// Flow 12 (docs/design/flows/12-phone-shell.md): activity copy. Keys: activity.*
// Task 11 (Jonas, Q3): the prototype's Activity: one list of avatar rows, each "where" over "who text".
export const activity = {
  'activity.title': 'Activity',
  'activity.emptyTitle': 'Nothing new',
  'activity.empty': 'Nothing new. Mentions and direct messages show up here.',
  'activity.mention': 'mentioned you: {text}',
  'activity.mentionEmpty': 'mentioned you',
  'activity.dm': 'Direct message',
  'activity.where': '{server} · #{channel}',
  'activity.newCount': '{n} new',
  'activity.mentionOne': '1 mention',
  'activity.mentionMany': '{n} mentions',
  'activity.reminder': 'Reminder · {where}',
} as const;
