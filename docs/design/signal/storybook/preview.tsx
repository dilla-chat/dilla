import type { Preview } from '@storybook/react-vite';
import '../src/styles/base.css';

// base.css needs: [data-motion="reduced"] { --duration-press:0ms; --duration-fast:0ms; --duration-normal:0ms;
//   --duration-slow:0ms; --duration-enter:0ms; --duration-badge:0ms; --duration-toast:0ms; --duration-pulse:0ms;
//   --duration-caret:0ms; --duration-flash:0ms; }

const preview: Preview = {
  globalTypes: {
    theme: { description: 'Theme', toolbar: { title: 'Theme', items: ['signal', 'light', 'high-contrast'], dynamicTitle: true } },
    density: { description: 'Density', toolbar: { title: 'Density', items: ['compact', 'regular', 'cozy'], dynamicTitle: true } },
    motion: { description: 'Motion', toolbar: { title: 'Motion', items: ['full', 'reduced'], dynamicTitle: true } },
  },
  initialGlobals: { theme: 'signal', density: 'regular', motion: 'full', viewport: { value: 'desktop' } },
  decorators: [
    (Story, ctx) => {
      const el = document.documentElement;
      el.dataset.theme = ctx.globals.theme;
      el.dataset.density = ctx.globals.density;
      if (ctx.globals.motion === 'reduced') el.dataset.motion = 'reduced'; else delete el.dataset.motion;
      return (
        <div className="d-root" style={{ padding: 16, minHeight: 120, background: 'var(--bg)', color: 'var(--fg)' }}>
          <Story />
        </div>
      );
    },
  ],
  parameters: {
    a11y: { test: 'error' },
    backgrounds: { disable: true },
    viewport: {
      options: {
        desktop: { name: 'Desktop 1280×800', styles: { width: '1280px', height: '800px' } },
        phone: { name: 'Phone 390×844', styles: { width: '390px', height: '844px' } },
      },
    },
  },
};
export default preview;
