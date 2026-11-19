//! System notifications over the XDG notifications D-Bus interface, via
//! `notify-rust`.

use std::cell::RefCell;
use std::collections::HashMap;
use std::rc::Rc;
use std::sync::{Arc, Condvar, Mutex};

use futures::StreamExt as _;
use futures::channel::mpsc;
use gpui::{
    ForegroundExecutor, SharedString, SystemNotification, SystemNotificationResponse, Task,
};

/// The XDG action key invoked when the user activates the notification body
/// rather than a specific action button.
const DEFAULT_ACTION: &str = "default";

/// What `LiveIds` knows of one tag.
#[derive(Default)]
struct Slot {
    /// The notification server's id, once a `Notify` for the tag has answered.
    id: Option<u32>,
    /// A `Notify` for the tag is in flight.
    pending: bool,
    /// `dismiss` arrived while it was in flight.
    dismissed: bool,
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
/// for the first's id and then replaces it, so a tag never stacks two notifications. A `dismiss` that arrives while a
/// `Notify` is in flight is honoured when the id comes back.
// DILLA PATCH: close and replace by tag (core/vendor/README.md, "gpui-pre-linux").
#[derive(Clone, Default)]
struct LiveIds(Arc<(Mutex<HashMap<SharedString, Slot>>, Condvar)>);

impl LiveIds {
    /// Starts a show of `tag`, waiting for any show of it already in flight. Returns the id to replace.
    fn begin(&self, tag: &SharedString) -> Option<u32> {
        let (slots, changed) = &*self.0;
        let mut slots = slots.lock().unwrap();
        while slots.get(tag).is_some_and(|slot| slot.pending) {
            slots = changed.wait(slots).unwrap();
        }
        let slot = slots.entry(tag.clone()).or_default();
        slot.pending = true;
        slot.dismissed = false;
        slot.id
    }

    /// The `Notify` that `begin` announced answered with `id`.
    fn finish(&self, tag: &str, id: u32) -> Arrival {
        let (slots, changed) = &*self.0;
        let mut slots = slots.lock().unwrap();
        let arrival = match slots.get_mut(tag) {
            Some(slot) if slot.dismissed => {
                slots.remove(tag);
                Arrival::CloseNow
            }
            Some(slot) => {
                slot.pending = false;
                slot.id = Some(id);
                Arrival::Keep
            }
            None => Arrival::Keep,
        };
        changed.notify_all();
        arrival
    }

    /// The `Notify` that `begin` announced failed.
    fn abort(&self, tag: &str) {
        let (slots, changed) = &*self.0;
        let mut slots = slots.lock().unwrap();
        if let Some(slot) = slots.get_mut(tag) {
            slot.pending = false;
            if slot.id.is_none() {
                slots.remove(tag);
            }
        }
        changed.notify_all();
    }

    /// Forgets `tag`. Returns the id to close now; a `Notify` still in flight is marked to close on arrival instead.
    fn dismiss(&self, tag: &str) -> Option<u32> {
        let mut slots = self.0.0.lock().unwrap();
        let slot = slots.get_mut(tag)?;
        if slot.pending {
            slot.dismissed = true;
            None
        } else {
            slots.remove(tag).and_then(|slot| slot.id)
        }
    }

    /// The notification `id` is gone; forgets it unless the tag already moved on to a newer id.
    fn closed(&self, tag: &str, id: u32) {
        let mut slots = self.0.0.lock().unwrap();
        if slots
            .get(tag)
            .is_some_and(|slot| slot.id == Some(id) && !slot.pending)
        {
            slots.remove(tag);
        }
    }

    #[cfg(test)]
    fn id(&self, tag: &str) -> Option<u32> {
        self.0.0.lock().unwrap().get(tag).and_then(|slot| slot.id)
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
        // `show` connects to the session bus and `wait_for_action` blocks
        // until the notification closes, so both run off the UI thread.
        let spawn_result = std::thread::Builder::new()
            .name("system-notification".to_string())
            .spawn(move || {
                // A live notification with this tag is replaced in place.
                if let Some(old_id) = live.begin(&tag) {
                    built.id(old_id);
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
                        handle.wait_for_action(|action| {
                    let action_id = match action {
                        // Sent by `notify-rust` when the notification closes
                        // without being activated.
                        "__closed" => {
                            live.closed(&tag, id);
                            return;
                        }
                        transport_id => {
                            let Some(action_id) = response_action_id(transport_id, &action_ids)
                            else {
                                log::warn!(
                                    "system notification returned unknown action {transport_id:?}"
                                );
                                return;
                            };
                            action_id
                        }
                    };
                    live.closed(&tag, id);
                    sender
                        .unbounded_send(SystemNotificationResponse {
                            tag: tag.clone(),
                            action_id,
                        })
                        .ok();
                        })
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

    #[test]
    fn live_ids_replace_returns_the_old_id() {
        let ids = LiveIds::default();
        assert_eq!(ids.begin(&tag()), None);
        assert_eq!(ids.finish("t", 1), Arrival::Keep);
        assert_eq!(ids.begin(&tag()), Some(1));
        assert_eq!(ids.finish("t", 2), Arrival::Keep);
        assert_eq!(ids.id("t"), Some(2));
    }

    #[test]
    fn dismiss_takes_the_id_and_removes_it() {
        let ids = LiveIds::default();
        ids.begin(&tag());
        ids.finish("t", 7);
        assert_eq!(ids.dismiss("t"), Some(7));
        assert_eq!(ids.dismiss("t"), None);
    }

    #[test]
    fn closed_ignores_a_newer_id() {
        let ids = LiveIds::default();
        ids.begin(&tag());
        ids.finish("t", 2);
        ids.closed("t", 1);
        assert_eq!(ids.id("t"), Some(2));
        ids.closed("t", 2);
        assert_eq!(ids.id("t"), None);
    }

    #[test]
    fn dismiss_while_pending_closes_on_arrival() {
        let ids = LiveIds::default();
        ids.begin(&tag());
        assert_eq!(ids.dismiss("t"), None);
        assert_eq!(ids.finish("t", 9), Arrival::CloseNow);
        assert_eq!(ids.id("t"), None);
        // The tag is free again: the next show starts fresh.
        assert_eq!(ids.begin(&tag()), None);
    }

    #[test]
    fn show_while_pending_replaces_not_stacks() {
        let ids = LiveIds::default();
        ids.begin(&tag());
        let second = std::thread::spawn({
            let ids = ids.clone();
            move || ids.begin(&tag())
        });
        // The second show waits for the first's id.
        std::thread::sleep(std::time::Duration::from_millis(100));
        assert!(!second.is_finished());
        ids.finish("t", 5);
        assert_eq!(second.join().unwrap(), Some(5));
    }

    #[test]
    fn a_failed_notify_frees_the_tag() {
        let ids = LiveIds::default();
        ids.begin(&tag());
        ids.abort("t");
        assert_eq!(ids.begin(&tag()), None);
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

        let state = SystemNotificationState::new();
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
