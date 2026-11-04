import type { Meta, StoryObj } from '@storybook/react-vite';
import { expect, userEvent, waitFor, within } from 'storybook/test';
import { EmojiGrid } from './EmojiGrid.tsx';

// REACTION_EMOJI of task 9 (Mesh chat-app.jsx:389-393) with the L-COPY-03 names, quoted (en.ts gains them in task 9).
const EMOJI = ['👍', '👎', '❤️', '😄', '😂', '😢', '😡', '😍', '🎉', '🔥', '💯', '✨', '🙏', '👀', '🤔', '😴',
  '🛠', '🚀', '✅', '❌', '💡', '📌', '🐛', '📦', '☕', '🍕', '🌮', '🎨', '🎵', '🌙', '☀️', '🦀'];
const NAMES = ['thumbs up', 'thumbs down', 'red heart', 'grinning face', 'tears of joy', 'crying face', 'angry face', 'heart eyes',
  'party popper', 'fire', 'hundred points', 'sparkles', 'folded hands', 'eyes', 'thinking face', 'sleeping face',
  'hammer and wrench', 'rocket', 'check mark', 'cross mark', 'light bulb', 'pushpin', 'bug', 'package',
  'hot beverage', 'pizza', 'taco', 'artist palette', 'musical note', 'crescent moon', 'sun', 'crab'];

const meta = {
  title: 'Conversation/EmojiGrid', component: EmojiGrid,
  args: { label: 'pick a reaction', items: EMOJI.map((emoji, i) => ({ emoji, name: NAMES[i] ?? '' })), onPick: () => {}, onClose: () => {} },
  decorators: [Story => <div style={{ maxWidth: 720 }}><Story /></div>],
} satisfies Meta<typeof EmojiGrid>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Grid: Story = {};
/** Task 7 (§5.5): the composer's picker as ComposerArea opens it: the search field (focused) over the grid and the name line. */
export const SignalComposerPicker: Story = {
  globals: { theme: 'signal' },
  args: { label: 'Emoji', search: { label: 'Search emoji', placeholder: 'Search emoji' } },
};
/** The searchable preview follows the prototype independently of the roving grid's first tab stop. */
export const SignalPreview: Story = {
  ...SignalComposerPicker,
  args: { ...SignalComposerPicker.args, items: [
    { emoji: '👍', name: 'thumbs up' }, { emoji: '👎', name: 'thumbs down' }, { emoji: '🛡️', name: 'shield' },
  ] },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    const footer = canvasElement.querySelector('.d-emoji-grid__foot');
    await waitFor(() => expect(canvas.getByRole('searchbox')).toHaveFocus());
    await expect(footer).toHaveTextContent('🛡️:shield:');
    await userEvent.keyboard('{ArrowDown}{End}');
    await expect(canvas.getByRole('button', { name: 'shield' })).toHaveFocus();
    const thumbs = canvas.getByRole('button', { name: 'thumbs up' });
    await userEvent.hover(thumbs);
    await userEvent.unhover(thumbs);
    await expect(footer).toHaveTextContent('👍:thumbs_up:');
    await userEvent.keyboard('{Home}{ArrowRight}');
    await expect(canvas.getByRole('button', { name: 'thumbs down' })).toHaveFocus();
    await expect(footer).toHaveTextContent('👎:thumbs_down:');
  },
};
