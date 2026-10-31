// web3.6 W2, wave B task 3b: the second factor as steps 4 (choice) and 5 (code) of 6 of the sign-up, and the backup codes on
// the done step, 6 (flow 10 state 1; flow 01).
import { afterEach, describe, it, expect, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent, { type UserEvent } from '@testing-library/user-event';
import type { AccountState, Command, FactorsState } from '@dilla/client-core';
import { CoreProvider } from '../core/context.tsx';
import { FakeClient, refusal } from '../test/fake-client.ts';
import { account } from '../test/fixtures.ts';
import { expectNoAxeViolations } from '../test/setup.ts';
import { en } from '../strings/index.ts';
import { liveRegions, watchRegion } from '../test/live-regions.ts';
import { Onboarding } from './Onboarding.tsx';
import { browser } from '../browser.ts';
import { PHONE_QUERY } from '../layout.ts';
import { COMPLETE_DELAY_MS } from '@dilla/ui';
// Wave B task 3 fix round 1: the code boxes send the sixth digit after the prototype's 350 ms.
const settleCode = () => act(async () => { await new Promise<void>((resolve) => { setTimeout(resolve, COMPLETE_DELAY_MS + 20); }); });


const KEY = ['7K2M', 'QX9D', 'H4TB', 'R8NW', 'C3VF', 'J6PZ', 'A1GE', 'Y5KS', 'M0QT', 'B7XH', 'W2DN', 'F9RC', 'P4ZA'];
const ACK = 'I saved my recovery key somewhere safe';
const CODES_ACK = 'I saved my backup codes somewhere safe';
const SECRET = 'JBSWY3DPEHPK3PXP';
const CODES = ['AAAAABBBBBCCCCCDDDDDEEEEEF', 'GGGGGHHHHHIIIIIJJJJJKKKKKL'];
const COMMUNITY = 'c4'.repeat(16);
const NO_FACTORS: FactorsState = { owed: true, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: true };

type Script = (c: Command, fake: FakeClient) => Promise<unknown>;
const factorPhase: Partial<AccountState> = { phase: 'factor-setup', factorSetup: { origin: 'signup' }, user: { id: 'bb'.repeat(16), username: 'ada' }, recoveryKey: null };

const base: Script = (c, fake) => {
  switch (c.m) {
    case 'signupBegin': fake.set('account', { ...fake.get('account')!, phase: 'signup-keys', recoveryKey: KEY }); return Promise.resolve(null);
    case 'signupSubmit':
      fake.set('account', { ...fake.get('account')!, ...factorPhase });
      fake.set('factors', NO_FACTORS);
      return Promise.resolve({ communityId: null, joinError: null, factorOwed: true });
    case 'factorTotpBegin': return Promise.resolve({ secret: SECRET, uri: `otpauth://totp/dilla.test:ada?secret=${SECRET}` });
    case 'factorTotpConfirm': return Promise.resolve({ backupCodes: CODES });
    case 'factorSetupDone':
      fake.set('account', account({ phase: 'ready' }));
      return Promise.resolve({ communityId: null, joinError: null });
    default: return Promise.resolve(null);
  }
};

function setup(script: Script = base, over: Partial<AccountState> = {}) {
  window.history.replaceState(null, '', '/');
  const fake = new FakeClient();
  fake.set('account', account({ phase: 'needs-signup', user: null, deviceId: null, ...over }));
  fake.handler = c => script(c, fake);
  const onFinish = vi.fn();
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><Onboarding onFinish={onFinish} /></CoreProvider></div>);
  return { fake, onFinish, user, view };
}
const h1 = () => screen.getByRole('heading', { level: 1 });
const button = (name: string) => screen.getByRole('button', { name });

async function toFactor(user: UserEvent, invite = 'ABCD-EFGH') {
  await user.type(screen.getByRole('textbox', { name: 'Invite link' }), invite);
  await user.click(button('Continue'));
  await user.type(screen.getByRole('textbox', { name: 'Username' }), 'ada');
  await user.click(button('Create account'));
  await user.click(await screen.findByRole('checkbox', { name: ACK }));
  await user.click(button('I saved it'));
  await screen.findByRole('heading', { level: 1, name: en['factor.choose.title'] });
}
/** Step 4's Continue, then step 5's code: the sixth digit sends it, and the set-up ends (factorSetupDone). */
async function throughApp(user: UserEvent) {
  await user.click(button('Continue'));
  await screen.findByRole('img', { name: /QR code/ });
  await user.type(screen.getByRole('textbox', { name: en['factor.totp.code'] }), '123456');
  await settleCode();
}

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

function setViewport(phone: boolean) {
  vi.stubGlobal('matchMedia', (query: string) => ({
    matches: phone && query === PHONE_QUERY, media: query, onchange: null,
    addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, dispatchEvent: () => true,
  }));
}

async function doneAtViewport(phone: boolean) {
  setViewport(phone);
  const context = setup();
  await toFactor(context.user);
  await throughApp(context.user);
  await screen.findByRole('heading', { name: 'You’re in' });
  return context;
}

describe.each([{ layout: 'desktop', phone: false }, { layout: 'phone', phone: true }])('$layout done step backup codes', ({ phone }) => {
  it('omits codes from the DOM until Show, and Hide removes them with matching aria-expanded', async () => {
    const { user, view } = await doneAtViewport(phone);
    expect(screen.getByText('2 backup codes')).toBeInTheDocument();
    expect(view.container.querySelector('.d-code-list__code')).toBeNull();
    const show = button('Show');
    expect(show).toHaveAttribute('aria-expanded', 'false');
    const listId = show.getAttribute('aria-controls');
    expect(document.getElementById(listId!)).toBeNull();
    await user.click(show);
    expect(button('Hide')).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByRole('list', { name: en['factor.codes.label'] })).toHaveAttribute('id', listId);
    expect(view.container.querySelectorAll('.d-code-list__code')).toHaveLength(2);
    await user.click(button('Hide'));
    expect(button('Show')).toHaveAttribute('aria-expanded', 'false');
    expect(view.container.querySelector('.d-code-list__code')).toBeNull();
  });

  it('copies all codes while collapsed', async () => {
    const { user } = await doneAtViewport(phone);
    await user.click(button('Copy'));
    expect(await navigator.clipboard.readText()).toBe(CODES.join('\n'));
    expect(screen.queryByRole('list', { name: en['factor.codes.label'] })).toBeNull();
  });

  it('prints all codes while collapsed and removes them afterwards', async () => {
    const { user } = await doneAtViewport(phone);
    const print = vi.spyOn(browser, 'print').mockImplementation(() => {
      expect(screen.getByRole('list', { name: en['factor.codes.label'] }).querySelectorAll('li')).toHaveLength(2);
    });
    await user.click(button('Print'));
    expect(print).toHaveBeenCalledOnce();
    expect(screen.queryByRole('list', { name: en['factor.codes.label'] })).toBeNull();
  });

  it('requires the saved tick before Open dilla while collapsed or expanded', async () => {
    const { user, onFinish } = await doneAtViewport(phone);
    expect(button('Open dilla')).toHaveAttribute('aria-disabled', 'true');
    await user.click(button('Open dilla'));
    expect(onFinish).not.toHaveBeenCalled();
    await user.click(button('Show'));
    expect(button('Open dilla')).toHaveAttribute('aria-disabled', 'true');
    await user.click(button('Hide'));
    await user.click(screen.getByRole('checkbox', { name: CODES_ACK }));
    expect(button('Open dilla')).not.toHaveAttribute('aria-disabled');
    await user.click(button('Open dilla'));
    expect(onFinish).toHaveBeenCalledOnce();
  });
});

describe('steps 4 and 5: the second factor', () => {
  it('wave B task 3: the app step asks for its code in six boxes over one input, and the sixth digit sends it', async () => {
    const { user, view, fake } = setup();
    await toFactor(user);
    await user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    const code = screen.getByRole('textbox', { name: en['factor.totp.code'] });
    expect(code).toHaveAttribute('autocomplete', 'one-time-code');
    expect(code).toHaveAttribute('inputmode', 'numeric');
    expect(code.closest('.d-code-boxes')).not.toBeNull();
    expect(view.container.querySelectorAll('.d-code-boxes__box')).toHaveLength(6);
    expect(view.container.querySelector('.d-code-boxes__row')).toHaveAttribute('aria-hidden', 'true');
    await user.type(code, '12a3456');
    await settleCode();
    expect(fake.callsOf('factorTotpConfirm')).toEqual([{ m: 'factorTotpConfirm', code: '123456' }]);
    await waitFor(() => { expect(h1()).toHaveTextContent('You’re in'); });
  });

  // Wave B task 3b: the code step takes a pasted code and the browser's one-time-code autofill, which both arrive as one
  // whole value (CodeBoxes reads a paste whole, so "123 456" is six digits, not five).
  it('step 5 takes a pasted code and a one-time-code autofill, each sent once', async () => {
    const pasted = setup();
    await toFactor(pasted.user);
    await pasted.user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    await pasted.user.click(screen.getByRole('textbox', { name: en['factor.totp.code'] }));
    await pasted.user.paste('123 456');
    await settleCode();
    expect(pasted.fake.callsOf('factorTotpConfirm')).toEqual([{ m: 'factorTotpConfirm', code: '123456' }]);
    pasted.view.unmount();

    const filled = setup();
    await toFactor(filled.user);
    await filled.user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    // An autofill sets the whole value at once, as one input event.
    const input = screen.getByRole('textbox', { name: en['factor.totp.code'] });
    act(() => { fireEvent.change(input, { target: { value: '654321' } }); });
    await settleCode();
    expect(filled.fake.callsOf('factorTotpConfirm')).toEqual([{ m: 'factorTotpConfirm', code: '654321' }]);
  });

  it('follows the key step with the choice, counted as step 4 of 6, with no way to skip and no way back', async () => {
    const { user, view, onFinish } = setup();
    await toFactor(user);
    expect(h1()).toHaveTextContent('Add a second factor');
    expect(document.activeElement).toBe(h1());
    expect(screen.getByText('New account · step 4 of 6')).toBeInTheDocument();
    expect(screen.getByText('Required for signing in on a new browser.')).toBeInTheDocument();
    // An allow-list: the choice's Continue and the exit that clears this browser, nothing else (the account exists, so
    // there is no Back to the key step).
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual([en['factor.choose.next'], en['devices.forget']]);
    expect(onFinish).not.toHaveBeenCalled();
    await expectNoAxeViolations(view.container);
  });

  it('shows the code as step 5 of 6 in the prototype copy, with the QR code, the setup key and the hint kept', async () => {
    const { user, view } = setup();
    await toFactor(user);
    await user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    expect(screen.getByText('New account · step 5 of 6')).toBeInTheDocument();
    expect(h1()).toHaveTextContent('Enter your code');
    expect(screen.getByRole('textbox', { name: en['factor.totp.key'] })).toHaveValue(SECRET);
    // The hint sits above the boxes, with the QR code and the setup key (Jonas, task 3b); the boxes take focus, as the
    // prototype's code step opens on them, so a one-time-code autofill offers itself at once.
    const hint = screen.getByText(en['factor.totp.codeHint']);
    const code = screen.getByRole('textbox', { name: en['factor.totp.code'] });
    expect(hint.compareDocumentPosition(code) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(code).toHaveFocus();
    expect(button('Verify')).toHaveAttribute('aria-disabled', 'true');
    expect(button('Back').closest('.d-onboarding-frame__nav')).not.toBeNull();
    await expectNoAxeViolations(view.container);
    await user.click(button('Back'));
    expect(h1()).toHaveTextContent('Add a second factor');
    expect(screen.getByText('New account · step 4 of 6')).toBeInTheDocument();
  });

  it('desktop sends factorSetupDone with the invite after the code, then shows done with the backup codes, gating Open dilla on their tick', async () => {
    const { fake, user, onFinish, view } = setup();
    await toFactor(user);
    await throughApp(user);
    expect(fake.callsOf('factorSetupDone')).toEqual([{ m: 'factorSetupDone', invite: 'ABCD-EFGH' }]);
    await waitFor(() => { expect(h1()).toHaveTextContent('You’re in'); });
    expect(screen.getByText('New account · step 6 of 6')).toBeInTheDocument();
    expect(screen.getByText('Your account is ready.')).toBeInTheDocument();
    expect(screen.queryByRole('list', { name: en['factor.codes.label'] })).toBeNull();
    expect(button('Show')).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('heading', { name: en['factor.codes.title'] })).toBeNull();
    expect(button('Open dilla')).toHaveAttribute('aria-disabled', 'true');
    expect(button('Open dilla')).toHaveAccessibleDescription('Tick the box to continue.');
    await user.click(button('Open dilla'));
    expect(onFinish).not.toHaveBeenCalled();
    await expectNoAxeViolations(view.container);
    await user.click(screen.getByRole('checkbox', { name: CODES_ACK }));
    expect(button('Open dilla')).not.toHaveAttribute('aria-disabled');
    await user.click(button('Open dilla'));
    expect(onFinish).toHaveBeenCalledWith({ communityId: null, joinError: null, invite: 'ABCD-EFGH' });
  });

  it('keeps the codes out of every slice, the URL and storage', async () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem');
    const { fake, user } = setup();
    await toFactor(user);
    await throughApp(user);
    await waitFor(() => { expect(h1()).toHaveTextContent('You’re in'); });
    const everywhere = JSON.stringify(fake.calls) + window.location.href + JSON.stringify(setItem.mock.calls)
      + JSON.stringify(fake.get('account')) + JSON.stringify(fake.get('factors'));
    for (const code of CODES) expect(everywhere).not.toContain(code);
    setItem.mockRestore();
  });

  it('copies the codes as plain lines and announces it', async () => {
    const { user } = setup();
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    await toFactor(user);
    await throughApp(user);
    await waitFor(() => { expect(h1()).toHaveTextContent('You’re in'); });
    const announcer = screen.getByTestId('onboarding-announcer');
    await user.click(button(en['factor.codes.copy']));
    expect(writeText).toHaveBeenCalledWith(CODES.join('\n'));
    await waitFor(() => { expect(announcer).toHaveTextContent(en['factor.codes.copied']); });
  });

  it('names the joined server and its first text channel in the prototype panel', async () => {
    const script: Script = (c, f) => {
      if (c.m !== 'factorSetupDone') return base(c, f);
      f.set('account', account({ phase: 'ready' }));
      f.set('communities', [{ id: COMMUNITY, name: 'Midgard Crew', owner: 'aa'.repeat(16) }]);
      f.set(`channels:${COMMUNITY}`, [
        { id: 'e2'.repeat(16), communityId: COMMUNITY, kind: 1, mode: 0, name: 'Lounge', topic: '', parentId: null, position: 0, slowmode: 0, announcement: false, group: 'active' },
        { id: 'e1'.repeat(16), communityId: COMMUNITY, kind: 0, mode: 0, name: 'general', topic: '', parentId: null, position: 1, slowmode: 0, announcement: false, group: 'active' },
      ]);
      return Promise.resolve({ communityId: COMMUNITY, joinError: null });
    };
    const { user, fake } = setup(script);
    await toFactor(user);
    await throughApp(user);
    await waitFor(() => { expect(h1()).toHaveTextContent('You’re in'); });
    expect(screen.getByText('Midgard Crew is waiting in #general.')).toBeInTheDocument();
    // The worker reads a server's channels once it is selected: the done step selects the joined one, once.
    expect(fake.callsOf('selectCommunity')).toEqual([{ m: 'selectCommunity', communityId: COMMUNITY }]);
  });

  it('holds the factor screen until the join has answered, then hands over the joined server', async () => {
    let answer!: (v: unknown) => void;
    const script: Script = (c, f) => {
      if (c.m !== 'factorSetupDone') return base(c, f);
      f.set('account', account({ phase: 'ready' }));
      return new Promise((resolve) => { answer = resolve; });
    };
    const { user, onFinish } = setup(script);
    await toFactor(user);
    await throughApp(user);
    // The phase is ready, but the join has not answered: the done step waits.
    expect(screen.queryByRole('heading', { name: 'You’re in' })).toBeNull();
    await act(async () => { answer({ communityId: COMMUNITY, joinError: null }); await Promise.resolve(); });
    await waitFor(() => { expect(h1()).toHaveTextContent('You’re in'); });
    await user.click(screen.getByRole('checkbox', { name: CODES_ACK }));
    await user.click(button('Open dilla'));
    expect(onFinish).toHaveBeenCalledWith({ communityId: COMMUNITY, joinError: null, invite: 'ABCD-EFGH' });
  });

  it('sends one factorSetupDone however often the code step is pressed', async () => {
    const script: Script = (c, f) => c.m === 'factorSetupDone' ? new Promise(() => {}) : base(c, f);
    const { fake, user } = setup(script);
    await toFactor(user);
    await throughApp(user);
    await user.click(button(en['factor.totp.working']));
    await user.keyboard('{Enter}');
    expect(fake.callsOf('factorSetupDone')).toHaveLength(1);
    expect(fake.callsOf('factorTotpConfirm')).toHaveLength(1);
  });

  it('goes back to the choice, at step 4 and with no error line, when the factor is still owed', async () => {
    // web3.6 E3: the worker refuses E_FACTOR_REQUIRED with no HTTP status; the choice's own copy says what is owed.
    const script: Script = (c, f) => c.m === 'factorSetupDone' ? Promise.reject(refusal({ code: 'E_FACTOR_REQUIRED' })) : base(c, f);
    const { user, fake } = setup(script);
    await toFactor(user);
    await throughApp(user);
    await waitFor(() => { expect(h1()).toHaveTextContent(en['factor.choose.title']); });
    expect(screen.getByText('New account · step 4 of 6')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(fake.callsOf('factorSetupDone')).toHaveLength(1);
  });

  it('resumes at the factor step when the page is mounted there', () => {
    setup(base, { ...factorPhase });
    expect(h1()).toHaveTextContent('Add a second factor');
    expect(screen.getByText('New account · step 4 of 6')).toBeInTheDocument();
  });

  it('has one live region, and a copy is spoken once', async () => {
    const { user, view } = setup();
    await toFactor(user);
    expect(liveRegions(view.container)).toHaveLength(1);
    await user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    const region = screen.getByTestId('onboarding-announcer');
    expect(liveRegions(view.container)).toEqual([region]);
    expect(region).toHaveAttribute('aria-live', 'polite');
    const watch = watchRegion(region);
    await user.click(button('Copy'));
    await waitFor(() => { expect(watch.history).toEqual([en['factor.totp.keyCopied']]); });
    await new Promise(r => setTimeout(r, 450));
    expect(watch.history).toEqual([en['factor.totp.keyCopied']]);
    watch.stop();
  });

  it('keeps the earlier steps free of the factor region', async () => {
    const { user, view } = setup();
    await user.type(screen.getByRole('textbox', { name: 'Invite link' }), 'ABCD-EFGH');
    expect(liveRegions(view.container)).toHaveLength(0);
    await user.click(button('Continue'));
    expect(liveRegions(view.container)).toHaveLength(0);
  });

  it('a 429 or a dropped connection after the code says what happened', async () => {
    for (const [r, text] of [[{ code: 'E_RATE_LIMITED', status: 429, retryAfterMs: 2000 }, 'Too many attempts. Try again in 2 s.'],
      [{ code: 'E_NETWORK', status: 0 }, en['factor.error.network']]] as const) {
      const script: Script = (c, f) => c.m === 'factorSetupDone' ? Promise.reject(refusal(r)) : base(c, f);
      const { user, view } = setup(script);
      await toFactor(user);
      await throughApp(user);
      expect(await screen.findByRole('alert')).toHaveTextContent(text);
      view.unmount();
    }
  });

  describe('retry after a refused finish', () => {
    it('sends factorSetupDone again from the retry step and finishes with the same codes on the done step', async () => {
      let n = 0;
      const script: Script = (c, f) => {
        if (c.m !== 'factorSetupDone') return base(c, f);
        n += 1;
        return n === 1 ? Promise.reject(refusal({ code: 'E_NETWORK', status: 0 })) : base(c, f);
      };
      const { fake, user, onFinish } = setup(script);
      await toFactor(user);
      await throughApp(user);
      expect(await screen.findByRole('alert')).toHaveTextContent(en['factor.error.network']);
      // Security review LOW-4: the factor is in place, so the retry shows the backup codes and their tick before Continue
      // retries the finish; a page closed now has still shown them.
      await waitFor(() => { expect(h1()).toHaveTextContent(en['factor.codes.title']); });
      expect(screen.getByRole('list', { name: en['factor.codes.label'] }).querySelectorAll('li')).toHaveLength(CODES.length);
      expect(button(en['factor.codes.next'])).toHaveAttribute('aria-disabled', 'true');
      await user.click(button(en['factor.codes.next']));
      expect(fake.callsOf('factorSetupDone')).toHaveLength(1);
      await user.click(screen.getByRole('checkbox', { name: en['factor.codes.ack'] }));
      await user.click(button(en['factor.codes.next']));
      await waitFor(() => { expect(h1()).toHaveTextContent('You’re in'); });
      expect(fake.callsOf('factorSetupDone')).toHaveLength(2);
      expect(fake.callsOf('factorTotpConfirm')).toHaveLength(1);
      expect(screen.queryByRole('list', { name: en['factor.codes.label'] })).toBeNull();
      await user.click(button('Show'));
      expect(screen.getByRole('list', { name: en['factor.codes.label'] }).querySelectorAll('li')).toHaveLength(CODES.length);
      await user.click(screen.getByRole('checkbox', { name: CODES_ACK }));
      await user.click(button('Open dilla'));
      expect(onFinish).toHaveBeenCalledTimes(1);
    });
  });

  describe('forget this browser', () => {
    const forget = () => screen.getByRole('button', { name: en['devices.forget'] });
    it('is reachable by keyboard, sends forgetBrowser only on confirm, and Cancel returns focus to it', async () => {
      const { fake, user } = setup();
      await toFactor(user);
      for (let i = 0; i < 8 && document.activeElement !== forget(); i++) await user.tab();
      expect(document.activeElement).toBe(forget());
      await user.keyboard('{Enter}');
      await user.click(await screen.findByRole('button', { name: en['signin.cancel'] }));
      expect(document.activeElement).toBe(forget());
      expect(fake.callsOf('forgetBrowser')).toEqual([]);
      await user.click(forget());
      await user.click(await screen.findByRole('button', { name: en['devices.confirmForget'] }));
      expect(fake.callsOf('forgetBrowser')).toEqual([{ m: 'forgetBrowser' }]);
      expect(fake.callsOf('factorSetupDone')).toEqual([]);
    });
  });
});
