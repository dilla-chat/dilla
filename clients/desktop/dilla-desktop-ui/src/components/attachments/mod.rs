//! Task 12's components (`components/attachments/`): the ports of `packages/ui/src/{AttachmentCard, AttachmentTray,
//! DropOverlay, Lightbox, MediaPlayer, GifPicker}`. They draw what they are given; the app's `screens::attachments`
//! holds the state and does the work. Every size is the ui CSS's, through the tokens; a constant without a token names
//! the CSS it comes from.

pub mod card;
pub mod drop_overlay;
pub mod gif_picker;
pub mod lightbox;
pub mod player;
pub mod tray;

use gpui::{App, ClickEvent, Window};

/// A click handler of these components.
pub type OnClick = std::rc::Rc<dyn Fn(&ClickEvent, &mut Window, &mut App) + 'static>;

/// tokens.css `--row-gap` (0.125rem): the gap between a message's attachment cards (app.css `.dw-attachments`).
pub const ROW_GAP: f32 = 2.0;
/// The body line height of a component's text (`body { line-height: 1.5 }`).
pub const LEADING: f32 = 1.5;
