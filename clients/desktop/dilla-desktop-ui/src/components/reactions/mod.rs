//! Task 10's components (`components/reactions/`): the reaction bar (`ReactionBar`), the emoji grid (`EmojiGrid`) and the
//! who-reacted tooltip (`ReactionTooltip`), ported from packages/ui/src/{ReactionBar,EmojiGrid,ReactionTooltip}.

pub mod bar;
pub mod grid;
pub mod tooltip;

pub use bar::{Chip, Decorate, ReactionBar, Slot, bar_height, chip_width, wrap_lines};
pub use grid::{EmojiGrid, EmojiItem, GridEvent, Part, grid_next};
pub use tooltip::ReactionTooltip;
