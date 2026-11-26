# Members aside missing at startup: diagnosis

Worktree: /home/thim/Repositories/dilla/.claude/worktrees/signal-design (line numbers as of 2026-11-26; shell.rs carries
5 uncommitted lines from another builder, so they may shift a little). Diagnosed by reading the code. Nothing was built or edited.

## Root cause

The members pane is drawn only when the shell's **route** is `Route::Channel`. At startup with a running client, the
route stays `Route::Root` after the shell picks its first channel, because the auto-selection never updates the history.

1. `app.rs:1395-1403` `show_shell`: `ShellView::new(first, Some(client), None, ...)` with the client's *first* state
   snapshot. The server list hasn't been read yet, so it has no communities.
2. `shell.rs:388` builds the history as `History::new(Route::Root)`. `shell.rs:420` runs `settle_selection`, which finds no
   community. `shell.rs:423-424` sets `history = History::new(this.current_route())`, which gives `Route::Root` because
   `self.community` is `None` (`current_route`, `shell.rs:567-575`).
3. When the server list lands, `set_state` (`shell.rs:477-487`) calls `settle_selection`. That sets `self.community` and
   calls `focus_channel(first)` (`shell.rs:500-538`), so the channel, header, log and composer all show it. But
   `settle_selection` / `focus_channel` never `push` or `replace` the history. `self.route()` (`shell.rs:590`) stays
   `Route::Root` for the rest of the session until the user navigates.
4. `members::pane` (`screens/members/mod.rs:1045-1050`) does
   `let Route::Channel { community, .. } = route else { return None; };`. `slots::aside(self.route(), ...)`
   (`slots.rs:267-268`, called from `shell.rs:~1744`) therefore returns `None`. The threads pane ignores its route
   argument (`threads/mod.rs:521`), so only the members pane breaks.
5. The header button still exists and its handler runs: `header.rs:77` calls `on_toggle`, then `toggle_aside`
   (`shell.rs:987-994`), then `members::toggle_pane` (`members/mod.rs:1075-1083`), which flips `MembersUi.open`
   true->false->true. But `pane` returns `None` for the route whichever way the flag is set, so nothing appears.
   The button never shows as pressed either: `shell.rs:~1974-1981` needs `aside.is_some()`. That matches "can't click
   the members icon".

## Why switching channel fixes it

A click goes through `ShellView::select` (`shell.rs:~700-725`), which runs
`self.history.push(Route::Channel { community, channel: Some(channel) })`. Now `route()` is a channel route,
`members::pane` matches, and `MembersUi.open` (default `true`, `members/mod.rs:135-140`) takes effect. The pane appears
and the toggle works.

## Why the tests miss it

Every fixture and test builds the shell with an already populated `AppState` (`world()`, `app.rs:290-367` fixtures).
So `current_route()` at `new` is already `Channel { .. }`. The tests that click `header-members`
(`tests/shell.rs:163,183`, `tests/bot.rs:192`) launch fixtures. None of them starts empty and then receives the server
list through `set_state`, which is the running-client path.

## Minimal fix

`clients/desktop/dilla-desktop/src/screens/shell.rs`, at the end of `settle_selection` (non-DM branch, after the
`if !valid { ... }` block): when the history head is a selection route (`Root` or `Channel`) and the selection settled
somewhere else, replace the head, as the redirect `History::replace` already describes ("a route that settled on its
default"):

```rust
let settled = self.current_route();
if matches!(self.history.current(), Route::Root | Route::Channel { .. })
    && *self.history.current() != settled
{
    self.history.replace(settled);
}
```

This doesn't touch Settings or DM heads (the DM branch returns earlier). It also covers a server deleted under the open
route. You could then drop the re-creation at `shell.rs:423-424`, but you don't have to.

Alternative, narrower but weaker: make `members::pane` read `ctx.community` (the synced `SlotCx`) instead of the route.
That fixes the symptom, but the stale `Route::Root` head stays for back/forward and `home_shown`. Prefer the history fix.

## Failing test (shell.rs `mod tests`, the same style as `the_no_server_view_waits_for_the_server_list`)

```rust
/// A running client's first state has no servers yet: once the list lands and the shell opens the first text channel,
/// the route is that channel's, so the member list (open by default) is drawn and the header's Members toggles it.
#[gpui::test]
fn the_first_channel_opened_after_the_server_list_lands_is_the_route(cx: &mut gpui::TestAppContext) {
    cx.update(crate::install);
    let (shell, vcx) = cx.add_window_view(move |window, cx| {
        ShellView::new(Arc::new(AppState::default()), None, None,
            Clock::Fixed { now: 0, utc: true }, window, cx)
    });
    vcx.run_until_parked();
    shell.update(vcx, |s, cx| s.set_state(world(), cx));
    vcx.run_until_parked();
    let at = Route::Channel { community: CommunityId([1; 16]), channel: Some(ChannelId([2; 16])) };
    shell.read_with(vcx, |s, _| {
        assert_eq!(s.channel, Some(ChannelId([2; 16])), "the server opens on its first channel");
        assert_eq!(s.route(), &at, "the settled channel is the route, not Root");   // fails today: Root
    });
    // The members aside is drawn (render syncs its slot context), and the toggle closes it.
    vcx.draw(/* or vcx.update(|w, cx| w.refresh()) + run_until_parked, as the crate's other draw tests do */);
    let drawn = |vcx: &mut gpui::VisualTestContext| vcx.update(|window, cx| {
        let route = shell.read(cx).route().clone();
        crate::slots::aside(&route, window, cx).is_some()
    });
    assert!(drawn(vcx), "the member list is open by default");                       // fails today
    vcx.update(|window, cx| crate::screens::members::toggle_pane(window.window_handle().window_id(), cx));
    assert!(!drawn(vcx), "Members closes it");
}
```

The first `route()` assertion alone proves the bug: today it reads `Route::Root`. An e2e variant could also be added to
`tests/shell.rs`, but the automation fixtures start populated, so it would need a fixture that delivers communities
after the first frame. The unit test is cheaper.

## Cheap confirmation without a build

Start the real app (not a fixture) and press Alt+Left. Today there's no history entry before `Root`. After you click
any channel, Alt+Left goes "back" to `Root`, and the member pane disappears again. That confirms the head is `Root`.
