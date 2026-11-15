//! The application chooser of the share's sound section (parity plan Package 3; beta.5 main.ts `dilla:app-audio`'s
//! message box): "Share application sound", "Choose the application others will hear", one button per application
//! that plays (Dilla's own process tree left out), Cancel; with none, beta.5's "No playback applications are
//! available…". It opens only from a press on the focused window; the list is read off the UI thread.

use std::collections::HashMap;

use dilla_desktop_ui::a11y::{Accessible, Role};
use dilla_desktop_ui::components::button::{Button, ButtonSize, ButtonVariant};
use dilla_desktop_ui::components::dialog::Dialog;
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, text};
use dilla_media::app_audio::{AppAudioError, AppSource};
use gpui::{
    AnyElement, App, Global, InteractiveElement, IntoElement, ParentElement, Styled, Window,
    WindowId, div, px,
};

use crate::strings::t;

/// The chooser body's gap between its sections (the web share picker's app list).
const BODY_GAP: f32 = 8.0;
/// The gap inside an application row (icon, name, state).
const ROW_GAP: f32 = 6.0;

/// Lists the applications (the real `dilla_media::app_audio::sources`; the tests' fake).
pub type AppLister =
    std::sync::Arc<dyn Fn() -> Result<Vec<AppSource>, AppAudioError> + Send + Sync>;

/// Every window's open chooser: `None` while its list is read.
#[derive(Default)]
pub struct AppChoosers {
    open: HashMap<WindowId, Option<Vec<AppSource>>>,
    lister: Option<AppLister>,
}

impl Global for AppChoosers {}

/// The tests' application list.
pub fn set_lister(lister: AppLister, cx: &mut App) {
    cx.default_global::<AppChoosers>().lister = Some(lister);
}

/// Whether this system has application audio (Linux), or a test list stands in.
pub fn available(cx: &App) -> bool {
    available_now()
        || cx
            .try_global::<AppChoosers>()
            .is_some_and(|c| c.lister.is_some())
}

/// Whether this build has application audio (Linux; macOS and Windows not yet).
pub fn available_now() -> bool {
    cfg!(target_os = "linux")
}

pub fn is_open(window: WindowId, cx: &App) -> bool {
    cx.try_global::<AppChoosers>()
        .is_some_and(|c| c.open.contains_key(&window))
}

/// The applications the open chooser of `window` offers (`None`: not open or not read yet).
pub fn listed(window: WindowId, cx: &App) -> Option<Vec<AppSource>> {
    cx.try_global::<AppChoosers>()?.open.get(&window)?.clone()
}

/// Opens the chooser over `window` after a press on it (the focused window only). False when it did not open.
pub fn open(window: &mut Window, cx: &mut App) -> bool {
    if !window.is_window_active() {
        return false;
    }
    let id = window.window_handle().window_id();
    let choosers = cx.default_global::<AppChoosers>();
    if choosers.open.contains_key(&id) {
        return true;
    }
    choosers.open.insert(id, None);
    let lister = choosers.lister.clone();
    cx.spawn(async move |cx| {
        let found = cx
            .background_executor()
            .spawn(async move {
                match lister {
                    Some(l) => l(),
                    None => dilla_media::app_audio::sources(),
                }
            })
            .await;
        cx.update(|cx| {
            if let Some(slot) = cx.default_global::<AppChoosers>().open.get_mut(&id) {
                *slot = Some(found.unwrap_or_else(|e| {
                    log::warn!("voice: no application list: {e}");
                    Vec::new()
                }));
            }
            super::refresh(id, cx);
        });
    })
    .detach();
    window.refresh();
    true
}

/// Closes the chooser; `chosen` is the application (`None`: Cancel). The choice becomes the share's sound.
pub fn close(window: WindowId, chosen: Option<AppSource>, cx: &mut App) {
    if cx.has_global::<AppChoosers>() {
        cx.global_mut::<AppChoosers>().open.remove(&window);
    }
    if let Some(source) = chosen {
        crate::screens::voice::set_share_app(window, Some(source), cx);
    }
    super::refresh(window, cx);
}

/// The open chooser of `window`, drawn over it.
pub fn overlay(window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let id = window.window_handle().window_id();
    let list = cx.try_global::<AppChoosers>()?.open.get(&id)?.clone();
    let mut body = div().flex().flex_col().gap(px(BODY_GAP));
    match &list {
        Some(apps) if apps.is_empty() => {
            body = body.child(line(
                t("callStage.share.appNone"),
                text::UI,
                line_height(text::UI, 1.5),
                color::FG_2,
            ));
        }
        Some(apps) => {
            body = body
                .child(line(
                    t("callStage.share.appChoose"),
                    text::UI,
                    line_height(text::UI, 1.5),
                    color::FG,
                ))
                .child(line(
                    t("callStage.share.audioAppNote"),
                    text::META,
                    line_height(text::META, 1.35),
                    color::FG_3,
                ));
            let mut rows = div()
                .id("share-app-list")
                .a11y(Role::List, t("callStage.share.appChoose"))
                .flex()
                .flex_col()
                .gap(px(ROW_GAP));
            for app in apps {
                let chosen = app.clone();
                rows = rows.child(
                    Button::new(
                        format!("share-app-{}-{}", app.pid, app.serial),
                        app.label.clone(),
                    )
                    .variant(ButtonVariant::Default)
                    .size(ButtonSize::Dialog)
                    .on_click(move |_, _, cx| close(id, Some(chosen.clone()), cx)),
                );
            }
            body = body.child(rows);
        }
        None => {}
    }
    Some(
        Dialog::new(
            "share-app-chooser",
            t("callStage.share.appTitle"),
            t("callStage.share.appCancel"),
        )
        .body(body)
        .on_close(move |_, cx| close(id, None, cx))
        .into_any_element(),
    )
}
