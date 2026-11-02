import { test } from '@playwright/test';
import { runNativeStockFeasibility } from './native-stock-feasibility.ts';
test('stock DEB manager automation feasibility', async () => { await runNativeStockFeasibility(); });
