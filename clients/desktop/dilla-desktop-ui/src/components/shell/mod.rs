//! Task 8's components (`components/shell/`): the pane widths, the splitter between panes and the channel header's button.

pub mod bar_button;
pub mod panes;
pub mod splitter;

pub use bar_button::BarButton;
pub use panes::{Pane, PaneInput, Panes, clamp_panes};
pub use splitter::{Splitter, SplitterKey, splitter_key};
