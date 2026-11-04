/** Copy for the key comparison ceremony. */
export const privacy = {
  'gif.dm.title': 'GIFs in direct messages',
  'gif.dm.notice': 'GIF searches and picks in direct messages go through {server} and Giphy. They are not end-to-end encrypted.',
  'gif.dm.use': 'Use GIFs',
  'gif.dm.notNow': 'Not now',
  'gif.dm.saving': 'Saving…',
  'gif.dm.hint': 'Searches and picks go through a shared server and Giphy, outside end-to-end encryption. This choice applies to this account on this device. Turning it on asks for confirmation before using a new server.',

  'privacy.section': 'Privacy',
  'privacy.compare': 'Compare with {name}',
  'privacy.title': 'Compare safety numbers',
  'privacy.copy': 'Copy number',
  'privacy.verify': 'All 12 blocks match',
  'privacy.instructions': 'Open Privacy on both devices. Read the 12 blocks of five digits to each other in person or on a call you started yourself.',
  'privacy.keyDetails': 'public key',
  'privacy.showQr': 'show qr',
  'privacy.hideQr': 'hide qr',
  'privacy.qrLabel': 'safety number as a qr code',
  'privacy.confirm': 'Yes, all 12 blocks match',
  'privacy.firstSeen': 'This is the first key this device saw for {name}. The server could have shown a different one. Compare numbers to verify.',
  'privacy.firstSeenLabel': 'First seen here — not verified',
  'privacy.verifiedLabel': 'verified',
  'privacy.changed': 'This contact’s identity changed. Compare the number before sending.',
  'privacy.changedNoKey': 'This contact’s identity changed, but no device list shows the new key, so there is nothing to compare yet. Sending is paused where their changed device is a member until it’s removed.',
  'privacy.unlisted': 'This device is not on the contact’s list.',
  // A message held because its device is not on its sender's signed device list (E_DEVICE_UNLISTED): the sender adding
  // the device to their account is what makes it readable, so the row says that, not only the state. It opens at this
  // reader's next ready (also an epoch change of the group, a verify of the contact or a send the verdict refuses), when
  // the worker reads the sender's list again and the core releases the held row.
  'privacy.unlistedMessage': 'Sent from a device that is not on the sender’s device list. Ask them to add it to their account; the message opens once they do.',
  // A message from a device its sender removed (E_DEVICE_REVOKED): final in the core, so nothing is asked of anyone.
  'privacy.revokedMessage': 'Sent from a device the sender removed from their account. It will not open.',
  'privacy.leafFailed': 'A device in this conversation failed the identity check. Sending is paused here until it’s removed.',
  'privacy.noShared': 'Open a conversation with this person to compare numbers.',
  'privacy.removed': 'You can send in this conversation after this member leaves. Their changed identity is still unverified elsewhere.',
  'privacy.ownChanged': 'A device using your account under a different identity is in some of your conversations. Sending is paused there. Open Devices and remove any device you don’t recognise.',
  'privacy.ownUnlisted': 'A device that is not on your device list is using your account in some conversations. Sending is paused there. Remove it in Devices.',
  'privacy.ownAction': 'Open devices',
  'privacy.loading': 'Working out the number…',
  'privacy.loadFailed': 'The number could not be worked out. Close this and try again.',
  'privacy.keyMoved': 'The key changed while you compared. Close this and compare the new number.',
  'privacy.verifyFailed': 'This could not be marked as verified. Close this and compare again.',
} as const;
