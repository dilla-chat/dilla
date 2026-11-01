import { render, screen } from '@testing-library/react';
import { describe, it, expect } from 'vitest';
import { Tag } from './Tag.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

describe('Tag', () => {
  it('renders text tags', () => {
    render(<><Tag kind="bot" /><Tag kind="web" /><Tag kind="admin" /><Tag kind="playbackOnly" /><Tag kind="live" /></>);
    expect(screen.getByText('live')).toBeInTheDocument();
    expect(screen.getByText('bot')).toBeInTheDocument();
    expect(screen.getByText('web')).toBeInTheDocument();
    expect(screen.getByText('admin')).toBeInTheDocument();
    expect(screen.getByText('playback only')).toBeInTheDocument();
  });
  it('renders the readable-channel glyph with an accessible name and no visible text', () => {
    render(<Tag kind="readable" />);
    const glyph = screen.getByRole('img', { name: 'Readable by this server' });
    expect(glyph.querySelector('[data-icon="eye"]')).not.toBeNull();
    expect(glyph.textContent).toBe('');
  });
  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><Tag kind="web" /><Tag kind="readable" /></div>);
    await expectNoAxeViolations(container);
  });
});
