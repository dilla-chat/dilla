import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, it } from 'vitest';
import { Tooltip } from '../Tooltip/Tooltip.tsx';
import { ReactionTooltip } from './ReactionTooltip.tsx';
const text = 'Ada, Björn and you reacted with 🛡️';
it.each(['hover', 'focus'] as const)('shows the reaction text and at most six avatars on %s through Tooltip', async mode => {
  const user = userEvent.setup();
  render(<Tooltip label={text} content={<ReactionTooltip text={text} avatars={['Ada', 'Björn', 'Mira', 'Sven', 'Liv', 'Kari', 'Ole'].map(name => ({ name }))} />}><button>🛡️ 7</button></Tooltip>);
  if (mode === 'hover') await user.hover(screen.getByRole('button')); else await user.tab();
  const tip = await screen.findByRole('tooltip');
  expect(tip).toHaveTextContent(text);
  expect(tip.querySelectorAll('.d-avatar')).toHaveLength(6);
});
