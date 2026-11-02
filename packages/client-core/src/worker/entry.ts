/// <reference lib="webworker" />
import { startCoreWorker } from './runtime';

// The desktop factory sends configuration before any commands. Direct/browser workers retain the existing
// protocol: their first command starts the default origin-root controller and is delivered exactly once.
self.addEventListener('message', function initialize(event: MessageEvent<unknown>) {
  self.removeEventListener('message', initialize);
  const value = event.data as { t?: unknown; instanceURL?: unknown } | null;
  if (value?.t === 'configure-instance') {
    startCoreWorker(self, { testHooks: false, instanceURL: value.instanceURL });
  } else {
    const controller = startCoreWorker(self, { testHooks: false });
    controller.handle(event.data);
  }
});
