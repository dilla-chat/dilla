import { createPasskey, getPasskey } from './webauthn.ts';

const button = document.querySelector<HTMLButtonElement>('#continue');
const status = document.querySelector<HTMLElement>('#status');
if (button && status) {
  const ceremony = location.pathname.replace(/\/$/, '');
  const id = ceremony.split('/').at(-1) ?? '';
  if (new URLSearchParams(location.search).get('done') === '1') {
    button.hidden = true;
    status.textContent = 'Return to your Dilla desktop app.';
  }
  button.onclick = async () => {
    button.disabled = true;
    try {
      const response = await fetch(`${ceremony}/options`, { credentials: 'same-origin', cache: 'no-store' });
      if (!response.ok) throw new Error('This request expired. Start again in Dilla.');
      const request = await response.json() as { kind: string; options: string };
      if (request.kind === 'oidc') {
        const base = ceremony.slice(0, ceremony.lastIndexOf('/desktop-auth/'));
        const launch = await fetch(`${ceremony}/oidc`, { method: 'POST', credentials: 'same-origin' });
        if (!launch.ok) throw new Error('This request expired. Start again in Dilla.');
        const { ticket } = await launch.json() as { ticket: string };
        location.assign(`${base}/v1/auth/oidc/start?desktop_request=${encodeURIComponent(id)}&desktop_ticket=${encodeURIComponent(ticket)}`);
        return;
      }
      let result: { response: string } | { error: string };
      try {
        if (request.kind !== 'get' && request.kind !== 'create') throw new Error('Unsupported request');
        result = { response: await (request.kind === 'create' ? createPasskey : getPasskey)(request.options) };
      } catch { result = { error: 'Passkey ceremony was cancelled or failed. Try again in Dilla.' }; }
      const finished = await fetch(`${ceremony}/finish`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin', body: JSON.stringify(result) });
      if (!finished.ok) throw new Error('This request expired. Start again in Dilla.');
      status.textContent = 'Return to your Dilla desktop app.';
      button.hidden = true;
    } catch (error) { status.textContent = error instanceof Error ? error.message : 'Unable to complete sign-in.'; }
  };
}
