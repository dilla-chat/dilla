// A successful callback is only a delivery acknowledgement. Runtime status must confirm the account.
const returns = new Map<string, number>();
export function noteSpotifyReturn(communityId: string): void { returns.set(communityId, Date.now()); }
export function spotifyReturnDeadline(communityId: string): number | null {
  const at = returns.get(communityId);
  if (at === undefined || Date.now() - at >= 60_000) { returns.delete(communityId); return null; }
  return at + 60_000;
}
