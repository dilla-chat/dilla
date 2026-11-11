import { StrictMode, useEffect, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { ScreenSourcePicker, type ScreenSourceTile } from '@dilla/ui';
import '@dilla/ui/base.css';
import './screen-picker-page.css';

// The screen-share picker window's page: the packages/ui picker over the tiles main sends. It has no network, no storage and
// no bridge beyond `dillaPicker` (screen-picker-preload.ts); the only thing it can send is one tile id or Cancel.
interface PickerBridge {
  onSources(listener: (sources: ScreenSourceTile[]) => void): () => void;
  choose(id: string): void;
  cancel(): void;
}
const bridge = (globalThis as { dillaPicker?: PickerBridge }).dillaPicker;
const LABELS = { title: 'Share your screen', screens: 'Screens', windows: 'Windows', share: 'Share', cancel: 'Cancel', empty: 'Nothing to share here' };

function Picker() {
  const [sources, setSources] = useState<ScreenSourceTile[]>([]);
  useEffect(() => bridge?.onSources(setSources), []);
  return <ScreenSourcePicker open sources={sources} labels={LABELS} onShare={id => bridge?.choose(id)} onCancel={() => bridge?.cancel()} />;
}

const root = document.getElementById('root');
if (root) createRoot(root).render(<StrictMode><Picker /></StrictMode>);
