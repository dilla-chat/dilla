//! Task 28's cross-area tests: flows of the web e2e suites that cross areas, against a real dillad (dilla-testhost).
//!
//! The app is the real `AppView` in a GPUI test window, on a client of the person's (the workspace window a sign-in
//! hands over to) or started by the window itself from its data root (a restart). The other people are headless clients.
//! The window is driven through what its controls call (the palette's keys, the pins dialog's jump, the shell's route);
//! what it shows is read from its client's state and its areas. `#[ignore]`d like every real-dillad suite: the
//! desktop-gpui CI job runs them on Linux with `cargo test -p dilla-desktop --test integration -- --ignored`.

mod support;

use std::rc::Rc;
use std::sync::Arc;
use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::state::threads::OpenThread;
use dilla_client::state::{
    AppState, ChannelId, CommunityId, MessageId, TimelineItem, TimelineItemState,
};
use dilla_desktop::app::{AppView, Launch};
use dilla_desktop::router::Route;
use dilla_desktop::screens::shell::ShellView;
use gpui::{Entity, TestAppContext, VisualTestContext};

use support::world::{self, Host, Person};

const WAIT: Duration = Duration::from_secs(60);

fn runtime() -> tokio::runtime::Runtime {
    tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap()
}

/// A real instance, Ada and Björn signed up and set up, and a community owned by `owner` (0 Ada, 1 Björn) with #general
/// that the other one joined. The sessions are cleared, so each client establishes its own from its device key.
struct World {
    rt: tokio::runtime::Runtime,
    host: Host,
    ada: Person,
    bjorn: Person,
    community: [u8; 16],
}

fn world(tag: &str, owner: usize) -> World {
    let rt = runtime();
    let host = world::start_host(tag, "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let (ada, bjorn) = rt.block_on(async {
        let ada = world::sign_up(&host, &invite, "ada", "Ada", tag).await;
        let bjorn = world::sign_up(&host, &invite, "bjorn", "Björn", tag).await;
        (ada, bjorn)
    });
    let community = if owner == 0 {
        world::community(&host, &ada, "Cross", &[&bjorn])
    } else {
        world::community(&host, &bjorn, "Cross", &[&ada])
    };
    world::clear_session(&ada);
    world::clear_session(&bjorn);
    World {
        rt,
        host,
        ada,
        bjorn,
        community,
    }
}

impl World {
    fn start(&self, p: &Person) -> Client {
        self.rt.block_on(world::start(p))
    }

    fn wait(&self, client: &Client, what: &str, pred: impl Fn(&AppState) -> bool) -> Arc<AppState> {
        self.rt.block_on(world::wait_for(client, WAIT, what, pred))
    }

    fn general(&self, client: &Client) -> ChannelId {
        let c = CommunityId(self.community);
        self.wait(client, "the channel list", |s| {
            s.channel_named(c, "general").is_some()
        })
        .channel_named(c, "general")
        .unwrap()
        .id
    }

    fn open(&self, client: &Client, channel: ChannelId) {
        self.rt
            .block_on(client.open_channel(channel))
            .unwrap()
            .expect("the channel opens");
    }

    /// Sends `body` in `channel` and waits until `client` stored it; answers its id.
    fn send(&self, client: &Client, channel: ChannelId, body: &str) -> MessageId {
        self.rt
            .block_on(client.send_text(channel, body.to_owned()))
            .unwrap()
            .expect("queued");
        self.stored(client, channel, body)
    }

    /// The id of the stored item `body` of `channel` once `client` holds it.
    fn stored(&self, client: &Client, channel: ChannelId, body: &str) -> MessageId {
        let s = self.wait(client, &format!("{body:?} stored"), |s| {
            item(s, channel, body)
                .is_some_and(|i| i.seq.is_some() && i.state == TimelineItemState::Ok)
        });
        item(&s, channel, body).unwrap().msg_id.unwrap()
    }
}

fn item<'a>(s: &'a AppState, channel: ChannelId, body: &str) -> Option<&'a TimelineItem> {
    s.timelines
        .get(&channel)?
        .items
        .iter()
        .find(|i| i.body == body)
}

/// The data root the app lists `p`'s instance in (`<root>/instances/<n>` is its data dir).
fn root_of(p: &Person) -> std::path::PathBuf {
    p.cfg
        .data_dir
        .parent()
        .and_then(|d| d.parent())
        .unwrap()
        .to_path_buf()
}

/// The workspace window on `client` (the sign-in's hand-over), at the test window's size.
fn window_on<'a>(
    cx: &'a mut TestAppContext,
    p: &Person,
    client: &Rc<Client>,
) -> (Entity<AppView>, &'a mut VisualTestContext) {
    cx.executor().allow_parking();
    cx.update(dilla_desktop::install);
    let mut launch = Launch::live(Some(root_of(p)), p.kc.clone());
    launch.shipping = false;
    launch.resume = false;
    launch.handoff = Some(client.clone());
    cx.add_window_view(move |window, cx| AppView::new(launch, window, cx))
}

/// Draws and runs the window's work once.
fn pump(vcx: &mut VisualTestContext) {
    vcx.run_until_parked();
    vcx.executor().advance_clock(Duration::from_millis(20));
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    std::thread::sleep(Duration::from_millis(20));
}

/// Pumps the window until `pred` holds (at most [`WAIT`]).
fn until(
    vcx: &mut VisualTestContext,
    what: &str,
    mut pred: impl FnMut(&mut VisualTestContext) -> bool,
) {
    let deadline = Instant::now() + WAIT;
    loop {
        pump(vcx);
        if pred(vcx) {
            return;
        }
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
    }
}

fn shell_of(app: &Entity<AppView>, vcx: &mut VisualTestContext) -> Entity<ShellView> {
    app.read_with(vcx, |a, _| a.shell())
        .expect("the window shows the shell")
}

/// The shell goes to `channel` of the world's community, as a click on its row does.
fn go_to(
    app: &Entity<AppView>,
    vcx: &mut VisualTestContext,
    community: [u8; 16],
    channel: ChannelId,
) {
    let shell = shell_of(app, vcx);
    shell.update(vcx, |s, cx| {
        s.navigate(
            Route::Channel {
                community: CommunityId(community),
                channel: Some(channel),
            },
            cx,
        );
    });
    pump(vcx);
}

/// Whether the window's thread pane is open on `root` (the aside drawn by the threads area of this window).
fn pane_open_on(
    app: &Entity<AppView>,
    vcx: &mut VisualTestContext,
    channel: ChannelId,
    root: MessageId,
) -> bool {
    let shell = shell_of(app, vcx);
    let ctx = shell.read_with(vcx, |s, _| s.slot_cx());
    let drawn = vcx.update(|_, cx| dilla_desktop::screens::threads::is_open(&ctx, cx));
    drawn && ctx.state.threads.open == Some(OpenThread { channel, root })
}

/// What the open thread's pane lists: its root's body, then its replies' bodies.
fn pane_bodies(client: &Client, root: MessageId) -> (Option<String>, Vec<String>) {
    let s = client.state().borrow().clone();
    s.threads
        .threads
        .get(&root)
        .map_or((None, Vec::new()), |d| {
            (
                d.root.as_ref().map(|r| r.body.clone()),
                d.replies.iter().map(|r| r.body.clone()).collect(),
            )
        })
}

/// threads.spec.ts tests 4 and 5's common part: Ada's window on #general; Ada posts the plan, Björn answers it in its
/// thread with the needle, posts a decoy and answers that in its own thread. Both chips show. Answers the window, the
/// channel, the plan's id, the needle and the needle reply's id.
struct ThreadScene {
    ada: Rc<Client>,
    bjorn: Client,
    general: ChannelId,
    plan: MessageId,
    plan_body: String,
    needle: String,
    reply: MessageId,
}

fn thread_scene<'a>(
    w: &World,
    cx: &'a mut TestAppContext,
    tag: &str,
) -> (ThreadScene, Entity<AppView>, &'a mut VisualTestContext) {
    let ada = Rc::new(w.start(&w.ada));
    let bjorn = w.start(&w.bjorn);
    let general = w.general(&ada);
    w.general(&bjorn);
    w.open(&ada, general);
    w.open(&bjorn, general);
    let (app, vcx) = window_on(cx, &w.ada, &ada);
    until(vcx, "the shell", |vcx| {
        app.read_with(vcx, |a, _| a.shell().is_some())
    });
    go_to(&app, vcx, w.community, general);

    let plan_body = format!("plan {tag}");
    let plan = w.send(&ada, general, &plan_body);
    w.stored(&bjorn, general, &plan_body);
    let needle = format!("quokka{tag}");
    let reply_body = format!("{needle} in a thread");
    let reply =
        w.rt.block_on(
            bjorn
                .threads()
                .send_in_thread(general, plan, reply_body.clone()),
        )
        .unwrap()
        .expect("the reply is queued");
    let decoy = w.send(&bjorn, general, &format!("decoy {tag}"));
    w.rt.block_on(
        bjorn
            .threads()
            .send_in_thread(general, decoy, format!("decoyreply {tag}")),
    )
    .unwrap()
    .expect("the decoy's reply is queued");
    // Both chips show on Ada's side: one reply under the plan and one under the decoy.
    until(vcx, "both thread chips", |_| {
        let s = ada.state().borrow().clone();
        [plan, decoy].iter().all(|root| {
            s.threads
                .summary(general, *root)
                .is_some_and(|x| x.count == 1)
        })
    });
    // The reply is not a row of the channel.
    assert!(item(&ada.state().borrow(), general, &reply_body).is_none());
    assert!(
        !pane_open_on(&app, vcx, general, plan),
        "no pane before the hit"
    );
    (
        ThreadScene {
            ada,
            bjorn,
            general,
            plan,
            plan_body,
            needle,
            reply,
        },
        app,
        vcx,
    )
}

/// The pane is open on the plan's thread, lists the plan and the needle's reply, and nothing of the decoy.
fn assert_plan_thread_open(
    scene: &ThreadScene,
    app: &Entity<AppView>,
    vcx: &mut VisualTestContext,
) {
    until(vcx, "the plan's thread in the pane", |vcx| {
        pane_open_on(app, vcx, scene.general, scene.plan)
            && pane_bodies(&scene.ada, scene.plan)
                .1
                .iter()
                .any(|b| b.contains(&scene.needle))
    });
    let (root, replies) = pane_bodies(&scene.ada, scene.plan);
    assert_eq!(
        root.as_deref(),
        Some(scene.plan_body.as_str()),
        "the pane shows the root"
    );
    assert!(
        replies.iter().all(|b| !b.contains("decoy")) && root.is_some_and(|r| !r.contains("decoy")),
        "nothing of the decoy: {replies:?}"
    );
}

/// e2e/web/threads.spec.ts, "a thread opens from a search hit": the reply is not a row of the channel, so its hit cannot
/// focus it there; choosing it in the palette (Ctrl+K, the needle typed, Enter) opens its thread in the aside, with the
/// root and the reply and nothing of the decoy thread.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn thread_opens_from_search_hit(cx: &mut TestAppContext) {
    let w = world("t28-search-thread", 1);
    let (scene, app, vcx) = thread_scene(&w, cx, "s1");
    vcx.simulate_keystrokes("ctrl-k");
    until(vcx, "the palette", |vcx| {
        vcx.update(|window, cx| {
            dilla_desktop::screens::search::inspect(window, cx).is_some_and(|p| p.open)
        })
    });
    vcx.simulate_input(&scene.needle);
    let needle = scene.needle.clone();
    let mut at = None;
    until(vcx, "the needle's hit", |vcx| {
        let rows = vcx.update(|window, cx| {
            dilla_desktop::screens::search::inspect(window, cx)
                .map(|p| p.rows)
                .unwrap_or_default()
        });
        at = rows.iter().position(|(label, sub)| {
            label.contains(&needle) || sub.as_ref().is_some_and(|s| s.contains(&needle))
        });
        at.is_some()
    });
    for _ in 0..at.unwrap() {
        vcx.simulate_keystrokes("down");
    }
    // The row Enter chooses (the palette's active result) and the hit behind it.
    let chosen = vcx
        .update(|window, cx| dilla_desktop::screens::search::inspect(window, cx))
        .and_then(|p| p.active)
        .expect("an active result");
    let hit = scene
        .ada
        .state()
        .borrow()
        .search
        .hits
        .iter()
        .find(|h| format!("msg:{}", h.id()) == chosen)
        .cloned()
        .unwrap_or_else(|| panic!("the active row {chosen} is a message hit"));
    vcx.simulate_keystrokes("enter");
    assert_plan_thread_open(&scene, &app, vcx);
    // The hit chosen is the needle reply itself: it names the plan's thread, and the reply the opened pane holds (by
    // its id) is the message at the hit's place (channel and seq).
    assert_eq!(
        hit.thread_root,
        Some(scene.plan),
        "the hit is a reply in the plan's thread"
    );
    let opened = scene
        .ada
        .state()
        .borrow()
        .threads
        .threads
        .get(&scene.plan)
        .and_then(|d| {
            d.replies
                .iter()
                .find(|r| r.msg_id == Some(scene.reply))
                .cloned()
        })
        .expect("the pane holds the needle reply");
    assert!(opened.body.contains(&scene.needle));
    assert_eq!(
        (hit.channel, Some(hit.seq)),
        (scene.general, opened.seq),
        "the chosen hit is that reply"
    );
    drop(app);
    scene.bjorn.stop();
}

/// e2e/web/threads.spec.ts, "a thread opens from the pin of a reply": Björn (the server's owner, who may pin) pins the
/// reply; the pins dialog lists it, and its Jump closes the dialog and opens the reply's thread in the aside.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn thread_opens_from_pin_of_reply(cx: &mut TestAppContext) {
    let w = world("t28-pin-thread", 1);
    let (scene, app, vcx) = thread_scene(&w, cx, "p1");
    w.rt.block_on(scene.bjorn.messages().pin(scene.general, scene.reply, true))
        .unwrap()
        .expect("the pin is queued");
    let general = scene.general;
    let client = scene.ada.clone();
    vcx.update(|window, cx| {
        dilla_desktop::screens::messages::open_pins(general, Some(&client), window, cx);
    });
    let needle = scene.needle.clone();
    until(vcx, "the pins dialog lists the reply", |vcx| {
        let open = vcx.update(|window, cx| {
            dilla_desktop::screens::messages::ui::handle(window, cx)
                .is_some_and(|ui| ui.read(cx).pins == Some(general))
        });
        // The dialog reloads its list as it opens; a pin made since is read again.
        let listed = scene
            .ada
            .state()
            .borrow()
            .messages
            .pins
            .get(&general)
            .is_some_and(|p| {
                p.iter()
                    .any(|pin| pin.msg_id == scene.reply && pin.excerpt.contains(&needle))
            });
        if open && !listed {
            let reload = scene.ada.messages().load_pins(general);
            drop(reload);
        }
        open && listed
    });
    let reply = scene.reply;
    vcx.update(|window, cx| {
        dilla_desktop::screens::messages::dialogs::jump_from_pins(general, reply, window, cx)
    });
    pump(vcx);
    let closed = vcx.update(|window, cx| {
        dilla_desktop::screens::messages::ui::handle(window, cx)
            .is_none_or(|ui| ui.read(cx).pins.is_none())
    });
    assert!(closed, "the jump closes the dialog");
    assert_plan_thread_open(&scene, &app, vcx);
    drop(app);
    scene.bjorn.stop();
}

/// The item of `msg` in `channel`, as `client` holds it.
fn by_id(client: &Client, channel: ChannelId, msg: MessageId) -> Option<TimelineItem> {
    client
        .state()
        .borrow()
        .timelines
        .get(&channel)?
        .items
        .iter()
        .find(|i| i.msg_id == Some(msg))
        .cloned()
}

/// Waits (pumping the window) until `client`'s item of `msg` satisfies `pred`; answers it.
fn until_item(
    vcx: &mut VisualTestContext,
    client: &Client,
    channel: ChannelId,
    msg: MessageId,
    what: &str,
    pred: impl Fn(&TimelineItem) -> bool,
) -> TimelineItem {
    let mut found = None;
    until(vcx, what, |_| {
        found = by_id(client, channel, msg).filter(|i| pred(i));
        found.is_some()
    });
    found.unwrap()
}

/// Runs the toolbar's (or the context menu's) action `id` on `item`, as its button does in this window.
fn run_action(app: &Entity<AppView>, vcx: &mut VisualTestContext, item: &TimelineItem, id: &str) {
    let ctx = shell_of(app, vcx).read_with(vcx, |s, _| s.slot_cx());
    let action = dilla_desktop::slots::message_actions(item, &ctx)
        .into_iter()
        .find(|a| a.id.as_ref() == id)
        .unwrap_or_else(|| panic!("no {id} action on {:?}", item.body));
    vcx.update(|window, cx| (action.run)(&ctx, window, cx));
    pump(vcx);
}

/// The composer's field, and whether it has the keyboard.
fn composer_focused(app: &Entity<AppView>, vcx: &mut VisualTestContext) -> bool {
    let shell = shell_of(app, vcx);
    vcx.update(|window, cx| shell.read(cx).composer_focus(cx).is_focused(window))
}

/// A click into the composer's field: it takes the keyboard.
fn click_composer(app: &Entity<AppView>, vcx: &mut VisualTestContext) {
    let shell = shell_of(app, vcx);
    vcx.update(|window, cx| {
        let handle = shell.read(cx).composer_focus(cx);
        window.focus(&handle, cx);
    });
    pump(vcx);
}

fn message_ui<R>(
    vcx: &mut VisualTestContext,
    f: impl FnOnce(&dilla_desktop::screens::messages::ui::MessageUi) -> R,
) -> R {
    vcx.update(|window, cx| {
        let ui = dilla_desktop::screens::messages::ui::handle(window, cx).expect("the log drew");
        f(ui.read(cx))
    })
}

/// e2e/web/conversation.spec.ts, "edit, reply, react, pin and delete between the browser and the peer", as one flow in the
/// window (Ada) with Björn's client as the peer. Ada owns the server here, so she may pin (the web's grant of the pin right
/// to @everyone is the roles area's, held by its own tests). The narrow-width toolbar and the accessibility scans are the
/// browser's.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn edit_reply_react_pin_delete_one_flow(cx: &mut TestAppContext) {
    use dilla_desktop::screens::messages::dialogs;
    let w = world("t28-one-flow", 0);
    let ada = Rc::new(w.start(&w.ada));
    let bjorn = w.start(&w.bjorn);
    let general = w.general(&ada);
    w.general(&bjorn);
    w.open(&ada, general);
    w.open(&bjorn, general);
    let (app, vcx) = window_on(cx, &w.ada, &ada);
    until(vcx, "the shell", |vcx| {
        app.read_with(vcx, |a, _| a.shell().is_some())
    });
    go_to(&app, vcx, w.community, general);
    let ada_user = dilla_client::state::UserId(w.ada.user_id);

    // Ada types in the composer and sends with Enter.
    let original = "original words";
    click_composer(&app, vcx);
    vcx.simulate_input(original);
    vcx.simulate_keystrokes("enter");
    let mine = w.stored(&bjorn, general, original);
    assert_eq!(
        by_id(&bjorn, general, mine).unwrap().sender_user,
        Some(ada_user)
    );

    // Edit in place from the empty composer: ArrowUp opens the editor on her last message, Enter saves.
    until_item(vcx, &ada, general, mine, "her message stored", |i| {
        i.seq.is_some()
    });
    vcx.simulate_keystrokes("up");
    pump(vcx);
    assert!(
        message_ui(vcx, |ui| ui.editing.is_some()),
        "ArrowUp opens the editor"
    );
    vcx.simulate_keystrokes("ctrl-a");
    let edited = "edited words";
    vcx.simulate_input(edited);
    vcx.simulate_keystrokes("enter");
    until_item(vcx, &ada, general, mine, "the edit in the window", |i| {
        i.edited && i.body == edited
    });
    w.wait(&bjorn, "the edit at the peer", |s| {
        s.timelines.get(&general).is_some_and(|t| {
            t.items
                .iter()
                .any(|i| i.msg_id == Some(mine) && i.edited && i.body == edited)
        })
    });

    // The peer's message and its own edit.
    let theirs = w.send(&bjorn, general, "from the peer");
    until_item(vcx, &ada, general, theirs, "the peer's message", |i| {
        i.seq.is_some()
    });
    w.rt.block_on(
        bjorn
            .messages()
            .edit(general, theirs, "the peer edited".into()),
    )
    .unwrap()
    .expect("the peer's edit");
    let target = until_item(vcx, &ada, general, theirs, "the peer's edit", |i| {
        i.edited && i.body == "the peer edited"
    });

    // A reply: the toolbar's Reply aims the composer at the peer's message; the answer carries its reference. The
    // keyboard is first moved off the composer (to the shell's root, as a click on the toolbar leaves it), so the
    // focus assertion below holds only if Reply itself puts the keyboard back in the composer.
    let shell = shell_of(&app, vcx);
    vcx.update(|window, cx| {
        let root = gpui::Focusable::focus_handle(shell.read(cx), cx);
        window.focus(&root, cx);
    });
    pump(vcx);
    assert!(
        !composer_focused(&app, vcx),
        "the keyboard is off the composer before Reply"
    );
    run_action(&app, vcx, &target, "reply");
    let replying = ada.state().borrow().threads.replying.clone();
    assert_eq!(replying.map(|r| r.msg_id), Some(theirs), "the reply bar");
    assert!(
        composer_focused(&app, vcx),
        "Reply puts the keyboard in the composer"
    );
    vcx.simulate_input("my answer");
    vcx.simulate_keystrokes("enter");
    let answer = w.stored(&bjorn, general, "my answer");
    let at_peer = by_id(&bjorn, general, answer).unwrap();
    assert_eq!(at_peer.reply.as_ref().map(|r| r.msg_id), Some(theirs));
    let shown = until_item(vcx, &ada, general, answer, "the answer stored", |i| {
        i.seq.is_some()
    });
    let line = shown.reply.expect("the reply line");
    assert!(line.excerpt.contains("the peer edited"), "{line:?}");
    assert!(
        ada.state().borrow().threads.replying.is_none(),
        "the reply bar went with the send"
    );

    // Reactions are per user: the peer's 👍, then Ada's click on the chip makes it two, hers.
    w.rt.block_on(bjorn.reactions().react(general, theirs, "👍", true))
        .unwrap()
        .expect("the peer reacts");
    let item = until_item(vcx, &ada, general, theirs, "the peer's reaction", |i| {
        i.reactions
            .iter()
            .any(|r| r.emoji == "👍" && r.count == 1 && !r.mine)
    });
    let ctx = shell_of(&app, vcx).read_with(vcx, |s, _| s.slot_cx());
    dilla_desktop::screens::reactions::picker::toggle(&ctx, &item.key, "👍");
    until_item(vcx, &ada, general, theirs, "Ada's reaction", |i| {
        i.reactions
            .iter()
            .any(|r| r.emoji == "👍" && r.count == 2 && r.mine)
    });
    w.wait(&bjorn, "Ada's reaction at the peer", |s| {
        s.timelines.get(&general).is_some_and(|t| {
            t.items.iter().any(|i| {
                i.msg_id == Some(theirs)
                    && i.reactions.iter().any(|r| r.emoji == "👍" && r.count == 2)
            })
        })
    });

    // A pin, listed with who pinned it; the dialog's Jump closes it and flashes the row.
    let item = by_id(&ada, general, theirs).unwrap();
    run_action(&app, vcx, &item, "pin");
    let pinned = until_item(vcx, &ada, general, theirs, "the row pinned", |i| i.pinned);
    w.wait(&bjorn, "the pin at the peer", |s| {
        s.timelines
            .get(&general)
            .is_some_and(|t| t.items.iter().any(|i| i.msg_id == Some(theirs) && i.pinned))
    });
    let client = ada.clone();
    vcx.update(|window, cx| {
        dilla_desktop::screens::messages::open_pins(general, Some(&client), window, cx)
    });
    until(vcx, "the pin listed by Ada", |_| {
        ada.state()
            .borrow()
            .messages
            .pins
            .get(&general)
            .is_some_and(|p| {
                p.iter().any(|pin| {
                    pin.msg_id == theirs
                        && pin.pinned_by == ada_user
                        && pin.excerpt.contains("the peer edited")
                })
            })
    });
    vcx.update(|window, cx| dialogs::jump_from_pins(general, theirs, window, cx));
    pump(vcx);
    assert!(
        message_ui(vcx, |ui| ui.pins.is_none()),
        "the jump closes the dialog"
    );
    assert!(
        message_ui(vcx, |ui| ui.flashing(&pinned.key)),
        "the jump flashes the row"
    );

    // Unpin from the dialog: the row loses the pin, the peer sees it, the dialog stays.
    vcx.update(|window, cx| {
        dilla_desktop::screens::messages::open_pins(general, Some(&client), window, cx)
    });
    pump(vcx);
    vcx.update(|window, cx| dialogs::unpin_from_pins(general, theirs, window, cx));
    until_item(vcx, &ada, general, theirs, "the row unpinned", |i| {
        !i.pinned
    });
    w.wait(&bjorn, "the unpin at the peer", |s| {
        s.timelines.get(&general).is_some_and(|t| {
            t.items
                .iter()
                .any(|i| i.msg_id == Some(theirs) && !i.pinned)
        })
    });
    assert!(
        message_ui(vcx, |ui| ui.pins == Some(general)),
        "the dialog stays open"
    );

    // The pins dialog is closed (its close button's call), as a person must before reaching a message's toolbar.
    vcx.update(|window, cx| {
        let ui = dilla_desktop::screens::messages::ui::handle(window, cx).expect("the log drew");
        ui.update(cx, |ui, cx| ui.close_pins(cx));
    });
    pump(vcx);
    assert!(message_ui(vcx, |ui| ui.pins.is_none()), "the pins closed");

    // Delete for everyone, only through the dialog.
    let own = by_id(&ada, general, mine).unwrap();
    run_action(&app, vcx, &own, "delete");
    assert!(
        message_ui(vcx, |ui| ui.deleting.is_some()),
        "the delete asks first"
    );
    assert_eq!(
        by_id(&ada, general, mine).unwrap().state,
        TimelineItemState::Ok
    );
    // A real click on the drawn dialog's Delete, at its painted bounds (the button's wiring, not only the function).
    pump(vcx);
    let delete = vcx
        .debug_bounds("message-delete-confirm")
        .expect("the delete dialog's Delete is drawn");
    vcx.simulate_click(delete.center(), gpui::Modifiers::none());
    until_item(vcx, &ada, general, mine, "her message deleted", |i| {
        i.state == TimelineItemState::Deleted && !i.body.contains("edited")
    });
    assert!(
        message_ui(vcx, |ui| ui.deleting.is_none()),
        "the dialog closed"
    );
    w.wait(&bjorn, "the delete at the peer", |s| {
        s.timelines.get(&general).is_some_and(|t| {
            t.items
                .iter()
                .any(|i| i.msg_id == Some(mine) && i.state == TimelineItemState::Deleted)
        })
    });

    // The peer deletes its own message: Ada shows it deleted, without its text.
    w.rt.block_on(bjorn.messages().delete(general, theirs))
        .unwrap()
        .expect("the peer deletes");
    until_item(vcx, &ada, general, theirs, "the peer's delete", |i| {
        i.state == TimelineItemState::Deleted && !i.body.contains("the peer edited")
    });
    drop(app);
    bjorn.stop();
}

// ---- two devices of one account, and restarts ----

/// A new device of Ada's on the data root `root`: the sign-in's login, authenticator code and recovery key (the app's
/// sign-in screen runs the same `SignIn`). Answers its instance and its keychain.
fn ada_signs_in(
    w: &mut World,
    root: &std::path::Path,
) -> (
    dilla_client::seams::InstanceConfig,
    Arc<dilla_client::keychain::MemoryKeychain>,
) {
    ada_signs_in_with(
        w,
        root,
        Arc::new(dilla_client::keychain::MemoryKeychain::new()),
    )
}

fn start_device(
    w: &World,
    cfg: &dilla_client::seams::InstanceConfig,
    kc: &Arc<dilla_client::keychain::MemoryKeychain>,
) -> Client {
    let (cfg, kc) = (cfg.clone(), kc.clone());
    w.rt.block_on(async move {
        tokio::task::spawn_blocking(move || Client::start(cfg, kc).expect("the device starts"))
            .await
            .unwrap()
    })
}

fn scratch_root(tag: &str) -> std::path::PathBuf {
    let dir = world::repo_root().join(format!("target/t28-{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

/// A PNG of `w`x`h` drawn from a gradient.
fn png(w: u32, h: u32) -> Vec<u8> {
    let img = image::RgbaImage::from_fn(w, h, |x, y| {
        image::Rgba([(x % 256) as u8, (y % 256) as u8, 90, 255])
    });
    let mut out = std::io::Cursor::new(Vec::new());
    img.write_to(&mut out, image::ImageFormat::Png).unwrap();
    out.into_inner()
}

/// e2e/web/core-worker.spec.ts, "two devices of one account fold edits, reactions, replies, pins and deletes, and files
/// go through the worker": Ada's first device A and a second device B signed in by password, code and recovery key,
/// with Björn as the peer. B edits A's message (one user); both react 👍 and count one chip of 2 with the peer's; B
/// replies to the peer and A shows the reference; A pins the peer's message and B shows the pin; A sends a PNG that B
/// opens whole and as a small thumbnail; A deletes it and both and the peer show it deleted. (The blob purge is the
/// instance's, held by its own suite.)
#[test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn own_devices_fold_everything() {
    use dilla_client::features::attachments::{NewFile, is_thumbnail_bytes};
    use dilla_client::state::UserId;
    let mut w = world("t28-fold", 0);
    let a = w.start(&w.ada);
    let peer = w.start(&w.bjorn);
    let ch = w.general(&a);
    w.open(&a, ch);
    w.general(&peer);
    w.open(&peer, ch);
    let root = scratch_root("fold-b");
    let (cfg, kc) = ada_signs_in(&mut w, &root);
    let b = start_device(&w, &cfg, &kc);
    let s = w.wait(&b, "B ready", |s| s.account.user_id.is_some());
    let ada = UserId(w.ada.user_id);
    assert_eq!(s.account.user_id, Some(ada), "the same account");
    assert_ne!(
        s.account.device_id.map(|d| d.0),
        Some(w.ada.device_id),
        "another device"
    );
    w.general(&b);
    w.open(&b, ch);
    let at = |c: &Client, msg: MessageId| -> Option<TimelineItem> { by_id(c, ch, msg) };
    let wait_item =
        |c: &Client, msg: MessageId, what: &str, pred: &dyn Fn(&TimelineItem) -> bool| {
            w.wait(c, what, |s| {
                s.timelines
                    .get(&ch)
                    .and_then(|t| t.items.iter().find(|i| i.msg_id == Some(msg)))
                    .is_some_and(pred)
            })
        };

    // A sends; B edits it (the same user); both show the edit and the peer gets it from Ada.
    let first = w.send(&a, ch, "first words");
    wait_item(&b, first, "B holds it", &|i| i.body == "first words");
    w.rt.block_on(b.messages().edit(ch, first, "better words".into()))
        .unwrap()
        .expect("B edits");
    for c in [&a, &b] {
        wait_item(c, first, "the edit on both", &|i| {
            i.body == "better words" && i.edited
        });
    }
    let seen = wait_item(&peer, first, "the edit at the peer", &|i| {
        i.body == "better words" && i.edited
    });
    assert_eq!(
        seen.timelines[&ch]
            .items
            .iter()
            .find(|i| i.msg_id == Some(first))
            .unwrap()
            .sender_user,
        Some(ada)
    );

    // A and B both react 👍: one reaction per user; the peer's makes it two. A later message of the peer's is the
    // positive control: once both show it, every earlier fold is in, so the count is the final one.
    for c in [&a, &b] {
        w.rt.block_on(c.reactions().react(ch, first, "👍", true))
            .unwrap()
            .expect("reacts");
    }
    wait_item(&peer, first, "both devices' reactions at the peer", &|i| {
        i.reactions.iter().any(|r| r.emoji == "👍")
    });
    w.rt.block_on(peer.reactions().react(ch, first, "👍", true))
        .unwrap()
        .expect("the peer reacts");
    let after = w.send(&peer, ch, "after the reactions");
    for c in [&a, &b] {
        wait_item(c, after, "the control message", &|_| true);
        let reactions: Vec<_> = at(c, first)
            .unwrap()
            .reactions
            .iter()
            .map(|r| (r.emoji.clone(), r.count, r.mine))
            .collect();
        assert_eq!(
            reactions,
            [("👍".to_owned(), 2, true)],
            "per user, not per device"
        );
    }

    // B replies to the peer's message; A shows the reference.
    let hi = w.send(&peer, ch, "peer says hi");
    let held = wait_item(&b, hi, "B holds the peer's message", &|i| i.seq.is_some());
    let hi_seq = held.timelines[&ch]
        .items
        .iter()
        .find(|i| i.msg_id == Some(hi))
        .unwrap()
        .seq;
    let answer =
        w.rt.block_on(b.threads().reply(ch, hi, "answer".into()))
            .unwrap()
            .expect("B replies");
    let shown = wait_item(&a, answer, "A shows the reply", &|i| i.reply.is_some());
    let reply = shown.timelines[&ch]
        .items
        .iter()
        .find(|i| i.msg_id == Some(answer))
        .unwrap()
        .reply
        .clone()
        .unwrap();
    assert_eq!(
        (
            reply.msg_id,
            reply.state,
            reply.sender_user,
            reply.excerpt.as_str(),
            reply.seq
        ),
        (hi, 0, Some(UserId(w.bjorn.user_id)), "peer says hi", hi_seq)
    );
    let at_peer = wait_item(&peer, answer, "the reply at the peer", &|i| {
        i.reply.is_some()
    });
    assert_eq!(
        at_peer.timelines[&ch]
            .items
            .iter()
            .find(|i| i.msg_id == Some(answer))
            .unwrap()
            .reply
            .as_ref()
            .map(|r| r.msg_id),
        Some(hi)
    );

    // A pins the peer's message: A's pins list it, by Ada; B's item shows the pin.
    w.rt.block_on(a.messages().pin(ch, hi, true))
        .unwrap()
        .expect("A pins");
    let pins = w.rt.block_on(async {
        let deadline = Instant::now() + WAIT;
        loop {
            a.messages()
                .load_pins(ch)
                .await
                .unwrap()
                .expect("pins read");
            let p = a
                .state()
                .borrow()
                .messages
                .pins
                .get(&ch)
                .cloned()
                .unwrap_or_default();
            if !p.is_empty() || Instant::now() > deadline {
                return p;
            }
            tokio::time::sleep(Duration::from_millis(300)).await;
        }
    });
    assert_eq!(pins.len(), 1, "{pins:?}");
    assert_eq!(
        (
            pins[0].msg_id,
            Some(pins[0].seq),
            pins[0].sender_user,
            pins[0].excerpt.as_str(),
            pins[0].pinned_by,
            pins[0].thread_root
        ),
        (
            hi,
            hi_seq,
            Some(UserId(w.bjorn.user_id)),
            "peer says hi",
            ada,
            None
        )
    );
    wait_item(&b, hi, "B shows the pin", &|i| i.pinned);

    // A attaches a PNG; B opens it whole and as a thumbnail.
    let drawn = png(320, 240);
    let file_msg = w.rt.block_on(async {
        let tray = a.attachments().tray(ch);
        tray.add(vec![NewFile::from_bytes(
            "drawn.png",
            "image/png",
            drawn.clone(),
        )])
        .await
        .unwrap();
        let d = tray.take_all().unwrap();
        a.attachments()
            .send(ch, String::new(), d, None)
            .await
            .unwrap()
            .unwrap()
    });
    let s = wait_item(&b, file_msg, "B holds the file", &|i| {
        i.state == TimelineItemState::Ok && i.attachments.len() == 1
    });
    let item = s.timelines[&ch]
        .items
        .iter()
        .find(|i| i.msg_id == Some(file_msg))
        .unwrap()
        .clone();
    assert_eq!(
        (
            item.body.as_str(),
            item.attachments[0].name.as_str(),
            item.attachments[0].mime.as_str(),
            item.attachments[0].size
        ),
        ("", "drawn.png", "image/png", drawn.len() as u64)
    );
    let seq = item.seq.unwrap();
    let full =
        w.rt.block_on(b.attachments().open(ch, seq, 0, false))
            .unwrap()
            .unwrap();
    assert_eq!(&full.bytes[..], &drawn[..], "the whole file");
    let small =
        w.rt.block_on(b.attachments().open(ch, seq, 0, true))
            .unwrap()
            .unwrap();
    assert!(is_thumbnail_bytes(&small.bytes), "a WebP or JPEG thumbnail");
    assert!(small.bytes.len() <= 8176, "{} bytes", small.bytes.len());

    // A deletes the file message: both of Ada's devices and the peer show it deleted.
    w.rt.block_on(a.messages().delete(ch, file_msg))
        .unwrap()
        .expect("A deletes");
    for c in [&a, &b, &peer] {
        wait_item(c, file_msg, "the delete", &|i| {
            i.state == TimelineItemState::Deleted
        });
    }
    b.stop();
    let _ = std::fs::remove_dir_all(&root);
}

/// The app quits: everything of `client` is let go (what the process's exit does: `release_client`, then the window's
/// close fan-out), and the test waits until its store is closed, so the next launch opens it as a restart does.
fn quit(vcx: &mut VisualTestContext, client: Rc<Client>) {
    let probe = client.store_probe();
    vcx.update(|_, cx| dilla_desktop::release::release_client(&client, cx));
    drop(client);
    vcx.update(|window, cx| {
        let id = window.window_handle().window_id();
        dilla_desktop::windows::on_closed(cx, id);
        window.remove_window();
    });
    let deadline = Instant::now() + WAIT;
    while !probe.is_closed() {
        assert!(Instant::now() < deadline, "the store never closed");
        vcx.run_until_parked();
        std::thread::sleep(Duration::from_millis(20));
    }
}

/// Launches the app on `root` with `kc` as a restart does: the window starts the first listed instance's client itself.
fn relaunch<'a>(
    cx: &'a mut TestAppContext,
    root: &std::path::Path,
    kc: &Arc<dilla_client::keychain::MemoryKeychain>,
) -> (Entity<AppView>, &'a mut VisualTestContext) {
    let mut launch = Launch::live(Some(root.to_path_buf()), kc.clone());
    launch.shipping = false;
    launch.resume = true;
    cx.add_window_view(move |window, cx| AppView::new(launch, window, cx))
}

/// The client the window's shell runs, once it shows the shell.
fn shell_client(app: &Entity<AppView>, vcx: &mut VisualTestContext) -> Rc<Client> {
    let mut found = None;
    until(vcx, "the shell with its client", |vcx| {
        found = app.read_with(vcx, |a, cx| {
            a.shell().and_then(|s| s.read(cx).client().cloned())
        });
        found.is_some()
    });
    found.unwrap()
}

/// e2e/web/reload-mid-sync.spec.ts, "messages that reach a second device mid-sync are readable after it reloads, in every
/// group it joined": Ada's first device writes in #general, #raid and #news; the app signs in as her second device and
/// opens two of them (it joins their groups); the first device writes in both, and the app restarts at once, while those
/// messages reach it. After the restart each of them is readable, in every group it joined. (The browser's held joins and
/// slow device-list reads are its worker's; here the restart lands on whatever the sync had done.)
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn restart_mid_sync_reads_every_group(cx: &mut TestAppContext) {
    let mut w = world("t28-mid-sync", 0);
    let a = w.start(&w.ada);
    let c = CommunityId(w.community);
    let general = w.general(&a);
    // Two more text channels, made by Ada as the owner.
    {
        use dilla_core::ids::DeviceId;
        use dilla_testkit::HttpDs;
        let mut ds = HttpDs::with_session(
            &w.host.public,
            DeviceId::from_bytes(w.ada.device_id),
            w.ada.token.clone(),
        )
        .unwrap();
        ds.create_text_channel(&w.community, "raid").unwrap();
        ds.create_text_channel(&w.community, "news").unwrap();
    }
    a.refresh();
    let named = |n: &'static str| {
        w.wait(&a, n, move |s| s.channel_named(c, n).is_some())
            .channel_named(c, n)
            .unwrap()
            .id
    };
    let channels = [general, named("raid"), named("news")];
    // One message per channel before the second device, as the web's run makes them.
    for (i, ch) in channels.iter().enumerate() {
        w.open(&a, *ch);
        w.send(&a, *ch, &format!("before the phone {i}"));
    }

    // The second device: the app on a fresh data root, signed in; it opens #general and #raid.
    let root = scratch_root("mid-sync-app");
    let (cfg, kc) = ada_signs_in(&mut w, &root);
    let phone = Rc::new(start_device(&w, &cfg, &kc));
    cx.executor().allow_parking();
    cx.update(dilla_desktop::install);
    let mut launch = Launch::live(Some(root.clone()), kc.clone());
    launch.shipping = false;
    launch.resume = false;
    launch.handoff = Some(phone.clone());
    let (app, vcx) = cx.add_window_view(move |window, cx| AppView::new(launch, window, cx));
    until(vcx, "the shell", |vcx| {
        app.read_with(vcx, |a, _| a.shell().is_some())
    });
    // Its opens join the two channels' groups (a device that joins later reads nothing from before its join).
    for ch in &channels[..2] {
        go_to(&app, vcx, w.community, *ch);
        let ch = *ch;
        until(vcx, "the channel's group joined", |_| {
            phone
                .state()
                .borrow()
                .timelines
                .get(&ch)
                .is_some_and(|t| t.group == dilla_client::state::ChannelGroupState::Active)
        });
    }
    // The first device writes in both joined channels; the app restarts while the messages reach it.
    let mut sent = Vec::new();
    for (i, ch) in channels[..2].iter().enumerate() {
        let body = format!("mid sync {i}");
        w.rt.block_on(a.send_text(*ch, body.clone()))
            .unwrap()
            .expect("queued");
        sent.push((*ch, body));
    }
    quit(vcx, phone);
    let (app, vcx) = relaunch(cx, &root, &kc);
    let phone = shell_client(&app, vcx);
    for (ch, body) in &sent {
        go_to(&app, vcx, w.community, *ch);
        let (ch, body) = (*ch, body.clone());
        until(vcx, &format!("{body:?} readable after the restart"), |_| {
            item(&phone.state().borrow(), ch, &body)
                .is_some_and(|i| i.state == TimelineItemState::Ok)
        });
    }
    drop(app);
    let _ = std::fs::remove_dir_all(&root);
}

/// e2e/web/installed-upgrade-account.spec.ts, "installed upgrade preserves the actual device keystore, encrypted history,
/// draft guard and recovery enrollment", its parts for a fresh native install (the installer and the update's own guard
/// are the updater's, the next plan's): Ada's device in the app reads the history and leaves a draft; after a restart it
/// is the same device (its store key in the keychain untouched), the history is readable, the draft is still in the
/// composer, and sending it comes from the same device. After her session expired, a restart signs the same device in
/// with its own key. The recovery enrolment is [`recovery_enrols_a_new_device_the_peer_reads`].
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn restart_keeps_keystore_history_draft_guard_recovery(cx: &mut TestAppContext) {
    use dilla_client::seams::Keychain as _;
    use dilla_client::state::DeviceId;
    let w = world("t28-restart", 1);
    let peer = w.start(&w.bjorn);
    let general = w.general(&peer);
    w.open(&peer, general);
    let ada = Rc::new(w.start(&w.ada));
    w.general(&ada);
    w.open(&ada, general);
    let root = root_of(&w.ada);
    let kc = w.ada.kc.clone();
    let account = dilla_client::store::store_keychain_account(&w.ada.cfg).unwrap();
    let key_before = kc.get(&account).unwrap().expect("the store's key");
    let device_a = DeviceId(w.ada.device_id);
    let (app, vcx) = window_on(cx, &w.ada, &ada);
    until(vcx, "the shell", |vcx| {
        app.read_with(vcx, |a, _| a.shell().is_some())
    });
    go_to(&app, vcx, w.community, general);
    let historical = "encrypted before the restart";
    w.send(&peer, general, historical);
    until(vcx, "the history readable", |_| {
        item(&ada.state().borrow(), general, historical)
            .is_some_and(|i| i.state == TimelineItemState::Ok)
    });
    let draft = "work that waits for the restart";
    click_composer(&app, vcx);
    vcx.simulate_input(draft);
    // The draft is kept a moment after the last key (the composer's delayed write).
    for _ in 0..30 {
        pump(vcx);
    }
    vcx.executor().advance_clock(Duration::from_secs(1));
    pump(vcx);
    quit(vcx, ada);

    // The restart: the same device, its key untouched, the history readable, the draft still there.
    let (app, vcx) = relaunch(cx, &root, &kc);
    let ada = shell_client(&app, vcx);
    assert_eq!(
        ada.state().borrow().account.device_id,
        Some(device_a),
        "the same device"
    );
    // Compared without printing: a key never goes to a log.
    assert!(
        kc.get(&account)
            .unwrap()
            .is_some_and(|k| k[..] == key_before[..]),
        "the store's key is untouched"
    );
    go_to(&app, vcx, w.community, general);
    until(vcx, "the history after the restart", |_| {
        item(&ada.state().borrow(), general, historical)
            .is_some_and(|i| i.state == TimelineItemState::Ok)
    });
    let shell = shell_of(&app, vcx);
    until(vcx, "the draft back in the composer", |vcx| {
        vcx.update(|_, cx| shell.read(cx).composer_value(cx)) == draft
    });
    click_composer(&app, vcx);
    vcx.simulate_keystrokes("enter");
    let sent = w.stored(&peer, general, draft);
    assert_eq!(
        by_id(&peer, general, sent).unwrap().sender_device,
        Some(device_a)
    );

    // Past the session's life (the web's 169 h): the restart signs the same device in with its key.
    drop(shell);
    quit(vcx, ada);
    world::advance_clock(&w.host, 169 * 3600);
    let (app, vcx) = relaunch(cx, &root, &kc);
    let ada = shell_client(&app, vcx);
    w.wait(&ada, "online again", |s| {
        s.connection.status == dilla_client::state::ConnectionStatus::Online
    });
    assert_eq!(ada.state().borrow().account.device_id, Some(device_a));
    go_to(&app, vcx, w.community, general);
    w.open(&ada, general);
    let again = w.send(&ada, general, "same device after the session expired");
    w.stored(&peer, general, "same device after the session expired");
    assert_eq!(
        by_id(&peer, general, again).unwrap().sender_device,
        Some(device_a)
    );
    drop(app);
    peer.stop();
}

/// e2e/web/installed-upgrade-account.spec.ts (the same test), its recovery half: Ada's only device, in the app, signs
/// itself out with the recovery key (the app wipes it and shows the sign-in); password, code and key then enrol a new
/// device on the same install, which sends, and the peer reads it from that new device.
///
/// The new device takes the blank leaf the old one left, and the peer still caches the list it read for the old one:
/// the peer reads that leaf again because the epoch change forgets the group's leaves already read (the engine's
/// `apply_hook`, as controller.ts `onGroupChanged`); dilla-client's
/// `send_after_new_device::a_device_enrolled_after_a_self_revocation_is_read_and_sent_to` holds the same.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn recovery_enrols_a_new_device_the_peer_reads(cx: &mut TestAppContext) {
    use dilla_client::seams::Keychain as _;
    use dilla_client::state::DeviceId;
    let mut w = world("t28-recovery", 1);
    let peer = w.start(&w.bjorn);
    let general = w.general(&peer);
    w.open(&peer, general);
    let ada = Rc::new(w.start(&w.ada));
    w.general(&ada);
    w.open(&ada, general);
    let root = root_of(&w.ada);
    let kc = w.ada.kc.clone();
    let account = dilla_client::store::store_keychain_account(&w.ada.cfg).unwrap();
    let device_a = DeviceId(w.ada.device_id);
    let (app, vcx) = window_on(cx, &w.ada, &ada);
    until(vcx, "the shell", |vcx| {
        app.read_with(vcx, |a, _| a.shell().is_some())
    });
    let before = w.send(&ada, general, "from the first device");
    w.stored(&peer, general, "from the first device");
    assert_eq!(
        by_id(&peer, general, before).unwrap().sender_device,
        Some(device_a)
    );

    // Sign this device out with the recovery key; the app wipes it and shows the sign-in.
    let probe = ada.store_probe();
    let answer = ada
        .devices()
        .sign_out_and_remove(zeroize::Zeroizing::new(w.ada.recovery_key.clone()));
    drop(ada);
    let deadline = Instant::now() + WAIT;
    while !(probe.is_closed() && app.read_with(vcx, |a, _| a.shows_sign_in())) {
        assert!(Instant::now() < deadline, "signed out and wiped");
        pump(vcx);
    }
    drop(answer);
    assert!(
        kc.get(&account).unwrap().is_none(),
        "the old device's key is gone"
    );
    quit_window(vcx);

    // Recovery: password, code and key enrol a new device on this install, which sends.
    let (cfg, kc2) = ada_signs_in_with(&mut w, &root, kc.clone());
    let device_b = {
        let c = start_device(&w, &cfg, &kc2);
        let s = w.wait(&c, "the new device ready", |s| {
            s.account.device_id.is_some()
        });
        let id = s.account.device_id.unwrap();
        assert_ne!(id, device_a, "a new device");
        let general = w.general(&c);
        w.open(&c, general);
        let m = w.send(&c, general, "recovery-enrolled after the restart");
        w.stored(&peer, general, "recovery-enrolled after the restart");
        assert_eq!(by_id(&peer, general, m).unwrap().sender_device, Some(id));
        c.stop();
        id
    };
    assert_ne!(device_b, device_a);
    peer.stop();
}

/// Wave 2 H (the Bot lane's finding): the DM area starts with the shell, not when the DMs home (or a conversation's trust
/// banners) first draws. Ada's shell shows no conversation and never the DMs; Björn opens a DM with her and writes; her
/// client lists that DM with no visit to the DMs.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn the_dm_list_is_read_without_opening_the_dms(cx: &mut TestAppContext) {
    let w = world("w2h-dm-start", 0);
    let ada = Rc::new(w.start(&w.ada));
    let bjorn = w.start(&w.bjorn);
    w.general(&ada);
    w.general(&bjorn);
    cx.executor().allow_parking();
    cx.update(dilla_desktop::install);
    assert!(
        !ada.dms().started(),
        "nothing started the DM area before the shell"
    );
    // The shell as it first shows a person with no server yet (a fresh account, its server list still loading): no
    // conversation is drawn, so nothing but the shell's own start reads the DMs.
    let mut first = (*ada.state().borrow().clone()).clone();
    first.communities.clear();
    first.channels.clear();
    let state = Arc::new(first);
    let client = ada.clone();
    let (_shell, vcx) = cx.add_window_view(move |window, cx| {
        ShellView::new(
            state,
            Some(client),
            None,
            dilla_desktop::screens::shell::Clock::Local,
            window,
            cx,
        )
    });
    pump(vcx);
    assert!(ada.dms().started(), "the shell's start started the DM area");
    let ada_user = dilla_client::state::UserId(w.ada.user_id);
    w.rt.block_on(async {
        let dm = bjorn
            .dms()
            .open_dm(ada_user)
            .await
            .unwrap()
            .expect("Björn opens the DM");
        bjorn
            .dms()
            .send_text(dm, "hello from the DMs".into())
            .await
            .unwrap()
            .expect("queued");
    });
    until(vcx, "Ada's client lists the DM", |_| {
        ada.state()
            .borrow()
            .dms
            .all()
            .iter()
            .any(|d| d.members.iter().any(|m| m.0 == w.bjorn.user_id))
    });
}

/// Closes the window (nothing of a client in it).
fn quit_window(vcx: &mut VisualTestContext) {
    vcx.update(|window, _| window.remove_window());
    vcx.run_until_parked();
}

/// [`ada_signs_in`] with the install's own keychain.
fn ada_signs_in_with(
    w: &mut World,
    root: &std::path::Path,
    kc: Arc<dilla_client::keychain::MemoryKeychain>,
) -> (
    dilla_client::seams::InstanceConfig,
    Arc<dilla_client::keychain::MemoryKeychain>,
) {
    use dilla_client::signin::{Owed, SignIn};
    let cfg = dilla_client::instances::Instances::load(root)
        .unwrap()
        .add(url::Url::parse(&w.host.public).unwrap())
        .unwrap();
    let code = w.ada.next_code(&w.host);
    let key = w.ada.recovery_key.clone();
    let (c, k) = (cfg.clone(), kc.clone());
    w.rt.block_on(async move {
        let mut flow = SignIn::new(c, k).unwrap();
        assert_eq!(
            flow.login("ada", world::PASSWORD).await.unwrap(),
            Owed::Code
        );
        flow.code(&code).await.unwrap();
        flow.add_device(&key).await.unwrap();
    });
    (cfg, kc)
}
