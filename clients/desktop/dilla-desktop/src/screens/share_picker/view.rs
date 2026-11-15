//! The open picker drawn over its window: dilla-desktop-ui's `SharePicker` with the latest enumeration's tiles.

use dilla_desktop_ui::components::share_picker::{PickerLabels, PickerTile, SharePicker, TileKind};
use dilla_media::sources::desktop::SourceKind;
use gpui::{AnyElement, App, IntoElement, Window};

use crate::strings::t;

/// The picker of `window`, or its application chooser, when one is open (the shell's overlays).
pub fn overlay(window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    if let Some(chooser) = super::apps::overlay(window, cx) {
        return Some(chooser);
    }
    let id = window.window_handle().window_id();
    let picker = super::picker_of(id, cx)?;
    let tiles: Vec<PickerTile> = picker
        .tiles
        .iter()
        .flatten()
        .map(|t| PickerTile {
            key: t.key.clone(),
            name: t.source.title.clone().into(),
            kind: match t.source.kind {
                SourceKind::Screen => TileKind::Screen,
                SourceKind::Window => TileKind::Window,
            },
            thumbnail: t.image.clone(),
        })
        .collect();
    let labels = PickerLabels {
        title: t("callStage.share.title").into(),
        screens: t("callStage.share.pickerScreens").into(),
        windows: t("callStage.share.pickerWindows").into(),
        share: t("callStage.share.pickerShare").into(),
        cancel: t("callStage.share.pickerCancel").into(),
        empty: t("callStage.share.pickerEmpty").into(),
    };
    Some(
        SharePicker::new("share-picker", labels)
            .tiles(tiles)
            .tab(picker.tab)
            .selected(picker.selected.clone())
            .on_tab(move |tab, _, cx| super::set_tab(id, tab, cx))
            .on_select(move |key, _, cx| super::select(id, key, cx))
            .on_share(move |key, _, cx| {
                super::share(id, &key, cx);
            })
            .on_cancel(move |_, cx| super::cancel(id, cx))
            .into_any_element(),
    )
}
