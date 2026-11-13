//! State an area keeps for one window (integration 3, final review C1 and the sweep): an app-wide map from window to
//! the area's value, so one window's updates never reach another's state, client or account. Every type kept here is
//! pruned when its window closes (`windows::on_closed` runs [`window_closed`]); an area that holds a client in its value
//! drops it in its own `release`.

use std::collections::HashMap;

use gpui::{App, Global, WindowId};

/// One area's values, by window. `scratch` is what a late write for a closed window gets: a throwaway value, reset on
/// every such write, so a closed window never gets an entry back (re-review minor).
pub struct PerWindow<T>(HashMap<WindowId, T>, Option<T>);

impl<T: 'static> Global for PerWindow<T> {}

impl<T> Default for PerWindow<T> {
    fn default() -> Self {
        Self(HashMap::new(), None)
    }
}

/// Drops one kept type's value of a closed window.
type Prune = fn(WindowId, &mut App);

/// How to drop every kept type's value of a closed window, and the windows that closed.
#[derive(Default)]
struct Pruners(
    Vec<(std::any::TypeId, Prune)>,
    std::collections::HashSet<WindowId>,
);

fn closed(window: WindowId, cx: &App) -> bool {
    cx.try_global::<Pruners>()
        .is_some_and(|p| p.1.contains(&window))
}

impl Global for Pruners {}

fn prune<T: 'static>(window: WindowId, cx: &mut App) {
    if cx.has_global::<PerWindow<T>>() {
        let pw = cx.global_mut::<PerWindow<T>>();
        pw.0.remove(&window);
        pw.1 = None;
    }
}

fn map<T: 'static>(cx: &mut App) -> &mut HashMap<WindowId, T> {
    if !cx.has_global::<PerWindow<T>>() {
        cx.set_global(PerWindow::<T>::default());
        let id = std::any::TypeId::of::<T>();
        let pruners = cx.default_global::<Pruners>();
        if !pruners.0.iter().any(|(t, _)| *t == id) {
            pruners.0.push((id, prune::<T>));
        }
    }
    &mut cx.global_mut::<PerWindow<T>>().0
}

/// The window's value, made with `Default` on first use.
pub fn get_mut<T: Default + 'static>(window: WindowId, cx: &mut App) -> &mut T {
    if closed(window, cx) {
        map::<T>(cx);
        let pw = cx.global_mut::<PerWindow<T>>();
        return pw.1.insert(T::default());
    }
    map::<T>(cx).entry(window).or_default()
}

/// The window's value, if it has one.
pub fn get<T: 'static>(window: WindowId, cx: &App) -> Option<&T> {
    cx.try_global::<PerWindow<T>>()?.0.get(&window)
}

/// Sets (`Some`) or drops (`None`) the window's value.
pub fn set<T: 'static>(window: WindowId, value: Option<T>, cx: &mut App) {
    match value {
        Some(v) => {
            if !closed(window, cx) {
                map::<T>(cx).insert(window, v);
            }
        }
        None => {
            if cx.has_global::<PerWindow<T>>() {
                cx.global_mut::<PerWindow<T>>().0.remove(&window);
            }
        }
    }
}

/// Every window's value, for a release that walks them all.
pub fn each_mut<T: 'static>(cx: &mut App, mut f: impl FnMut(WindowId, &mut T)) {
    if cx.has_global::<PerWindow<T>>() {
        for (w, v) in cx.global_mut::<PerWindow<T>>().0.iter_mut() {
            f(*w, v);
        }
    }
}

/// Drops the values for which `keep` answers false.
pub fn retain<T: 'static>(cx: &mut App, mut keep: impl FnMut(WindowId, &mut T) -> bool) {
    if cx.has_global::<PerWindow<T>>() {
        cx.global_mut::<PerWindow<T>>().0.retain(|w, v| keep(*w, v));
    }
}

/// `window` closed: every kept type lets go of its value.
pub fn window_closed(window: WindowId, cx: &mut App) {
    cx.default_global::<Pruners>().1.insert(window);
    let pruners: Vec<Prune> = cx
        .try_global::<Pruners>()
        .map(|p| p.0.iter().map(|(_, f)| *f).collect())
        .unwrap_or_default();
    for f in pruners {
        f(window, cx);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[derive(Default, Debug, PartialEq)]
    struct Count(u32);

    /// Two windows keep separate values, and a closed window's value goes.
    #[gpui::test]
    fn each_window_keeps_its_own_and_a_closed_one_goes(cx: &mut gpui::TestAppContext) {
        let a = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let b = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        cx.update(|cx| {
            get_mut::<Count>(a, cx).0 = 1;
            get_mut::<Count>(b, cx).0 = 2;
            assert_eq!(get::<Count>(a, cx), Some(&Count(1)));
            assert_eq!(get::<Count>(b, cx), Some(&Count(2)));
            window_closed(a, cx);
            assert_eq!(get::<Count>(a, cx), None);
            assert_eq!(get::<Count>(b, cx), Some(&Count(2)));
            // Re-review minor: a late write for a closed window (an answer landing after the close) makes no entry.
            get_mut::<Count>(a, cx).0 = 9;
            set(a, Some(Count(9)), cx);
            assert_eq!(get::<Count>(a, cx), None, "a closed window keeps nothing");
        });
    }
}
