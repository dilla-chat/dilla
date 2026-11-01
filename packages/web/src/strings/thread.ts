// Flow 11 (docs/design/flows/11-slow-mode-announcements-threads.md): threads copy. Keys: thread.*
export const thread = {
  'thread.open': 'reply in thread',
  'thread.summary.one': '1 reply',
  'thread.summary.other': '{count} replies',
  // Task 7 (Signal §5.5): the thread chip's latest reply time, after the count.
  'thread.summary.last': 'last {time}',
  'thread.title': 'Thread',
  'thread.close': 'close thread',
  'thread.composer': 'Reply in thread',
  'thread.list': 'threads',
  'thread.empty': 'No threads in #{name} yet.',
  'thread.loadFailed': 'Threads could not be loaded.',
  'thread.noReplies': 'No replies yet.',
  'thread.rootMissing': 'The message that started this thread cannot be shown here.',
} as const;
