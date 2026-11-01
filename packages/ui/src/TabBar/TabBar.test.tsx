import { fireEvent, render, screen } from '@testing-library/react';
import { describe, it, expect, vi } from 'vitest';
import { TabBar, type TabBarItem } from './TabBar.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const items = (over: Partial<Record<string, Partial<TabBarItem>>> = {}): TabBarItem[] => [
  { id: 'servers', label: 'servers', href: '/s', icon: 'server', current: true, ...over.servers },
  { id: 'dms', label: 'dms', href: '/d', icon: 'message-square', current: false, ...over.dms },
  { id: 'activity', label: 'activity', href: '/a', icon: 'activity', current: false, ...over.activity },
  { id: 'you', label: 'you', href: '/y', icon: 'user', current: false, ...over.you },
];

describe('TabBar', () => {
  it('renders four links in order inside a named navigation', () => {
    render(<TabBar label="main" items={items()} onSelect={() => {}} />);
    expect(screen.getByRole('navigation', { name: 'main' })).toHaveClass('d-tab-bar');
    const links = screen.getAllByRole('link');
    expect(links.map(l => l.textContent)).toEqual(['servers', 'dms', 'activity', 'you']);
    expect(links.map(l => l.getAttribute('href'))).toEqual(['/s', '/d', '/a', '/y']);
  });
  it('marks only the current item with aria-current', () => {
    render(<TabBar label="main" items={items()} onSelect={() => {}} />);
    const links = screen.getAllByRole('link');
    expect(links[0]).toHaveAttribute('aria-current', 'page');
    expect(links.slice(1).some(l => l.hasAttribute('aria-current'))).toBe(false);
  });
  it('selects on a plain click and prevents the navigation', () => {
    const onSelect = vi.fn();
    render(<TabBar label="main" items={items()} onSelect={onSelect} />);
    const notPrevented = fireEvent.click(screen.getByRole('link', { name: 'dms' }));
    expect(onSelect).toHaveBeenCalledWith('dms');
    expect(notPrevented).toBe(false);
  });
  it.each([['ctrl', { ctrlKey: true }], ['meta', { metaKey: true }], ['shift', { shiftKey: true }], ['middle', { button: 1 }]])(
    'leaves a %s click to the browser', (_n, init) => {
      const onSelect = vi.fn();
      render(<TabBar label="main" items={items()} onSelect={onSelect} />);
      const notPrevented = fireEvent.click(screen.getByRole('link', { name: 'dms' }), init);
      expect(onSelect).not.toHaveBeenCalled();
      expect(notPrevented).toBe(true);
    });
  it('shows a decorative dot for a count (Signal §5.10) and names the link with countLabel', () => {
    render(<TabBar label="main" items={items({ dms: { count: 120, countLabel: 'dms, 120 unread' }, activity: { count: 3, countLabel: 'activity, 3 unread' } })} onSelect={() => {}} />);
    const dms = screen.getByRole('link', { name: 'dms, 120 unread' });
    expect(dms.querySelector('.d-tab-bar__dot')).toHaveAttribute('aria-hidden', 'true');
    expect(dms.querySelector('.d-tab-bar__dot')).toHaveTextContent('');
    expect(screen.getByRole('link', { name: 'activity, 3 unread' }).querySelector('.d-tab-bar__dot')).not.toBeNull();
  });
  it('is a list for VoiceOver, and a count without a countLabel is still announced', () => {
    render(<TabBar label="main" items={items({ dms: { count: 3 }, activity: { count: 120 } })} onSelect={() => {}} />);
    expect(screen.getByRole('list')).toBeInTheDocument();
    expect(screen.getAllByRole('listitem')).toHaveLength(4);
    expect(screen.getByRole('link', { name: 'dms, 3' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'activity, 99+' })).toBeInTheDocument();
  });
  it('shows no dot and no aria-label without a count, or with 0', () => {
    render(<TabBar label="main" items={items({ dms: { count: 0, countLabel: 'dms, 0 unread' } })} onSelect={() => {}} />);
    expect(document.querySelectorAll('.d-tab-bar__dot')).toHaveLength(0);
    expect(screen.getByRole('link', { name: 'dms' })).not.toHaveAttribute('aria-label');
  });
  it('has no axe violations', async () => {
    const { container } = render(<div className="d-root"><TabBar label="main" items={items({ dms: { count: 3, countLabel: 'dms, 3 unread' } })} onSelect={() => {}} /></div>);
    await expectNoAxeViolations(container);
  });
});
