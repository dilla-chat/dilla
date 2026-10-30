// One saver for every file the page hands to the browser: an attachment and, since wave B task 3b, the recovery key's
// "Save as file". The anchor is in the document while it is clicked (Firefox needs that), and the object URL lives for 10 s
// after the click, because WebKit and some Firefox builds start a blob download asynchronously and fail on a URL revoked
// at once (task 3b security review, MEDIUM-1).
export const REVOKE_SAVED_AFTER_MS = 10_000;

export function saveBlob(blob: Blob, name: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  a.rel = 'noopener';
  document.body.append(a);
  try {
    a.click();
  } finally {
    a.remove();
    window.setTimeout(() => { URL.revokeObjectURL(url); }, REVOKE_SAVED_AFTER_MS);
  }
}
