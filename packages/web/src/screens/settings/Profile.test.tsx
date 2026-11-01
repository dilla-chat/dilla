import { describe, it, expect } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { account, ME } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { t } from '../../strings/index.ts';
import { Profile } from './Profile.tsx';

function setup(user: { display?: string; statusMsg?: string; bio?: string } = {}) {
  const fake = new FakeClient();
  fake.set('account', account({ user: { ...ME, display: 'Ada', ...user } }));
  const u = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><Profile /></CoreProvider></div>);
  return { fake, u, view };
}
const display = () => screen.getByRole('textbox', { name: t('profile.display.label') });
const status = () => screen.getByRole('textbox', { name: t('profile.status.label') });
const bio = () => screen.getByRole('textbox', { name: t('profile.bio.label') });
// Task 11 (Jonas, Q7): the profile saves as you type (700 ms after the last change); Enter in a field saves at once.
const saved = (fake: FakeClient, calls: unknown[]) => waitFor(() => expect(fake.calls).toEqual(calls), { timeout: 3000 });
const notSaved = async (fake: FakeClient) => { await new Promise(r => setTimeout(r, 900)); expect(fake.calls).toEqual([]); };

describe('Settings → Profile', () => {
  it('shows the current display name, status message and bio, and says the server can read them', () => {
    setup({ statusMsg: 'on a call', bio: 'one\ntwo' });
    expect(screen.getByRole('heading', { level: 2, name: t('profile.title') })).toBeInTheDocument();
    expect(display()).toHaveValue('Ada');
    expect(status()).toHaveValue('on a call');
    expect(bio()).toHaveValue('one\ntwo');
    expect(bio().tagName).toBe('TEXTAREA');
    expect(screen.getByText(t('profile.body'))).toBeInTheDocument();
  });

  it('ends the form with the prototype\'s note that the host can read name, status and bio, and shows a preview card', () => {
    setup({ statusMsg: 'packing rope', bio: 'Navigator.' });
    expect(screen.getByText(t('profile.footnote'))).toBeInTheDocument();
    const card = screen.getByRole('group', { name: t('profile.preview') });
    expect(card).toHaveTextContent('Ada');
    expect(card).toHaveTextContent('@ada · packing rope');
    expect(card).toHaveTextContent('Navigator.');
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();
  });

  it('falls back to the username while the display name is not known yet', () => {
    const fake = new FakeClient();
    fake.set('account', account());
    render(<CoreProvider client={fake}><Profile /></CoreProvider>);
    expect(display()).toHaveValue('ada');
  });

  it('saves only the changed fields once typing stops, and says so', async () => {
    const { fake, u } = setup({ statusMsg: 'old' });
    await u.clear(status());
    await u.type(status(), 'new status');
    await u.type(bio(), 'hello{Enter}there');
    await saved(fake, [{ m: 'updateProfile', statusMsg: 'new status', bio: 'hello\nthere' }]);
    expect(await screen.findByRole('status')).toHaveTextContent(t('profile.saved'));
  });

  it('clears a field by sending the empty string', async () => {
    const { fake, u } = setup({ statusMsg: 'old' });
    await u.clear(status());
    await saved(fake, [{ m: 'updateProfile', statusMsg: '' }]);
  });

  it('will not save an empty display name', async () => {
    const { fake, u } = setup();
    await u.clear(display());
    await u.type(display(), '{Enter}');
    expect(fake.calls).toEqual([]);
    expect(screen.getByRole('alert')).toHaveTextContent(t('profile.error.display'));
    await notSaved(fake);
  });

  it('bounds each field to what the server accepts, and the status message is one line', () => {
    setup();
    expect(display()).toHaveAttribute('maxlength', '64');
    expect(status()).toHaveAttribute('maxlength', '190');
    expect(bio()).toHaveAttribute('maxlength', '500');
    expect(status().tagName).toBe('INPUT');
  });

  it('clears the saved word once something changes again', async () => {
    const { fake, u } = setup();
    await u.type(status(), 'x');
    await saved(fake, [{ m: 'updateProfile', statusMsg: 'x' }]);
    expect(await screen.findByRole('status')).toHaveTextContent(t('profile.saved'));
    await u.type(status(), 'y');
    expect(screen.getByRole('status')).toHaveTextContent('');
  });

  it('sends the text trimmed', async () => {
    const { fake, u } = setup();
    await u.type(status(), '  hello  ');
    await saved(fake, [{ m: 'updateProfile', statusMsg: 'hello' }]);
  });

  it('names the problem next to the field, without "try again", and does not send', async () => {
    const { fake, u } = setup();
    // a pasted tab or invisible character, a pasted text over the limit, and whitespace only
    fireEvent.change(bio(), { target: { value: `a${String.fromCodePoint(0x09)}b` } });
    expect(screen.getByRole('alert')).toHaveTextContent(t('profile.error.chars'));
    fireEvent.change(bio(), { target: { value: `a${String.fromCodePoint(0x202e)}b` } });
    expect(screen.getByRole('alert')).toHaveTextContent(t('profile.error.chars'));
    fireEvent.change(bio(), { target: { value: 'x'.repeat(501) } });
    expect(screen.getByRole('alert')).toHaveTextContent(t('profile.error.long'));
    fireEvent.change(bio(), { target: { value: '   ' } });
    expect(screen.getByRole('alert')).toHaveTextContent(t('profile.error.blank'));
    expect(bio()).toHaveAccessibleDescription(expect.stringContaining(t('profile.error.blank')));
    expect(bio()).toBeInvalid();
    fireEvent.change(status(), { target: { value: 'y'.repeat(191) } });
    expect(screen.getAllByRole('alert').map(a => a.textContent).join(' ')).not.toMatch(/try again/i);
    await notSaved(fake);
    fireEvent.change(bio(), { target: { value: 'fine' } });
    fireEvent.change(status(), { target: { value: 'fine' } });
    expect(screen.queryByRole('alert')).toBeNull();
    await saved(fake, [{ m: 'updateProfile', statusMsg: 'fine', bio: 'fine' }]);
    void u;
  });

  it('shows a refusal of the text by the server as a text problem, not as "try again"', async () => {
    const { fake, u } = setup();
    fake.handler = () => Promise.reject(refusal({ code: 'E_INVALID_REQUEST', status: 400 }));
    await u.type(bio(), 'text');
    const alert = await screen.findByRole('alert', {}, { timeout: 3000 });
    expect(alert).toHaveTextContent(t('profile.error.chars'));
    expect(alert.textContent).not.toMatch(/try again/i);
    fake.handler = () => Promise.reject(refusal({ code: 'E_BAD_INPUT', status: 0 }));
    await u.type(bio(), 's');
    expect(await screen.findByRole('alert', {}, { timeout: 3000 })).toHaveTextContent(t('profile.error.chars'));
  });

  it('does not validate a stored display name nobody changed, so status and bio still save', async () => {
    const legacy = `Ada${String.fromCodePoint(0x200b)}L`;
    const { fake, u } = setup({ display: legacy });
    expect(screen.queryByRole('alert')).toBeNull();
    await u.type(status(), 'still works');
    await saved(fake, [{ m: 'updateProfile', statusMsg: 'still works' }]);
    // editing the name checks it, and putting the same name back is not an edit
    fireEvent.change(display(), { target: { value: `${legacy}x` } });
    expect(screen.getByRole('alert')).toHaveTextContent(t('profile.error.chars'));
    fireEvent.change(display(), { target: { value: legacy } });
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('an account with an empty display name can still save a status or a bio', async () => {
    const fake = new FakeClient();
    fake.set('account', account({ user: { ...ME, display: '' } }));
    const u = userEvent.setup();
    render(<div className="d-root"><CoreProvider client={fake}><Profile /></CoreProvider></div>);
    // the username shows while the display name is empty, and nothing blocks the other fields
    await u.type(bio(), 'hello');
    await saved(fake, [{ m: 'updateProfile', bio: 'hello' }]);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('checks exactly what it sends: a stored name that is not trimmed is not validated or sent with a status-only save', async () => {
    const legacy = `  Ada${String.fromCodePoint(0x200b)}L  `;
    const { fake, u } = setup({ display: legacy });
    expect(screen.queryByRole('alert')).toBeNull();
    await u.type(status(), 'x');
    await saved(fake, [{ m: 'updateProfile', statusMsg: 'x' }]);
  });

  it('keeps what was typed when the account changes under the form, and follows it for a field left alone', async () => {
    const { fake, u } = setup({ statusMsg: 'old' });
    await u.type(bio(), 'my draft');
    fake.set('account', account({ user: { ...ME, display: 'Ada', statusMsg: 'from device two', bio: 'bio from device two' } }));
    await waitFor(() => expect(status()).toHaveValue('from device two'));
    // the bio was being edited: the draft stays
    expect(bio()).toHaveValue('my draft');
  });

  it('says so when the save is refused, keeps what was typed, and lets it be tried again', async () => {
    const { fake, u } = setup();
    fake.handler = () => Promise.reject(refusal({ code: 'E_NETWORK', status: 0 }));
    await u.type(bio(), 'text');
    expect(await screen.findByRole('alert', {}, { timeout: 3000 })).toHaveTextContent(t('profile.error.save'));
    expect(bio()).toHaveValue('text');
    // A failed save waits for the next change (or Enter) instead of retrying on its own.
    fake.handler = () => Promise.resolve(null);
    await u.type(status(), '{Enter}');
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull(), { timeout: 3000 });
    expect(await screen.findByRole('status', {}, { timeout: 3000 })).toHaveTextContent(t('profile.saved'));
  });

  it('tells a rate-limited save to wait', async () => {
    const { fake, u } = setup();
    fake.handler = () => Promise.reject(refusal({ code: 'E_RATE_LIMITED', status: 429, retryAfterMs: 2000 }));
    await u.type(bio(), 'text');
    expect(await screen.findByRole('alert', {}, { timeout: 3000 })).toHaveTextContent(t('profile.error.wait'));
  });

  it('follows the account when it changes on another device and nothing is being edited', () => {
    const { fake } = setup();
    fake.set('account', account({ user: { ...ME, display: 'Ada L', statusMsg: 'away' } }));
    return waitFor(() => { expect(display()).toHaveValue('Ada L'); expect(status()).toHaveValue('away'); });
  });

  // Fix round 2: Enter, a save in flight, and leaving.
  it('saves at once on Enter in a one-line field, without waiting for the pause', async () => {
    const { fake } = setup();
    fireEvent.change(status(), { target: { value: 'on call' } });
    fireEvent.keyDown(status(), { key: 'Enter' });
    await waitFor(() => expect(fake.calls).toEqual([{ m: 'updateProfile', statusMsg: 'on call' }]), { timeout: 300 });
  });

  it('keeps what is typed while a save is in flight and sends it once that save lands', async () => {
    const { fake } = setup();
    let land!: () => void;
    fake.handler = () => new Promise(resolve => { land = () => resolve(null); });
    fireEvent.change(status(), { target: { value: 'on' } });
    fireEvent.keyDown(status(), { key: 'Enter' });
    await waitFor(() => expect(fake.calls).toHaveLength(1));
    fireEvent.change(status(), { target: { value: 'on call' } });
    fireEvent.keyDown(status(), { key: 'Enter' });
    await new Promise(r => setTimeout(r, 50));
    expect(fake.calls, 'one save at a time').toHaveLength(1);
    fake.handler = () => Promise.resolve(null);
    land();
    await saved(fake, [{ m: 'updateProfile', statusMsg: 'on' }, { m: 'updateProfile', statusMsg: 'on call' }]);
  });

  it('sends a change still waiting for the pause when the section goes, after the save in flight', async () => {
    const { fake, view } = setup();
    let land!: () => void;
    fake.handler = () => new Promise(resolve => { land = () => resolve(null); });
    fireEvent.change(bio(), { target: { value: 'first' } });
    fireEvent.keyDown(status(), { key: 'Enter' });
    await waitFor(() => expect(fake.calls).toHaveLength(1));
    fireEvent.change(bio(), { target: { value: 'first and more' } });
    view.unmount();
    fake.handler = () => Promise.resolve(null);
    land();
    await saved(fake, [{ m: 'updateProfile', bio: 'first' }, { m: 'updateProfile', bio: 'first and more' }]);
  });

  it('has no serious axe violations', async () => {
    const { view } = setup({ statusMsg: 'x', bio: 'y' });
    await expectNoAxeViolations(view.container);
  });
});
