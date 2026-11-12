//! Task 11's components (`components/threads/`): the reply line under a reply's head, the thread chip, the reply bar over
//! the composer and the thread pane's frame and rows. Ports packages/ui `ReplyChip`, `ThreadPane` and the reply and thread
//! parts of `MessageRow` (their CSS), styled from `tokens` only. Every height is a function of the tokens, so the screens
//! that place these (the timeline's `message_extras`, the composer, the shell's aside) can add it to their own layout.

pub mod pane;
pub mod reply_bar;
pub mod reply_line;
pub mod thread_chip;
pub mod thread_row;

pub use pane::{PaneParts, pane};
pub use reply_bar::ReplyBar;
pub use reply_line::{ReplyLine, ReplyState};
pub use thread_chip::ThreadChip;
pub use thread_row::{ThreadRow, ThreadRowKind};
