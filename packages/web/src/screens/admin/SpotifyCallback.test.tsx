import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { account, INSTANCE, ME } from '../../test/fixtures.ts';
import { SpotifyCallback } from './SpotifyCallback.tsx';
import { spotifySession } from './spotify-link.ts';

const COMMUNITY = '11'.repeat(16);
const ATTEMPT = '22'.repeat(16);
const STATE = 'csrf-state';

function setup(query = `?code=oauth-code&state=${STATE}`, expires = `${Math.floor(Date.now() / 1000) + 600}`,
  handler?: (command: import('@dilla/client-core').Command) => Promise<unknown>) {
  spotifySession.clear();
  spotifySession.setItem('dilla.spotifyAttempt', JSON.stringify({
    communityId: COMMUNITY, attemptId: ATTEMPT, state: STATE, expires,
    instanceId: INSTANCE.id, userId: ME.id, deviceId: 'cc'.repeat(16),
  }));
  window.history.replaceState(null, '', `/settings/bot/music/spotify/callback${query}`);
  const client = new FakeClient();
  client.set('account', account());
  if (handler !== undefined) client.handler = handler;
  return { client, view: render(<CoreProvider client={client}><SpotifyCallback /></CoreProvider>) };
}

describe('Spotify authorization callback', () => {
  it('validates the attempt context, posts the transient code, scrubs the URL and clears storage', async () => {
    const { client } = setup();
    expect(await screen.findByText(/Authorization received; waiting for the bot/i)).toBeInTheDocument();
    expect(client.callsOf('callbackBotSpotifyAttempt')).toEqual([expect.objectContaining({
      communityId: COMMUNITY, attemptId: ATTEMPT, state: STATE, code: 'oauth-code',
    })]);
    expect(window.location.search).toBe('');
    expect(window.location.pathname).toBe(`/c/${COMMUNITY}/settings/music`);
    expect(screen.queryByText('Linked')).not.toBeInTheDocument();
    expect(spotifySession.getItem('dilla.spotifyAttempt')).toBeNull();
  });

  it('rejects a mismatched CSRF state without sending the code', async () => {
    const { client } = setup('?code=oauth-code&state=wrong');
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    await waitFor(() => expect(spotifySession.getItem('dilla.spotifyAttempt')).toBeNull());
    expect(client.callsOf('callbackBotSpotifyAttempt')).toHaveLength(0);
  });

  it('cancels a denied attempt and reports the denial', async () => {
    const { client } = setup(`?error=access_denied&state=${STATE}`);
    expect(await screen.findByText(/authorization was declined/i)).toBeInTheDocument();
    expect(client.callsOf('cancelBotSpotifyAttempt')).toEqual([expect.objectContaining({ communityId: COMMUNITY, attemptId: ATTEMPT })]);
  });

  it('cancels an expired attempt and reports expiry', async () => {
    const { client } = setup(`?code=oauth-code&state=${STATE}`, `${Math.floor(Date.now() / 1000) - 1}`);
    expect(await screen.findByText(/attempt expired/i)).toBeInTheDocument();
    expect(client.callsOf('cancelBotSpotifyAttempt')).toHaveLength(1);
    expect(client.callsOf('callbackBotSpotifyAttempt')).toHaveLength(0);
  });

  it('lets the signed-in user cancel an attempt from the callback page', async () => {
    const { client } = setup('');
    await userEvent.setup().click(await screen.findByRole('button', { name: /cancel authorization attempt/i }));
    expect(await screen.findByText(/authorization was cancelled/i)).toBeInTheDocument();
    expect(client.callsOf('cancelBotSpotifyAttempt')).toHaveLength(1);
  });

  it('keeps an ambiguous callback in memory and retries the same code and attempt', async () => {
    let tries = 0;
    const { client } = setup(undefined, undefined, command => {
      if (command.m === 'callbackBotSpotifyAttempt' && tries++ === 0) return Promise.reject(new Error('network lost after request'));
      return Promise.resolve(null);
    });
    expect(await screen.findByRole('button', { name: /retry authorization delivery/i })).toBeInTheDocument();
    expect(window.location.search).toBe('');
    expect(spotifySession.getItem('dilla.spotifyAttempt')).not.toBeNull();
    await userEvent.setup().click(screen.getByRole('button', { name: /retry authorization delivery/i }));
    expect(await screen.findByText(/Authorization received; waiting for the bot/i)).toBeInTheDocument();
    expect(client.callsOf('callbackBotSpotifyAttempt')).toEqual([
      expect.objectContaining({ attemptId: ATTEMPT, state: STATE, code: 'oauth-code' }),
      expect.objectContaining({ attemptId: ATTEMPT, state: STATE, code: 'oauth-code' }),
    ]);
    expect(spotifySession.getItem('dilla.spotifyAttempt')).toBeNull();
  });

  it('returns to the associated server after authorization has a definite result', async () => {
    const { client } = setup();
    expect(await screen.findByText(/Authorization received; waiting for the bot/i)).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: /return to server/i }));
    expect(window.location.pathname).toBe(`/c/${COMMUNITY}/settings/music`);
    expect(client.callsOf('callbackBotSpotifyAttempt')).toHaveLength(1);
  });
});
