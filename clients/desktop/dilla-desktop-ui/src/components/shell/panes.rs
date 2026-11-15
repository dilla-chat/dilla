//! The widths the shell's two panes may have (packages/ui `AppShell.tsx` `clampPanes`, web3.8): the sidebar in
//! `[12rem, 24rem]`, the aside in `[14rem, 28rem]`, and main (the window less the rail, the sidebar and a column aside) at
//! least `24rem`. When even the minimums break the main rule, the minimums win. The sidebar is placed first; the aside takes
//! what is left. Pure: the shell keeps the requested widths and clamps what it shows, never what it stores.

/// One rem in logical px (the root font size of the web client).
pub const REM: f32 = 16.0;
const SIDEBAR_REM: (f32, f32) = (12.0, 24.0);
const ASIDE_REM: (f32, f32) = (14.0, 28.0);
const MAIN_MIN_REM: f32 = 24.0;
/// `--sidebar-w`: the sidebar's width when nothing was dragged.
pub const SIDEBAR_DEFAULT: f32 = 244.0;
/// `--members-w` and `--thread-w`: the aside's default column widths.
pub const ASIDE_DEFAULT_MEMBERS: f32 = 232.0;
pub const ASIDE_DEFAULT_THREAD: f32 = 340.0;
/// The shell width from which the aside is a column of the grid (`COLUMN_REM`, 64rem); below it, an overlay.
pub const COLUMN_FROM: f32 = 64.0 * REM;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Pane {
    Sidebar,
    Aside,
}

/// What [`clamp_panes`] needs: the shell's width, the rail's, the two requested widths, and whether the aside is a column.
#[derive(Clone, Copy, Debug)]
pub struct PaneInput {
    pub shell: f32,
    pub rail: f32,
    pub sidebar: f32,
    pub aside: f32,
    pub aside_column: bool,
}

/// The widths shown.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Panes {
    pub sidebar: f32,
    pub aside: f32,
}

fn own(pane: Pane) -> (f32, f32) {
    match pane {
        Pane::Sidebar => SIDEBAR_REM,
        Pane::Aside => ASIDE_REM,
    }
}

/// The limits of `pane` given the other pane's width: its own rem range, and main staying at 24rem. The minimum wins.
pub fn range(pane: Pane, i: &PaneInput, other: f32) -> (f32, f32) {
    let (lo, hi) = own(pane);
    let min = lo * REM;
    let taken = i.rail
        + match pane {
            Pane::Sidebar if i.aside_column => other,
            Pane::Sidebar => 0.0,
            Pane::Aside => other,
        };
    let room = if i.shell > 0.0 {
        i.shell - taken - MAIN_MIN_REM * REM
    } else {
        f32::INFINITY
    };
    (min, min.max((hi * REM).min(room)))
}

pub fn clamp_panes(i: &PaneInput) -> Panes {
    let aside_first = i.aside.clamp(ASIDE_REM.0 * REM, ASIDE_REM.1 * REM);
    let (lo, hi) = range(Pane::Sidebar, i, aside_first);
    let sidebar = i.sidebar.clamp(lo, hi);
    let (lo, hi) = range(Pane::Aside, i, sidebar);
    Panes {
        sidebar,
        aside: i.aside.clamp(lo, hi),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn input(shell: f32, sidebar: f32, aside: f32, column: bool) -> PaneInput {
        PaneInput {
            shell,
            rail: 56.0,
            sidebar,
            aside,
            aside_column: column,
        }
    }

    #[test]
    fn the_defaults_pass_at_1280() {
        let p = clamp_panes(&input(1280.0, SIDEBAR_DEFAULT, ASIDE_DEFAULT_MEMBERS, true));
        assert_eq!(p.sidebar, 244.0);
        assert_eq!(p.aside, 232.0);
    }

    #[test]
    fn the_sidebar_stays_in_its_rem_range() {
        assert_eq!(
            clamp_panes(&input(1280.0, 10.0, 232.0, true)).sidebar,
            192.0
        );
        assert_eq!(
            clamp_panes(&input(1600.0, 900.0, 232.0, true)).sidebar,
            384.0
        );
    }

    #[test]
    fn main_keeps_24rem_and_the_minimums_win_over_it() {
        // 800 wide, no column aside: the widest sidebar leaves main at 24rem (384): 800 - 56 - 384 = 360 for the sidebar.
        assert_eq!(
            clamp_panes(&input(800.0, 900.0, 232.0, false)).sidebar,
            360.0
        );
        // 600 wide: the room is under the minimum, so the minimum (12rem) holds.
        assert_eq!(
            clamp_panes(&input(600.0, 300.0, 232.0, false)).sidebar,
            192.0
        );
    }

    #[test]
    fn a_column_aside_takes_room_from_the_sidebar() {
        let p = clamp_panes(&input(1100.0, 384.0, 340.0, true));
        assert!(
            p.sidebar + p.aside + 56.0 + 384.0 <= 1100.0 + 0.001,
            "{p:?}"
        );
    }

    #[test]
    fn an_unmeasured_shell_only_applies_the_own_ranges() {
        assert_eq!(clamp_panes(&input(0.0, 300.0, 300.0, true)).sidebar, 300.0);
    }
}
