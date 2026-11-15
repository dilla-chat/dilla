//! `attachments`: Task 12's slice of [`super::AppState`]: the files waiting to be sent per channel (the worker's
//! `tray:<channel>` slices).
//!
//! Nothing here holds key material: a tray entry names its file and its phase only (the worker's `publishTray`), never
//! its descriptor. GIF search results and opened files are not published: the view that asked holds them.

use std::collections::HashMap;

use super::ChannelId;

/// tray.ts `TrayPhase`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TrayPhase {
    Reading,
    Preparing,
    Uploading,
    Ready,
    Failed,
}

impl TrayPhase {
    /// The phase's name (`data-phase`, the `shell.tray.<phase>` string key).
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Reading => "reading",
            Self::Preparing => "preparing",
            Self::Uploading => "uploading",
            Self::Ready => "ready",
            Self::Failed => "failed",
        }
    }
}

/// core-port.ts `TrayItem`: one file waiting to be sent.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TrayItem {
    pub id: String,
    pub name: String,
    pub size: u64,
    pub mime: String,
    /// A renderable image within the cap: it gets a preview card.
    pub image: bool,
    pub phase: TrayPhase,
    /// The failure's code (`E_ATTACHMENT_TOO_LARGE`, a route's code), else empty.
    pub reason: String,
}

/// Task 12's slice.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct AttachmentState {
    /// `tray:<channel>`: absent while a channel's tray is empty.
    pub trays: HashMap<ChannelId, Vec<TrayItem>>,
    /// The thumbnails a drawn fixture's cards show, by attachment name and size: set only by the automation build's
    /// fixture (`dilla-desktop/src/fixture/attachments.rs`), where there is no client to open them. Empty otherwise.
    pub fixture_thumbs: HashMap<(String, u64), Vec<u8>>,
    /// The whole pictures of a drawn fixture's images (what its lightbox shows), by attachment name and size: set only
    /// by the same fixture facet. Empty otherwise.
    pub fixture_pictures: HashMap<(String, u64), std::sync::Arc<[u8]>>,
}

impl AttachmentState {
    /// The tray of `channel`, empty when it has none.
    pub fn tray(&self, channel: ChannelId) -> &[TrayItem] {
        self.trays.get(&channel).map_or(&[], Vec::as_slice)
    }
}
