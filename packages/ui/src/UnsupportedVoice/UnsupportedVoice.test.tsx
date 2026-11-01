import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, it, expect, vi } from 'vitest';
import { axe } from 'vitest-axe';
import { UnsupportedVoice, type UnsupportedVoiceProps } from './UnsupportedVoice.tsx';
import meta from './UnsupportedVoice.stories.tsx';

const labels: UnsupportedVoiceProps['labels'] = {
  title: 'Voice does not work in this browser',
  body: 'This browser lacks a feature dilla needs for voice. Use a current Chromium-based browser or Firefox.',
  close: 'Close',
};

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('UnsupportedVoice', () => {
  it('is a dialog titled and explained by its copy; close and Escape call onClose', async () => {
    const onClose = vi.fn();
    render(<UnsupportedVoice open labels={labels} onClose={onClose} />);
    expect(screen.getByRole('dialog', { name: 'Voice does not work in this browser' })).toBeInTheDocument();
    expect(screen.getByText(labels.body)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: /Close/ }));
    await userEvent.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  it('shows nothing while closed', () => {
    render(<UnsupportedVoice open={false} labels={labels} onClose={() => {}} />);
    expect(screen.queryByText(labels.body)).toBeNull();
  });

  it.each(['signal', 'light', 'high-contrast'])('passes axe at its default story args in %s', async theme => {
    document.documentElement.dataset.theme = theme;
    const { container } = render(<div className="d-root"><UnsupportedVoice {...(meta.args as UnsupportedVoiceProps)} /></div>);
    // jsdom has no layout or canvas, so contrast is left to the Storybook axe run in a real browser.
    expect((await axe(container, { rules: { 'color-contrast': { enabled: false } } })).violations).toEqual([]);
  });
});
