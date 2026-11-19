//! System notifications over the XDG notifications D-Bus interface, via
//! `notify-rust`.

use std::cell::RefCell;
use std::collections::HashMap;
use std::rc::Rc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant};

use futures::StreamExt as _;
use futures::channel::mpsc;
use gpui::{
    ForegroundExecutor, SharedString, SystemNotification, SystemNotificationResponse, Task,
};

/// The XDG action key invoked when the user activates the notification body
/// rather than a specific action button.
const DEFAULT_ACTION: &str = "default";

/// How long a show waits for an earlier show of its tag whose `Notify` has not answered.
const NOTIFY_WAIT: Duration = Duration::from_secs(5);

/// What `LiveIds` knows of one tag.
#[derive(Default)]
struct Slot {
    /// The notification server's id, once a `Notify` for the tag has answered.
    id: Option<u32>,
    /// A `Notify` for the tag is in flight.
    pending: bool,
    /// `dismiss` arrived while it was in flight.
    dismissed: bool,
    /// The ticket of the newest show or dismiss of the tag; anything older is superseded.
    current: u64,
}

/// What `LiveIds::begin` tells a show thread to do.
#[derive(Debug, PartialEq, Eq)]
enum Begin {
    /// A newer show or a dismiss of the tag came after this show: it must not appear.
    Skip,
    /// Show it, replacing the notification with this id if there is one.
    Show(Option<u32>),
}

/// What `LiveIds::finish` tells the show thread to do with the id that came back.
#[derive(Debug, PartialEq, Eq)]
enum Arrival {
    Keep,
    /// The tag was dismissed while the `Notify` was in flight: close the notification at once.
    CloseNow,
}

/// The notification server's id for each live notification, by tag. A same-tag `show` passes the old id as
/// `replaces_id` and `dismiss` closes by it (XDG `CloseNotification`). Shows of one tag are serialised: a second waits
/// for the first's id and then replaces it, so a tag never stacks two notifications. Every show and dismiss takes a
/// ticket on the calling thread; a show whose ticket was superseded before it ran is dropped (so a dismissed ring does
/// not come back), and only the newest show's thread reports a click (the server keeps one id for all of them).
// DILLA PATCH: close and replace by tag (core/vendor/README.md, "gpui-pre-linux").
#[derive(Clone)]
struct LiveIds {
    shared: Arc<(Mutex<HashMap<SharedString, Slot>>, Condvar)>,
    tickets: Arc<AtomicU64>,
    wait: Duration,
}

impl Default for LiveIds {
    fn default() -> Self {
        Self::with_wait(NOTIFY_WAIT)
    }
}

impl LiveIds {
    fn with_wait(wait: Duration) -> Self {
        Self {
            shared: Arc::default(),
            tickets: Arc::default(),
            wait,
        }
    }

    fn slots(&self) -> std::sync::MutexGuard<'_, HashMap<SharedString, Slot>> {
        self.shared.0.lock().unwrap()
    }

    fn next_ticket(&self) -> u64 {
        self.tickets.fetch_add(1, Ordering::SeqCst) + 1
    }

    /// Takes the ticket of a new show of `tag`, which supersedes every earlier show and dismiss of it.
    fn ticket(&self, tag: &SharedString) -> u64 {
        let mut slots = self.slots();
        let ticket = self.next_ticket();
        slots.entry(tag.clone()).or_default().current = ticket;
        ticket
    }

    /// Whether `ticket` is still the newest of `tag`.
    fn is_current(&self, tag: &str, ticket: u64) -> bool {
        self.slots()
            .get(tag)
            .is_some_and(|slot| slot.current == ticket)
    }

    /// Starts the show `ticket` of `tag`, waiting for any show of it already in flight (at most the wait limit: a
    /// `Notify` that never answers must not wedge the tag, so after that the show goes out without a replace).
    fn begin(&self, tag: &SharedString, ticket: u64) -> Begin {
        let changed = &self.shared.1;
        let mut slots = self.slots();
        let deadline = Instant::now() + self.wait;
        let mut gave_up = false;
        while slots.get(tag).is_some_and(|slot| slot.pending) {
            let Some(left) = deadline.checked_duration_since(Instant::now()) else {
                log::warn!(
                    "system notification {tag:?}: an earlier show never answered; showing without replacing"
                );
                gave_up = true;
                break;
            };
            slots = changed.wait_timeout(slots, left).unwrap().0;
        }
        let slot = slots.entry(tag.clone()).or_default();
        if slot.current != ticket {
            return Begin::Skip;
        }
        slot.pending = true;
        slot.dismissed = false;
        Begin::Show(if gave_up { None } else { slot.id })
    }

    /// The `Notify` that `begin` announced answered with `id`.
    fn finish(&self, tag: &str, id: u32) -> Arrival {
        let mut slots = self.slots();
        let mut arrival = Arrival::Keep;
        if let Some(slot) = slots.get_mut(tag) {
            slot.pending = false;
            if slot.dismissed {
                slot.dismissed = false;
                slot.id = None;
                arrival = Arrival::CloseNow;
            } else {
                slot.id = Some(id);
            }
        }
        self.shared.1.notify_all();
        arrival
    }

    /// The `Notify` that `begin` announced failed.
    fn abort(&self, tag: &str) {
        if let Some(slot) = self.slots().get_mut(tag) {
            slot.pending = false;
        }
        self.shared.1.notify_all();
    }

    /// Withdraws `tag`: supersedes its queued shows. Returns the id to close now; a `Notify` still in flight is marked
    /// to close on arrival instead.
    fn dismiss(&self, tag: &str) -> Option<u32> {
        let mut slots = self.slots();
        let ticket = self.next_ticket();
        let slot = slots.get_mut(tag)?;
        slot.current = ticket;
        if slot.pending {
            slot.dismissed = true;
            None
        } else {
            slot.id.take()
        }
    }

    /// The notification `id` is gone; forgets it unless the tag already moved on to a newer id.
    fn closed(&self, tag: &str, id: u32) {
        if let Some(slot) = self.slots().get_mut(tag)
            && slot.id == Some(id)
            && !slot.pending
        {
            slot.id = None;
        }
    }

    #[cfg(test)]
    fn id(&self, tag: &str) -> Option<u32> {
        self.slots().get(tag).and_then(|slot| slot.id)
    }
}

/// Sends `CloseNotification(id)` to the session bus's notification server.
fn close_notification(id: u32) -> zbus::Result<()> {
    futures::executor::block_on(async {
        zbus::Connection::session()
            .await?
            .call_method(
                Some("org.freedesktop.Notifications"),
                "/org/freedesktop/Notifications",
                Some("org.freedesktop.Notifications"),
                "CloseNotification",
                &(id,),
            )
            .await
            .map(drop)
    })
}

type ResponseCallback = Rc<RefCell<Option<Box<dyn FnMut(SystemNotificationResponse)>>>>;

pub(crate) struct SystemNotificationState {
    response_sender: mpsc::UnboundedSender<SystemNotificationResponse>,
    response_receiver: Option<mpsc::UnboundedReceiver<SystemNotificationResponse>>,
    callback: ResponseCallback,
    live: LiveIds,
    _response_task: Option<Task<()>>,
}

impl SystemNotificationState {
    pub(crate) fn new() -> Self {
        let (response_sender, response_receiver) = mpsc::unbounded();
        Self {
            response_sender,
            response_receiver: Some(response_receiver),
            callback: Rc::new(RefCell::new(None)),
            live: LiveIds::default(),
            _response_task: None,
        }
    }

    pub(crate) fn show(&self, app_name: Option<&str>, notification: SystemNotification) {
        let mut builder = notify_rust::Notification::new();
        if let Some(app_name) = app_name {
            builder.appname(app_name);
        }
        builder
            .summary(&notification.title)
            .body(&notification.body)
            .action(DEFAULT_ACTION, DEFAULT_ACTION);
        let mut action_ids = HashMap::new();
        for (index, action) in notification.actions.iter().enumerate() {
            let transport_id = format!("gpui-action-{index}");
            builder.action(&transport_id, &action.label);
            action_ids.insert(transport_id, action.id.clone());
        }
        let mut built = builder.finalize();

        let sender = self.response_sender.clone();
        let tag = notification.tag.clone();
        let live = self.live.clone();
        // Taken here, on the calling thread, so the order of shows and dismisses is the caller's.
        let ticket = live.ticket(&tag);
        // `show` connects to the session bus and `wait_for_action` blocks
        // until the notification closes, so both run off the UI thread.
        let spawn_result = std::thread::Builder::new()
            .name("system-notification".to_string())
            .spawn(move || {
                // A live notification with this tag is replaced in place.
                match live.begin(&tag, ticket) {
                    Begin::Skip => return,
                    Begin::Show(Some(old_id)) => {
                        built.id(old_id);
                    }
                    Begin::Show(None) => {}
                }
                match built.show() {
                    Ok(handle) => {
                        // `wait_for_action` consumes the handle, so take the id first.
                        let id = handle.id();
                        if live.finish(&tag, id) == Arrival::CloseNow {
                            if let Err(error) = close_notification(id) {
                                log::warn!("failed to close system notification {id}: {error}");
                            }
                            return;
                        }
                        let on_action = |action: &str| {
                            let action_id = match action {
                                // Sent by `notify-rust` when the notification closes
                                // without being activated.
                                "__closed" => {
                                    live.closed(&tag, id);
                                    return;
                                }
                                transport_id => {
                                    let Some(action_id) =
                                        response_action_id(transport_id, &action_ids)
                                    else {
                                        log::warn!(
                                            "system notification returned unknown action {transport_id:?}"
                                        );
                                        return;
                                    };
                                    action_id
                                }
                            };
                            // The server keeps one id for every same-tag show, and notify-rust wakes each thread
                            // waiting on it: only the newest show's thread (with the newest actions) answers.
                            if !live.is_current(&tag, ticket) {
                                return;
                            }
                            live.closed(&tag, id);
                            sender
                                .unbounded_send(SystemNotificationResponse {
                                    tag: tag.clone(),
                                    action_id,
                                })
                                .ok();
                        };
                        handle.wait_for_action(on_action)
                    }
                    Err(error) => {
                        live.abort(&tag);
                        log::warn!("failed to show system notification: {error}");
                    }
                }
            });
        if let Err(error) = spawn_result {
            log::warn!("failed to spawn system notification thread: {error}");
        }
    }

    pub(crate) fn dismiss(&self, tag: &str) {
        // A tag that is not live (never shown, already closed) has nothing to close.
        let Some(id) = self.live.dismiss(tag) else {
            return;
        };
        // The bus call blocks, so it runs off the UI thread.
        let spawned = std::thread::Builder::new()
            .name("system-notification-close".to_string())
            .spawn(move || {
                if let Err(error) = close_notification(id) {
                    log::warn!("failed to close system notification {id}: {error}");
                }
            });
        if let Err(error) = spawned {
            log::warn!("failed to spawn system notification close thread: {error}");
        }
    }

    pub(crate) fn on_response(
        &mut self,
        executor: &ForegroundExecutor,
        callback: Box<dyn FnMut(SystemNotificationResponse)>,
    ) {
        *self.callback.borrow_mut() = Some(callback);

        // Responses arrive from per-notification threads; this task hands
        // them to the registered callback on the main thread.
        if let Some(mut receiver) = self.response_receiver.take() {
            let callback = self.callback.clone();
            self._response_task = Some(executor.spawn(async move {
                while let Some(response) = receiver.next().await {
                    // Take the callback out for the call: it may re-enter the
                    // platform or replace itself.
                    let taken = callback.borrow_mut().take();
                    if let Some(mut taken) = taken {
                        taken(response);
                        callback.borrow_mut().get_or_insert(taken);
                    }
                }
            }));
        }
    }
}

fn response_action_id(
    transport_id: &str,
    action_ids: &HashMap<String, SharedString>,
) -> Option<Option<SharedString>> {
    if transport_id == DEFAULT_ACTION {
        Some(None)
    } else {
        action_ids.get(transport_id).cloned().map(Some)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tag() -> SharedString {
        "t".into()
    }

    /// Takes a ticket and begins it, expecting the show to go ahead; returns the id to replace.
    fn begin(ids: &LiveIds) -> Option<u32> {
        let ticket = ids.ticket(&tag());
        match ids.begin(&tag(), ticket) {
            Begin::Show(replaces) => replaces,
            Begin::Skip => panic!("show was skipped"),
        }
    }

    #[test]
    fn live_ids_replace_returns_the_old_id() {
        let ids = LiveIds::default();
        assert_eq!(begin(&ids), None);
        assert_eq!(ids.finish("t", 1), Arrival::Keep);
        assert_eq!(begin(&ids), Some(1));
        assert_eq!(ids.finish("t", 2), Arrival::Keep);
        assert_eq!(ids.id("t"), Some(2));
    }

    #[test]
    fn dismiss_takes_the_id_and_removes_it() {
        let ids = LiveIds::default();
        begin(&ids);
        ids.finish("t", 7);
        assert_eq!(ids.dismiss("t"), Some(7));
        assert_eq!(ids.dismiss("t"), None);
    }

    #[test]
    fn closed_ignores_a_newer_id() {
        let ids = LiveIds::default();
        begin(&ids);
        ids.finish("t", 2);
        ids.closed("t", 1);
        assert_eq!(ids.id("t"), Some(2));
        ids.closed("t", 2);
        assert_eq!(ids.id("t"), None);
    }

    #[test]
    fn dismiss_while_pending_closes_on_arrival() {
        let ids = LiveIds::default();
        begin(&ids);
        assert_eq!(ids.dismiss("t"), None);
        assert_eq!(ids.finish("t", 9), Arrival::CloseNow);
        assert_eq!(ids.id("t"), None);
        // The tag is free again: the next show starts fresh.
        assert_eq!(begin(&ids), None);
    }

    #[test]
    fn show_while_pending_replaces_not_stacks() {
        let ids = LiveIds::default();
        begin(&ids);
        // The second show is the newest, so it is the one that runs, once the first's id is known.
        let ticket = ids.ticket(&tag());
        let second = std::thread::spawn({
            let ids = ids.clone();
            move || ids.begin(&tag(), ticket)
        });
        std::thread::sleep(Duration::from_millis(100));
        assert!(!second.is_finished());
        ids.finish("t", 5);
        assert_eq!(second.join().unwrap(), Begin::Show(Some(5)));
    }

    #[test]
    fn a_queued_show_after_a_dismiss_is_dropped() {
        let ids = LiveIds::default();
        begin(&ids);
        let queued = ids.ticket(&tag());
        assert_eq!(ids.dismiss("t"), None);
        assert_eq!(ids.finish("t", 3), Arrival::CloseNow);
        assert_eq!(ids.begin(&tag(), queued), Begin::Skip);
        // A show after the dismiss is fine.
        assert_eq!(begin(&ids), None);
    }

    #[test]
    fn only_the_newest_show_answers_a_click() {
        let ids = LiveIds::default();
        let first = ids.ticket(&tag());
        assert!(ids.is_current("t", first));
        let second = ids.ticket(&tag());
        assert!(!ids.is_current("t", first));
        assert!(ids.is_current("t", second));
    }

    #[test]
    fn a_notify_that_never_answers_does_not_wedge_the_tag() {
        let ids = LiveIds::with_wait(Duration::from_millis(50));
        begin(&ids); // never finished
        let ticket = ids.ticket(&tag());
        let started = Instant::now();
        assert_eq!(ids.begin(&tag(), ticket), Begin::Show(None));
        assert!(started.elapsed() >= Duration::from_millis(50));
    }

    #[test]
    fn a_failed_notify_frees_the_tag() {
        let ids = LiveIds::default();
        begin(&ids);
        ids.abort("t");
        assert_eq!(begin(&ids), None);
    }

    /// Needs the session bus to be a private one: it claims `org.freedesktop.Notifications`, which on a desktop session
    /// belongs to the real notification server. Run under `dbus-run-session -- env DILLA_PRIVATE_BUS=1 cargo test ...`.
    #[test]
    fn dismiss_sends_close_notification_and_show_replaces() {
        if std::env::var("DILLA_PRIVATE_BUS").as_deref() != Ok("1") {
            eprintln!("skipped: DILLA_PRIVATE_BUS=1 not set (needs a private session bus)");
            return;
        }
        use std::sync::mpsc;
        use std::time::Duration;

        enum Seen {
            Notify { replaces_id: u32 },
            Close(u32),
        }
        struct Mock(
            std::sync::Mutex<mpsc::Sender<Seen>>,
            Arc<std::sync::atomic::AtomicU64>,
        );
        #[zbus::interface(name = "org.freedesktop.Notifications")]
        impl Mock {
            #[allow(clippy::too_many_arguments)]
            fn notify(
                &self,
                _app_name: String,
                replaces_id: u32,
                _app_icon: String,
                _summary: String,
                _body: String,
                _actions: Vec<String>,
                _hints: HashMap<String, zbus::OwnedVariant>,
                _expire_timeout: i32,
            ) -> u32 {
                self.0
                    .lock()
                    .unwrap()
                    .send(Seen::Notify { replaces_id })
                    .ok();
                std::thread::sleep(Duration::from_millis(
                    self.1.load(std::sync::atomic::Ordering::SeqCst),
                ));
                41
            }
            fn close_notification(&self, id: u32) {
                self.0.lock().unwrap().send(Seen::Close(id)).ok();
            }
            fn get_capabilities(&self) -> Vec<String> {
                vec!["actions".into()]
            }
            fn get_server_information(&self) -> (String, String, String, String) {
                ("mock".into(), "dilla".into(), "1".into(), "1.2".into())
            }
        }

        let (tx, rx) = mpsc::channel();
        let delay = Arc::new(std::sync::atomic::AtomicU64::new(0));
        let _bus = futures::executor::block_on(async {
            zbus::connection::Builder::session()
                .name("org.freedesktop.Notifications")
                .serve_at(
                    "/org/freedesktop/Notifications",
                    Mock(std::sync::Mutex::new(tx), delay.clone()),
                )
                .build()
                .await
        })
        .unwrap();

        let mut state = SystemNotificationState::new();
        let note = || SystemNotification {
            tag: "t".into(),
            title: "title".into(),
            body: "body".into(),
            actions: Vec::new(),
        };
        let wait = Duration::from_secs(10);

        state.show(Some("dilla"), note());
        assert!(matches!(
            rx.recv_timeout(wait).unwrap(),
            Seen::Notify { replaces_id: 0 }
        ));
        // `show` records the id after the call returns, on its own thread.
        let deadline = std::time::Instant::now() + wait;
        while state.live.id("t").is_none() {
            assert!(std::time::Instant::now() < deadline, "id never recorded");
            std::thread::sleep(Duration::from_millis(10));
        }

        state.show(Some("dilla"), note());
        assert!(matches!(
            rx.recv_timeout(wait).unwrap(),
            Seen::Notify { replaces_id: 41 }
        ));

        // The replacing show records its (same) id on its own thread after the mock answered.
        std::thread::sleep(Duration::from_millis(300));
        state.dismiss("t");
        assert!(matches!(rx.recv_timeout(wait).unwrap(), Seen::Close(41)));
        assert_eq!(state.live.id("t"), None);

        // A dismiss while the Notify is in flight (the server is slow to answer) closes on arrival, and a second
        // show of the tag meanwhile waits for the first id instead of stacking.
        delay.store(400, std::sync::atomic::Ordering::SeqCst);
        state.show(Some("dilla"), note());
        assert!(matches!(
            rx.recv_timeout(wait).unwrap(),
            Seen::Notify { replaces_id: 0 }
        ));
        state.dismiss("t");
        assert!(matches!(rx.recv_timeout(wait).unwrap(), Seen::Close(41)));
        assert_eq!(state.live.id("t"), None);

        // Two shows of the tag share the server's id 41; one click on it is one response, not one per show.
        delay.store(0, std::sync::atomic::Ordering::SeqCst);
        let mut responses = state.response_receiver.take().unwrap();
        for _ in 0..2 {
            state.show(Some("dilla"), note());
            assert!(matches!(
                rx.recv_timeout(wait).unwrap(),
                Seen::Notify { .. }
            ));
        }
        std::thread::sleep(Duration::from_millis(300));
        futures::executor::block_on(_bus.emit_signal(
            None::<&str>,
            "/org/freedesktop/Notifications",
            "org.freedesktop.Notifications",
            "ActionInvoked",
            &(41u32, "default"),
        ))
        .unwrap();
        std::thread::sleep(Duration::from_millis(500));
        let mut count = 0;
        while let Ok(response) = responses.try_recv() {
            assert_eq!(response.tag, SharedString::from("t"));
            count += 1;
        }
        assert_eq!(count, 1, "one click, one response");
    }

    #[test]
    fn caller_action_ids_do_not_collide_with_transport_action_ids() {
        let action_ids = HashMap::from([
            ("gpui-action-0".to_string(), SharedString::from("default")),
            ("gpui-action-1".to_string(), SharedString::from("__closed")),
        ]);

        assert_eq!(response_action_id(DEFAULT_ACTION, &action_ids), Some(None));
        assert_eq!(
            response_action_id("gpui-action-0", &action_ids),
            Some(Some("default".into()))
        );
        assert_eq!(
            response_action_id("gpui-action-1", &action_ids),
            Some(Some("__closed".into()))
        );
        assert_eq!(response_action_id("unknown", &action_ids), None);
    }
}
