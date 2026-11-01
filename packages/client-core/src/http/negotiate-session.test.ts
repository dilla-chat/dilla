import { describe, expect, it } from 'vitest';
import { decode } from '../cbor';
import { cborReply, fakeTransport, refusal } from './fake-fetch';
import { HttpClient } from './client';
import { negotiateSession } from './negotiate-session';

describe('negotiateSession', () => {
  it('posts the version preferences with the authenticated bearer and returns typed accepted capabilities', async () => {
    const transport = fakeTransport({ token: 'enrolled-token' });
    transport.replies.push(cborReply(200, [3, 1, 1]));
    const accepted = await negotiateSession(new HttpClient(transport.deps));
    expect(accepted).toEqual({ wire: 3, e2ee: 1, media: 1, botAvailable: true });
    const request = transport.seen[0];
    expect(request.url).toBe('https://dilla.test/v1/sessions/negotiate');
    expect(request.method).toBe('POST');
    expect(request.headers.get('authorization')).toBe('Bearer enrolled-token');
    expect(request.headers.get('content-type')).toBe('application/cbor');
    // Task S1 part 2: wire 4 (the account-kind attestation) first, then 3 and 2.
    expect(decode(request.body!)).toEqual([[5n, 4n, 3n, 2n], [1n], [1n]]);
  });

  it('accepts wire 5 realtime capability', async () => {
    const transport = fakeTransport();
    transport.replies.push(cborReply(200, [5, 1, 1]));
    await expect(negotiateSession(new HttpClient(transport.deps))).resolves.toEqual({ wire: 5, e2ee: 1, media: 1, botAvailable: true });
  });

  it('accepts wire 4 with bot capability', async () => {
    const transport = fakeTransport();
    transport.replies.push(cborReply(200, [4, 1, 1]));
    await expect(negotiateSession(new HttpClient(transport.deps))).resolves.toEqual({
      wire: 4, e2ee: 1, media: 1, botAvailable: true,
    });
  });

  it('reports wire 2 as available without bot capability', async () => {
    const transport = fakeTransport();
    transport.replies.push(cborReply(200, [2, 1, 1]));
    await expect(negotiateSession(new HttpClient(transport.deps))).resolves.toEqual({
      wire: 2, e2ee: 1, media: 1, botAvailable: false,
    });
  });

  it('preserves unsupported server refusals instead of assuming wire 3', async () => {
    const transport = fakeTransport();
    transport.replies.push(refusal(404, 'E_NOT_FOUND'));
    await expect(negotiateSession(new HttpClient(transport.deps))).rejects.toMatchObject({
      status: 404, code: 'E_NOT_FOUND',
    });
  });

  it.each([[[]], [[3, 1]], [[3, 1, 1, 1]], [[0, 1, 1]], [['3', 1, 1]], [[6, 1, 1]], [[1, 1, 1]], [[3, 2, 1]]])
    ('rejects malformed or invalid accepted versions %j', async (reply) => {
    const transport = fakeTransport();
    transport.replies.push(cborReply(200, reply));
    await expect(negotiateSession(new HttpClient(transport.deps))).rejects.toThrow();
  });
});
