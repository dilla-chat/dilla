import { beforeEach, describe, expect, it } from 'vitest';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { clearCommandDraftMemory, CommandHost } from './CommandHost.tsx';
import { CommandProvider } from './CommandContext.tsx';
import type { CommandProviderHandlers } from './command-types.ts';
import { privateResults } from './private-results.ts';

beforeEach(() => clearCommandDraftMemory());
type Uncertain = { status: 'uncertain'; private: true; message: string; retry?: true };
const host = (channelId: string, handlers: CommandProviderHandlers) => <CommandProvider handlers={handlers}>
  <CommandHost communityId={null} channelId={channelId} items={[]} selfId={null} draft="/me hello" onDraft={() => undefined}
    onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled /></CommandProvider>;
const rows = (fake: FakeClient, channelId: string) => privateResults(fake, ':', `dm/${channelId}/`).rows();

describe('a held row (an uncertain attempt offering Retry)', () => {
  it('is held while the composer offers Retry, and released when the host unmounts', async () => {
    const fake = new FakeClient();
    const handlers: CommandProviderHandlers = { me: () => Promise.resolve({ status: 'uncertain', private: true, message: 'no answer', retry: true } as Uncertain) };
    const view = render(<CoreProvider client={fake}>{host('hold-a', handlers)}</CoreProvider>);
    await userEvent.setup().click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(rows(fake, 'hold-a')[0]?.hold).toBe(true));
    expect(rows(fake, 'hold-a')[0]?.settledAt).toBeUndefined();
    view.unmount();
    expect(rows(fake, 'hold-a')[0]?.hold).toBe(false);
    expect(rows(fake, 'hold-a')[0]?.settledAt).toBeDefined();
  });
  it('is released when the conversation changes', async () => {
    const fake = new FakeClient();
    const handlers: CommandProviderHandlers = { me: () => Promise.resolve({ status: 'uncertain', private: true, message: 'no answer', retry: true } as Uncertain) };
    const view = render(<CoreProvider client={fake}>{host('hold-b', handlers)}</CoreProvider>);
    await userEvent.setup().click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(rows(fake, 'hold-b')[0]?.hold).toBe(true));
    view.rerender(<CoreProvider client={fake}>{host('hold-c', handlers)}</CoreProvider>);
    await waitFor(() => expect(rows(fake, 'hold-b')[0]?.hold).toBe(false));
    expect(rows(fake, 'hold-b')[0]?.settledAt).toBeDefined();
  });
  it('is not held when the answer arrives after the host is gone, or offers no Retry', async () => {
    const fake = new FakeClient(); let finish!: (v: Uncertain) => void;
    const late: CommandProviderHandlers = { me: () => new Promise(resolve => { finish = resolve; }) };
    const view = render(<CoreProvider client={fake}>{host('hold-d', late)}</CoreProvider>);
    await userEvent.setup().click(await screen.findByRole('button', { name: 'send' }));
    view.unmount();
    await act(() => { finish({ status: 'uncertain', private: true, message: 'no answer', retry: true }); return Promise.resolve(); });
    expect(rows(fake, 'hold-d')[0]?.hold).not.toBe(true);
    expect(rows(fake, 'hold-d')[0]?.settledAt).toBeDefined();
    const plain: CommandProviderHandlers = { me: () => Promise.resolve({ status: 'uncertain', private: true, message: 'maybe' } as Uncertain) };
    render(<CoreProvider client={fake}>{host('hold-e', plain)}</CoreProvider>);
    await userEvent.setup().click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(rows(fake, 'hold-e')[0]?.text).toBe('maybe'));
    expect(rows(fake, 'hold-e')[0]?.hold).not.toBe(true);
  });
});
