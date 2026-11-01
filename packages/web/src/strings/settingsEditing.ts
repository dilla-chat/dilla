/** Copy for the shared unsaved-settings guard. The primary string catalog may re-export these keys. */
export const settingsEditing = {
  save: 'Save',
  reset: 'Reset',
  unsaved: 'Unsaved changes',
  saveFailed: 'Changes could not be saved. Try again.',
  leaveTitle: 'Discard unsaved changes?',
  leaveBody: 'You have unsaved changes. Leaving now will discard them.',
  leaveBusyBody: 'A request is already in progress. It will continue if you leave. Unsaved changes will be discarded.',
  keepEditing: 'Keep editing',
  discard: 'Discard changes',
} as const;
