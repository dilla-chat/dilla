import { it, expect, vi } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { DesktopTitlebar } from './DesktopTitlebar.tsx';

it('opens the app menu without passing arbitrary native arguments and shows the full instance in its title', () => {
 const send = vi.fn().mockResolvedValue(undefined);
 render(<DesktopTitlebar bridge={{ send } as never} info={{ instance: 'https://chat.test/space/', platform: 'linux' } as never} />);
 fireEvent.click(screen.getByRole('button', { name: 'Dilla menu' }));
 expect(send).toHaveBeenCalledWith({ type: 'window-menu' });
 expect(screen.getByText('chat.test/space/')).toHaveAttribute('title', 'https://chat.test/space/');
 cleanup();
});


it('offers bounded window controls and a separate close control for the login card', () => {
 const send = vi.fn().mockResolvedValue(undefined);
 render(<DesktopTitlebar bridge={{ send } as never} info={{ instance: 'https://chat.test/', platform: 'linux' } as never} />);
 fireEvent.click(screen.getByRole('button', { name: 'Minimize window' }));
 expect(send).toHaveBeenLastCalledWith({ type: 'window-control', action: 'minimize' });
 fireEvent.click(screen.getByRole('button', { name: 'Maximize or restore window' }));
 expect(send).toHaveBeenLastCalledWith({ type: 'window-control', action: 'toggle-maximize' });
 fireEvent.click(screen.getByRole('button', { name: 'Close login window' }));
 expect(send).toHaveBeenLastCalledWith({ type: 'window-control', action: 'close' });
 cleanup();
});
