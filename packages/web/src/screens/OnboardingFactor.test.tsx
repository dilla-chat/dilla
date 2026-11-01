// web3.6 W2: the second factor as step 5 of 6 of the sign-up (flow 10 state 1; flow 01).
import { describe, it, expect, vi } from 'vitest';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent, { type UserEvent } from '@testing-library/user-event';
import type { AccountState, Command, FactorsState } from '@dilla/client-core';
import { CoreProvider } from '../core/context.tsx';
import { FakeClient, refusal } from '../test/fake-client.ts';
import { account } from '../test/fixtures.ts';
import { expectNoAxeViolations } from '../test/setup.ts';
import { en } from '../strings/index.ts';
import { liveRegions, watchRegion } from '../test/live-regions.ts';
import { Onboarding } from './Onboarding.tsx';

const KEY = ['7K2M', 'QX9D', 'H4TB', 'R8NW', 'C3VF', 'J6PZ', 'A1GE', 'Y5KS', 'M0QT', 'B7XH', 'W2DN', 'F9RC', 'P4ZA'];
const ACK = 'I have written down or printed my recovery key';
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
  await user.type(screen.getByRole('textbox', { name: 'Invite' }), invite);
  await user.click(button('Continue'));
  await user.type(screen.getByRole('textbox', { name: 'Username' }), 'ada');
  await user.click(button('Continue'));
  await user.click(await screen.findByRole('checkbox', { name: ACK }));
  await user.click(button('Continue'));
  await user.click(button('Create account'));
  await screen.findByRole('heading', { level: 1, name: en['factor.choose.title'] });
}
async function throughApp(user: UserEvent) {
  await user.click(button('Continue'));
  await screen.findByRole('img', { name: /QR code/ });
  await user.type(screen.getByRole('textbox', { name: en['factor.totp.code'] }), '123456{Enter}');
  await user.click(await screen.findByRole('checkbox', { name: en['factor.codes.ack'] }));
}

describe('step 5: the second factor', () => {
  it('follows the account step with the choice, counted as step 5 of 6, with no way to skip', async () => {
    const { user, view, onFinish } = setup();
    await toFactor(user);
    expect(h1()).toHaveTextContent('Add a second factor');
    expect(document.activeElement).toBe(h1());
    expect(screen.getByText('New account · step 5 of 6')).toBeInTheDocument();
    // An allow-list: the choice's Continue and the exit that clears this browser, nothing else.
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual([en['factor.choose.next'], en['devices.forget']]);
    expect(onFinish).not.toHaveBeenCalled();
    await expectNoAxeViolations(view.container);
  });

  it('sends factorSetupDone with the invite after the codes, then shows done as step 6 of 6 and hands the result on', async () => {
    const { fake, user, onFinish } = setup();
    await toFactor(user);
    await throughApp(user);
    await user.click(button('Continue'));
    expect(fake.callsOf('factorSetupDone')).toEqual([{ m: 'factorSetupDone', invite: 'ABCD-EFGH' }]);
    await waitFor(() => { expect(h1()).toHaveTextContent('You’re in'); });
    expect(screen.getByText('New account · step 6 of 6')).toBeInTheDocument();
    expect(screen.getByText('You are ada on dilla.test.')).toBeInTheDocument();
    await user.click(button('Open dilla'));
    expect(onFinish).toHaveBeenCalledWith({ communityId: null, joinError: null, invite: 'ABCD-EFGH' });
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
    await user.click(button('Continue'));
    // The phase is ready, but the join has not answered: the done step waits.
    expect(screen.queryByRole('heading', { name: 'You’re in' })).toBeNull();
    await act(async () => { answer({ communityId: COMMUNITY, joinError: null }); await Promise.resolve(); });
    await waitFor(() => { expect(h1()).toHaveTextContent('You’re in'); });
    await user.click(button('Open dilla'));
    expect(onFinish).toHaveBeenCalledWith({ communityId: COMMUNITY, joinError: null, invite: 'ABCD-EFGH' });
  });

  it('sends one factorSetupDone for a double press of Continue', async () => {
    const script: Script = (c, f) => c.m === 'factorSetupDone' ? new Promise(() => {}) : base(c, f);
    const { fake, user } = setup(script);
    await toFactor(user);
    await throughApp(user);
    await user.click(button('Continue'));
    await user.click(button('Continue'));
    expect(fake.callsOf('factorSetupDone')).toHaveLength(1);
  });

  it('goes back to the choice, at the same step and with no error line, when the factor is still owed', async () => {
    // web3.6 E3: the worker refuses E_FACTOR_REQUIRED with no HTTP status; the choice's own copy says what is owed.
    const script: Script = (c, f) => c.m === 'factorSetupDone' ? Promise.reject(refusal({ code: 'E_FACTOR_REQUIRED' })) : base(c, f);
    const { user, fake } = setup(script);
    await toFactor(user);
    await throughApp(user);
    await user.click(button('Continue'));
    await waitFor(() => { expect(h1()).toHaveTextContent(en['factor.choose.title']); });
    expect(screen.getByText('New account · step 5 of 6')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(fake.callsOf('factorSetupDone')).toHaveLength(1);
  });

  it('resumes at the factor step when the page was reloaded there', () => {
    setup(base, { ...factorPhase });
    expect(h1()).toHaveTextContent('Add a second factor');
    expect(screen.getByText('New account · step 5 of 6')).toBeInTheDocument();
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
    await user.type(screen.getByRole('textbox', { name: 'Invite' }), 'ABCD-EFGH');
    expect(liveRegions(view.container)).toHaveLength(0);
    await user.click(button('Continue'));
    expect(liveRegions(view.container)).toHaveLength(0);
  });

  it('a 429 or a dropped connection on the last step says what happened', async () => {
    for (const [r, text] of [[{ code: 'E_RATE_LIMITED', status: 429, retryAfterMs: 2000 }, 'Too many attempts. Try again in 2 s.'],
      [{ code: 'E_NETWORK', status: 0 }, en['factor.error.network']]] as const) {
      const script: Script = (c, f) => c.m === 'factorSetupDone' ? Promise.reject(refusal(r)) : base(c, f);
      const { user, view } = setup(script);
      await toFactor(user);
      await throughApp(user);
      await user.click(button('Continue'));
      expect(await screen.findByRole('alert')).toHaveTextContent(text);
      view.unmount();
    }
  });

  describe('retry after a refused finish', () => {
    it('sends factorSetupDone again from the same button, with the codes still on screen, and finishes', async () => {
      let n = 0;
      const script: Script = (c, f) => {
        if (c.m !== 'factorSetupDone') return base(c, f);
        n += 1;
        return n === 1 ? Promise.reject(refusal({ code: 'E_NETWORK', status: 0 })) : base(c, f);
      };
      const { fake, user, onFinish } = setup(script);
      await toFactor(user);
      await throughApp(user);
      await user.click(button('Continue'));
      expect(await screen.findByRole('alert')).toHaveTextContent(en['factor.error.network']);
      expect(screen.getAllByRole('listitem').length).toBeGreaterThan(0);
      await user.click(button('Continue'));
      await waitFor(() => { expect(h1()).toHaveTextContent('You’re in'); });
      expect(fake.callsOf('factorSetupDone')).toHaveLength(2);
      await user.click(button('Open dilla'));
      expect(onFinish).toHaveBeenCalledTimes(1);
    });
    it('a double press while one call is in flight sends it once', async () => {
      const { fake, user } = setup((c, f) => c.m === 'factorSetupDone' ? new Promise(() => {}) : base(c, f));
      await toFactor(user);
      await throughApp(user);
      await user.dblClick(button('Continue'));
      expect(fake.callsOf('factorSetupDone')).toHaveLength(1);
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
