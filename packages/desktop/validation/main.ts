import { runFailClosed, runProtectedAudio } from '../../media/harness/failclosed.ts';
import { acquireApplicationAudio } from '../../web/src/call/app-audio.ts';
import { startDesktopLifecycle } from '../../web/src/desktop.ts';

startDesktopLifecycle();
(window as unknown as { validation: { run: typeof runFailClosed; audio(options?: Parameters<typeof runProtectedAudio>[1]): ReturnType<typeof runProtectedAudio> } }).validation = { run: runFailClosed, audio: async options => runProtectedAudio(await acquireApplicationAudio(), options) };
