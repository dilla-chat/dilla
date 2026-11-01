// web3.6 W2: the forced screen (flow 10 state 2) and the App routing that reaches it.
import { describe, it, expect } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { Command, FactorsState } from '@dilla/client-core';
import { App } from '../../App.tsx';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { account } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { en } from '../../strings/index.ts';
import { liveRegions, watchRegion } from '../../test/live-regions.ts';

const SECRET = 'JBSWY3DPEHPK3PXP';
const CODES = ['AAAAABBBBBCCCCCDDDDDEEEEEF'];
const NO_FACTORS: FactorsState = { owed: true, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: true };

type Script = (c: Command, fake: FakeClient) => Promise<unknown>;
const happy: Script = (c, f) => {
  switch (c.m) {
    case 'factorTotpBegin': return Promise.resolve({ secret: SECRET, uri: `otpauth://totp/dilla.test:ada?secret=${SECRET}` });
    case 'factorTotpConfirm': return Promise.resolve({ backupCodes: CODES });
    case 'factorSetupDone': f.set('account', account({ phase: 'ready' })); return Promise.resolve({ communityId: null, joinError: null });
    default: return Promise.resolve(null);
  }
};

function setup(script: Script = happy, origin: 'required' | 'signup' | 'signin' = 'required') {
  window.history.replaceState(null, '', '/');
  const fake = new FakeClient();
  fake.set('account', account({ phase: 'factor-setup', factorSetup: { origin } }));
  fake.set('factors', NO_FACTORS);
  fake.handler = c => script(c, fake);
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><App fatal={null} /></CoreProvider></div>);
  return { fake, user, view };
}
const h1 = () => screen.getByRole('heading', { level: 1 });
const button = (name: string) => screen.getByRole('button', { name });

describe('the forced screen', () => {
  it('shows the choice with the required paragraph, no step line and no skip, cancel or back', async () => {
    const { view } = setup();
    expect(h1()).toHaveTextContent('Add a second factor');
    expect(screen.getByText(en['factor.choose.required'].replace('{instance}', 'dilla.test'))).toBeInTheDocument();
    expect(screen.queryByText(/^Step \d/)).toBeNull();
    // An allow-list: the only buttons are the choice's Continue and the one exit that clears this browser.
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual([en['factor.choose.next'], en['devices.forget']]);
    expect(screen.queryByRole('navigation')).toBeNull();
    await expectNoAxeViolations(view.container);
  });

  it('goes through the app, the codes and the done step, announces it, and opens the shell', async () => {
    const { fake, user } = setup();
    await user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    await user.type(screen.getByRole('textbox', { name: en['factor.totp.code'] }), '123456{Enter}');
    await user.click(await screen.findByRole('checkbox', { name: en['factor.codes.ack'] }));
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent('Second factor added');
    expect(screen.getByTestId('forced-announcer')).toHaveTextContent('Second factor added.');
    expect(screen.getByTestId('forced-announcer')).toHaveAttribute('aria-live', 'polite');
    await user.click(button('Open dilla'));
    expect(fake.callsOf('factorSetupDone')).toEqual([{ m: 'factorSetupDone', invite: null }]);
    await waitFor(() => { expect(screen.getByRole('navigation', { name: 'servers' })).toBeInTheDocument(); });
  });

  it('goes back to the choice, with no connection line, when the instance still finds the factor owed', async () => {
    // Web3.6 E3 finding: the worker refuses a done set-up E_FACTOR_REQUIRED with no HTTP status (status 0, as
    // factor.test.ts pins it), which the page showed as a dropped connection, and the person was left at the done step.
    const script: Script = (c, f) => c.m === 'factorSetupDone' ? Promise.reject(refusal({ code: 'E_FACTOR_REQUIRED' })) : happy(c, f);
    const { fake, user } = setup(script);
    await user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    await user.type(screen.getByRole('textbox', { name: en['factor.totp.code'] }), '123456{Enter}');
    await user.click(await screen.findByRole('checkbox', { name: en['factor.codes.ack'] }));
    await user.click(button('Continue'));
    await user.click(button('Open dilla'));
    await waitFor(() => { expect(h1()).toHaveTextContent(en['factor.choose.title']); });
    expect(screen.getByText(en['factor.choose.required'].replace('{instance}', 'dilla.test'))).toBeInTheDocument();
    expect(screen.queryByText(en['factor.error.network'])).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(fake.callsOf('factorSetupDone')).toHaveLength(1);
  });

  it('an operator reset on a live page shows the choice, never the done step, while the old factors are still in the slice', async () => {
    // Web3.6 E3 finding: a slice that says owed is never taken for a factor already in place.
    const { fake } = setup();
    fake.publish('factors', { owed: true, totp: { confirmedAt: 5 }, passkeys: [], backupCodesLeft: 8, hasPassword: true });
    await Promise.resolve();
    expect(h1()).toHaveTextContent(en['factor.choose.title']);
    expect(screen.queryByText(en['factor.done.title'])).toBeNull();
    expect(fake.callsOf('factorSetupDone')).toEqual([]);
  });
});

describe('the origin picks the screen', () => {
  it('signup mounts onboarding at step 5 of 6', () => {
    setup(happy, 'signup');
    expect(h1()).toHaveTextContent('Add a second factor');
    expect(screen.getByText('New account · step 5 of 6')).toBeInTheDocument();
  });
  it('signin mounts the sign-in ceremony at step 3 of 4', () => {
    setup(happy, 'signin');
    expect(h1()).toHaveTextContent('Add a second factor');
    expect(screen.getByText('Sign in · step 3 of 4')).toBeInTheDocument();
  });
});

describe('the way out: forget this browser', () => {
  const forget = () => screen.getByRole('button', { name: en['devices.forget'] });

  it('is reachable from the keyboard and opens a confirm that opens on Cancel', async () => {
    const { user } = setup();
    for (let i = 0; i < 8 && document.activeElement !== forget(); i++) await user.tab();
    expect(document.activeElement).toBe(forget());
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('dialog', { name: en['devices.forgetTitle'] })).toBeInTheDocument();
    expect(screen.getByText(en['devices.forgetBody'])).toBeInTheDocument();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: en['signin.cancel'] }));
  });

  it('sends forgetBrowser on confirm and nothing else: the factor is not marked set and the shell is not reached', async () => {
    const { fake, user } = setup((c) => c.m === 'forgetBrowser' ? new Promise(() => {}) : Promise.resolve(null));
    await user.click(forget());
    await user.click(await screen.findByRole('button', { name: en['devices.confirmForget'] }));
    expect(fake.calls.filter(c => c.m !== 'start')).toEqual([{ m: 'forgetBrowser' }]);
    expect(screen.queryByRole('navigation', { name: 'servers' })).toBeNull();
    expect(fake.callsOf('factorSetupDone')).toEqual([]);
  });

  it('Cancel and Escape close the dialog, send nothing and return focus to the control', async () => {
    const { fake, user } = setup();
    await user.click(forget());
    await user.click(await screen.findByRole('button', { name: en['signin.cancel'] }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(forget());
    await user.click(forget());
    await screen.findByRole('dialog');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(forget());
    expect(fake.callsOf('forgetBrowser')).toEqual([]);
    expect(h1()).toHaveTextContent('Add a second factor');
  });

  it('shows a refused wipe in the dialog and puts focus on Cancel', async () => {
    const { user } = setup((c) => c.m === 'forgetBrowser' ? Promise.reject(refusal({ code: 'E_STATE' })) : Promise.resolve(null));
    await user.click(forget());
    await user.click(await screen.findByRole('button', { name: en['devices.confirmForget'] }));
    expect(await screen.findByRole('alert')).toHaveTextContent('That did not work (E_STATE). Try again.');
    expect(document.activeElement).toBe(screen.getByRole('button', { name: en['signin.cancel'] }));
  });

  it('is on the sign-up and sign-in set-up views too', () => {
    for (const origin of ['signup', 'signin'] as const) {
      const { view } = setup(happy, origin);
      expect(forget()).toBeInTheDocument();
      view.unmount();
    }
  });
});

describe('refusals of the last step, by status and code', () => {
  async function toDone(script: Script) {
    const r = setup(script);
    await r.user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    await r.user.type(screen.getByRole('textbox', { name: en['factor.totp.code'] }), '123456{Enter}');
    await r.user.click(await screen.findByRole('checkbox', { name: en['factor.codes.ack'] }));
    await r.user.click(button('Continue'));
    await r.user.click(button('Open dilla'));
    return r;
  }
  it('a 429 says when to try again', async () => {
    await toDone((c, f) => c.m === 'factorSetupDone' ? Promise.reject(refusal({ code: 'E_RATE_LIMITED', status: 429, retryAfterMs: 3000 })) : happy(c, f));
    expect(await screen.findByRole('alert')).toHaveTextContent('Too many attempts. Try again in 3 s.');
  });
  it('a dropped connection says so, not a code', async () => {
    await toDone((c, f) => c.m === 'factorSetupDone' ? Promise.reject(refusal({ code: 'E_NETWORK', status: 0 })) : happy(c, f));
    expect(await screen.findByRole('alert')).toHaveTextContent(en['factor.error.network']);
  });
});

describe('the announcer', () => {
  it('has one live region on the choice, and speaks the added factor exactly once', async () => {
    const { view, user } = setup();
    expect(liveRegions(view.container)).toHaveLength(1);
    const watch = watchRegion(screen.getByTestId('forced-announcer'));
    await user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    await user.type(screen.getByRole('textbox', { name: en['factor.totp.code'] }), '123456{Enter}');
    await user.click(await screen.findByRole('checkbox', { name: en['factor.codes.ack'] }));
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent('Second factor added');
    await waitFor(() => { expect(watch.history).toEqual([en['factor.added']]); });
    // Nothing else speaks later, and no second region appeared.
    await new Promise(r => setTimeout(r, 450));
    expect(watch.history).toEqual([en['factor.added']]);
    expect(liveRegions(view.container)).toHaveLength(1);
    watch.stop();
  });
  it('speaks one copy once', async () => {
    const { view, user } = setup();
    await user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    const watch = watchRegion(screen.getByTestId('forced-announcer'));
    await user.click(button('Copy'));
    await waitFor(() => { expect(watch.history).toEqual([en['factor.totp.keyCopied']]); });
    await new Promise(r => setTimeout(r, 450));
    expect(watch.history).toEqual([en['factor.totp.keyCopied']]);
    expect(liveRegions(view.container)).toHaveLength(1);
    watch.stop();
  });
});

describe('retry after a refused finish', () => {
  async function toOpen(script: Script) {
    const r = setup(script);
    await r.user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    await r.user.type(screen.getByRole('textbox', { name: en['factor.totp.code'] }), '123456{Enter}');
    await r.user.click(await screen.findByRole('checkbox', { name: en['factor.codes.ack'] }));
    await r.user.click(button('Continue'));
    return r;
  }
  it('sends factorSetupDone again from the same button and gets in', async () => {
    let n = 0;
    const { fake, user } = await toOpen((c, f) => {
      if (c.m !== 'factorSetupDone') return happy(c, f);
      n += 1;
      // A refusal that is not the debt (still owed goes back to the choice: see above) keeps the done step and its button.
      return n === 1 ? Promise.reject(refusal({ code: 'E_INTERNAL', status: 500 })) : happy(c, f);
    });
    await user.click(button('Open dilla'));
    expect(await screen.findByRole('alert')).toHaveTextContent('E_INTERNAL');
    await user.click(button('Open dilla'));
    await waitFor(() => { expect(screen.getByRole('navigation', { name: 'servers' })).toBeInTheDocument(); });
    expect(fake.callsOf('factorSetupDone')).toHaveLength(2);
  });
  it('a double click while one call is in flight sends it once', async () => {
    const { fake, user } = await toOpen((c, f) => c.m === 'factorSetupDone' ? new Promise(() => {}) : happy(c, f));
    await user.dblClick(button('Open dilla'));
    await user.keyboard('{Enter}');
    expect(fake.callsOf('factorSetupDone')).toHaveLength(1);
  });
});
