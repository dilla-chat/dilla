import { test } from '@playwright/test';
import { runNativeStockFeasibility } from './native-stock-feasibility.ts';
test('stock installed manager Connect and HTTPS onboarding', async () => { await runNativeStockFeasibility(); });
