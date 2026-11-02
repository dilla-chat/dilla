import { runFailClosed, runProtectedAudio, runProtectedVideo } from '../../media/harness/failclosed.ts';
import { acquireApplicationAudio } from '../../web/src/call/app-audio.ts';
import { acquireDesktopScreen } from '../../web/src/call/desktop-screen.ts';
import { startDesktopLifecycle } from '../../web/src/desktop.ts';
import { observeAudioLifecycle } from './audio-lifecycle.ts';

startDesktopLifecycle();
const capture = () => acquireDesktopScreen({ video: { width: 640, height: 480, frameRate: 15 }, audio: false });
export const validation = {
 run: runFailClosed,
 audio: async (options?: Parameters<typeof runProtectedAudio>[1]) => {
  const track = await acquireApplicationAudio(), lifecycle = observeAudioLifecycle(track);
  try { return { ...await runProtectedAudio(track, options), captureLifecycle: lifecycle.events }; }
  finally { lifecycle.dispose(); }
 },
 capture,
 screen: async (options?: Parameters<typeof runProtectedVideo>[1]) => {
  const stream = await capture(); const track = stream.getVideoTracks()[0];
  try { return { ...await runProtectedVideo(track, options), audioTracks: stream.getAudioTracks().length }; }
  finally { stream.getTracks().forEach(track => track.stop()); }
 },
};
(window as unknown as { validation: typeof validation }).validation = validation;
