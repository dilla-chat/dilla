import { describe, expect, it } from 'vitest';
import { decode } from '../cbor';
import { cborReply, fakeTransport, refusal } from './fake-fetch';
import { HttpClient } from './client';
import { negotiateSession } from './negotiate-session';

describe('negotiateSession', () => {
  it('negotiate offers only wire 6', async () => {
    const transport = fakeTransport({ token: 'enrolled-token' });
    transport.replies.push(cborReply(200, [6, 1, 1]));
    const accepted = await negotiateSession(new HttpClient(transport.deps));
    expect(accepted).toEqual({ wire: 6, e2ee: 1, media: 1, botAvailable: true });
    const request = transport.seen[0];
    expect(request.url).toBe('https://dilla.test/v1/sessions/negotiate');
    expect(request.method).toBe('POST');
    expect(request.headers.get('authorization')).toBe('Bearer enrolled-token');
    expect(request.headers.get('content-type')).toBe('application/cbor');
    // Bot v2 Task 1: wire 6 is the only wire; older instances answer E_VERSION.
    expect(decode(request.body!)).toEqual([[6n], [1n], [1n]]);
  });

  it('passes an upgrade-required refusal through', async () => {
    const transport = fakeTransport();
    transport.replies.push(refusal(426, 'E_UPGRADE_REQUIRED'));
    await expect(negotiateSession(new HttpClient(transport.deps))).rejects.toMatchObject({
      status: 426, code: 'E_UPGRADE_REQUIRED',
    });
  });

  it('preserves unsupported server refusals instead of assuming wire 3', async () => {
    const transport = fakeTransport();
    transport.replies.push(refusal(404, 'E_NOT_FOUND'));
    await expect(negotiateSession(new HttpClient(transport.deps))).rejects.toMatchObject({
      status: 404, code: 'E_NOT_FOUND',
    });
  });

  it.each([[[]], [[6, 1]], [[6, 1, 1, 1]], [[0, 1, 1]], [['6', 1, 1]], [[5, 1, 1]], [[4, 1, 1]], [[3, 1, 1]], [[2, 1, 1]], [[7, 1, 1]], [[6, 2, 1]], [[6, 1, 2]]])
    ('rejects malformed or invalid accepted versions %j', async (reply) => {
    const transport = fakeTransport();
    transport.replies.push(cborReply(200, reply));
    await expect(negotiateSession(new HttpClient(transport.deps))).rejects.toThrow();
  });
});
