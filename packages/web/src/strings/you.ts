// Flow 12 (docs/design/flows/12-phone-shell.md): you copy. Keys: you.*
// Task 11 (Jonas, Q3): the prototype's You: who you are, one box with the connection and this device, and the rows
// Settings, Status and Sign out. The full facts (node, link, generation, devices) are Settings → Diagnostics.
export const you = {
  'you.title': 'You',
  'you.line': '@{handle} · {status}',
  'you.connected': 'Connected to {host} · {ms} ms',
  'you.connectedNoPing': 'Connected to {host}',
  'you.connecting': 'Connecting to {host}…',
  'you.offline': 'Offline · reconnecting to {host}',
  'you.device': '{browser} on {os} · device {id}',
  'you.deviceUnknown': 'This browser · device {id}',
  'you.settings': 'Settings',
  'you.statusRow': 'Status',
} as const;
