import { runFailClosed } from '../../media/harness/failclosed.ts';
import { startDesktopLifecycle } from '../../web/src/desktop.ts';

startDesktopLifecycle();
(window as unknown as { validation: { run: typeof runFailClosed } }).validation = { run: runFailClosed };
