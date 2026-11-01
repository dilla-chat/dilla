// Flow 09 (docs/design/flows/09-roles-and-private-channels.md): roles and private channels copy. Keys: serverNotify.*
export const serverNotify = {
  'serverNotify.title': 'Servers',
  'serverNotify.body': 'A server’s setting applies to its channels unless a channel has its own.',
  'serverNotify.mode.default': 'Default',
  'serverNotify.mode.all': 'All messages',
  'serverNotify.mode.mentions': 'Mentions',
  'serverNotify.mode.nothing': 'Nothing',
  'serverNotify.mute': 'Mute server',
  'serverNotify.suppressEveryone': 'Ignore @everyone and @here',
} as const;
