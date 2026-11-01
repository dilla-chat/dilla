import type { Meta, StoryObj } from '@storybook/react-vite';
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
