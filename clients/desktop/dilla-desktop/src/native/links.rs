//! `dilla://` links (Package 6; beta.5 `policy.ts` `parseInvite`, `main.ts` `openInvite`, `confirmInstance`,
//! `open-url`).
//!
//! A link reaches the app three ways, and all three end in [`super::NativeShell::open_link`]:
//! - the command line of the first launch ([`super::claim`] parses it with [`parse`]);
//! - a second launch, handed over by [`super::single_instance`];
//! - macOS `open-url` ([`prepare`] registers `Application::on_open_urls`; its callback has no app, so the URLs go
//!   through a channel to the UI thread, where [`install`] drains it). GPUI calls it on macOS only: Linux and Windows
//!   start a process with the link on its command line, which [`super::single_instance`] hands over.
//!
//! The shell queues links that come before the handler and drops every link while the app asks to quit or quits
//! (beta.5 `if (askingQuit || quitting) return`). The handler [`install`] sets routes a link (beta.5 `openInvite`):
//! - a listed instance: its window opens on the join dialog with the invite (beta.5 `openInstance(instance,
//!   '/welcome?invite=<code>')`); an open window is brought forward and its join dialog opens;
//! - an instance that is not listed: the instances window takes the link and asks first ("Connect to {host}? Only
//!   continue if you recognize this address.", beta.5 `confirmInstance`), and nothing is stored until Connect.
//!
//! **Scheme registration** (beta.5 `app.setAsDefaultProtocolClient('dilla')` when packaged) belongs to the packages
//! (release plan Tasks 2, 12 and 13; this lifts its D12). [`REGISTRATION`] lists what each needs.

use std::sync::{Mutex, OnceLock};

use dilla_client::instances::Instances;
use dilla_client::instances::address;
use dilla_client::seams::InstanceConfig;
use gpui::{App, Application};
use tokio::sync::mpsc;

use super::{Host, Link, NativeShell};
use crate::screens::instances::copy;
use crate::windows;

/// What each package registers so the OS starts (or hands to) Dilla for a `dilla://` link (release plan Tasks 2, 12,
/// 13). The app needs nothing more: the link arrives on the command line (Linux, Windows) or through `open-url`
/// (macOS).
pub const REGISTRATION: &[(&str, &str)] = &[
    (
        "linux (.desktop, Task 2)",
        "MimeType=x-scheme-handler/dilla; and Exec=dilla-desktop %U; the package's postinst runs update-desktop-database \
         (xdg-mime default <desktop file> x-scheme-handler/dilla for the AppImage's own integration)",
    ),
    (
        "macos (Info.plist, Task 12)",
        "CFBundleURLTypes = [{ CFBundleURLName = <bundle id>; CFBundleURLSchemes = [dilla] }]",
    ),
    (
        "windows (installer, Task 13)",
        "HKCU\\Software\\Classes\\dilla: (Default) = \"URL:Dilla\", \"URL Protocol\" = \"\"; \
         HKCU\\Software\\Classes\\dilla\\shell\\open\\command: (Default) = \"\\\"<install dir>\\dilla-desktop.exe\\\" \\\"%1\\\"\"; \
         removed by the uninstaller",
    ),
];

/// One command-line argument or URL as a link, or `None` when it is not exactly the invite grammar
/// `dilla://invite/<host[:port]>/<code>` (beta.5 `parseInvite`, ported as [`address::parse_invite`]): no credentials,
/// query, fragment, encoded separator or extra path, and a host that makes an `https` instance.
pub fn parse(raw: &str) -> Option<Link> {
    let input = address::parse_invite(raw).ok()?;
    Some(Link::Invite {
        host: authority(&input.instance),
        code: input.invite?,
    })
}

/// The link again as text (what the instances window's invite field takes).
pub fn to_url(link: &Link) -> String {
    let Link::Invite { host, code } = link;
    format!("dilla://invite/{host}/{code}")
}

/// `host` or `host:port`: the link's part that names the instance.
fn authority(instance: &url::Url) -> String {
    let host = instance.host_str().unwrap_or_default();
    match instance.port() {
        Some(port) => format!("{host}:{port}"),
        None => host.to_owned(),
    }
}

/// The URLs macOS hands over (`on_open_urls`), on their way to the UI thread.
struct OpenUrls {
    tx: mpsc::UnboundedSender<Vec<String>>,
    rx: Mutex<Option<mpsc::UnboundedReceiver<Vec<String>>>>,
}

fn open_urls() -> &'static OpenUrls {
    static URLS: OnceLock<OpenUrls> = OnceLock::new();
    URLS.get_or_init(|| {
        let (tx, rx) = mpsc::unbounded_channel();
        OpenUrls {
            tx,
            rx: Mutex::new(Some(rx)),
        }
    })
}

/// Called once by [`super::prepare`], before the run loop: macOS `open-url`. A link that comes before the app is ready
/// waits in the channel (beta.5 `pendingInvites`), then in the shell's queue.
pub fn prepare(app: &Application) {
    let urls = open_urls();
    app.on_open_urls(move |list| {
        let _ = urls.tx.send(list);
    });
}

/// Called once by [`super::install`]: the link handler, and the drain of macOS `open-url`.
pub fn install(cx: &mut App) {
    NativeShell::on_link(Box::new(route), cx);
    let rx = open_urls()
        .rx
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .take();
    if let Some(mut rx) = rx {
        cx.spawn(async move |cx| {
            while let Some(list) = rx.recv().await {
                cx.update(|cx| {
                    for raw in &list {
                        match parse(raw) {
                            Some(link) => {
                                NativeShell::open_link(link, cx);
                            }
                            None => log::warn!(
                                "dilla-desktop: a dilla:// link that is not an invite was ignored"
                            ),
                        }
                    }
                });
            }
        })
        .detach();
    }
}

/// beta.5 `openInvite`: a listed instance's window takes the invite; an unlisted one is asked about in the instances
/// window first.
fn route(link: Link, cx: &mut App) {
    let Some(host) = NativeShell::host(cx) else {
        return;
    };
    let Link::Invite {
        host: authority,
        code,
    } = &link;
    let Ok(instance) = address::canonical_instance(&format!("https://{authority}/"), false) else {
        return;
    };
    let listed = host
        .launch
        .root
        .as_deref()
        .and_then(|root| Instances::load(root).ok())
        .and_then(|list| list.find(&instance).cloned());
    match listed {
        Some(cfg) => open_listed(cfg, code.clone(), &host, cx),
        None => ask_first(&link, &host, cx),
    }
}

/// The instance is listed: its open window shows the join dialog with the invite, or its window opens with it.
fn open_listed(cfg: InstanceConfig, code: String, host: &Host, cx: &mut App) {
    let open: Vec<_> = NativeShell::open_windows(cx)
        .into_iter()
        .filter(|(_, instance)| *instance == Some(cfg.id))
        .map(|(id, _)| id)
        .collect();
    for id in open {
        let Some(handle) = cx.windows().into_iter().find(|w| w.window_id() == id) else {
            continue;
        };
        let code = code.clone();
        let shown = handle.update(cx, |_, window, cx| {
            window.activate_window();
            crate::screens::create::open_join_in(window, Some(code), cx);
        });
        if shown.is_ok() {
            cx.activate(true);
            return;
        }
    }
    if let Err(e) = windows::open_instance(cx, &host.launch, cfg, Some(code), host.allow_loopback) {
        // beta.5 `dialog.showErrorBox('Could not open this invite', …)`: the instances window says why.
        let message = match &e {
            windows::OpenError::Instance(dilla_client::instances::InstanceError::InUse) => {
                copy::ERROR_IN_USE
            }
            _ => copy::ERROR_STORE,
        };
        if let Ok(manager) = windows::open_manager(cx, &host.launch, host.allow_loopback) {
            let _ = manager.update(cx, |view, _, cx| view.show_error(message.to_owned(), cx));
        }
    }
}

/// The instance is not listed: the instances window takes the link and asks before it adds the instance (beta.5
/// `confirmInstance`). Nothing is stored or contacted until the person chooses Connect.
fn ask_first(link: &Link, host: &Host, cx: &mut App) {
    match windows::open_manager(cx, &host.launch, host.allow_loopback) {
        Ok(manager) => {
            let text = to_url(link);
            let _ = manager.update(cx, |view, window, cx| {
                view.set_invite(&text, window, cx);
                view.connect_invite(window, cx);
            });
            cx.activate(true);
        }
        Err(e) => log::warn!("dilla-desktop: the instances window did not open for an invite: {e}"),
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use std::path::PathBuf;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicU64, Ordering};

    use dilla_client::keychain::MemoryKeychain;
    use gpui::TestAppContext;

    use super::*;
    use crate::Launch;
    use crate::screens::instances::ManagerView;

    fn invite(host: &str, code: &str) -> Option<Link> {
        Some(Link::Invite {
            host: host.into(),
            code: code.into(),
        })
    }

    pub(crate) fn temp_root(tag: &str) -> PathBuf {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        let p = std::env::temp_dir().join(format!(
            "dilla-native-{tag}-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_dir_all(&p);
        std::fs::create_dir_all(&p).unwrap();
        p
    }

    /// beta.5 `test/policy.test.ts` "invites carry only a validated HTTPS instance and code, with no credentials".
    #[test]
    fn parses_only_the_invite_grammar() {
        assert_eq!(
            parse("dilla://invite/example.com/ABCD-EFGH"),
            invite("example.com", "ABCD-EFGH")
        );
        assert_eq!(
            parse("dilla://invite/chat.example:8443/abc_DEF"),
            invite("chat.example:8443", "abc_DEF")
        );
        // The default port names the same instance.
        assert_eq!(
            parse("dilla://invite/EXAMPLE.com:443/x"),
            invite("example.com", "x")
        );
        for bad in [
            "dilla://invite/a@b/code",
            "dilla://invite/b/a/b",
            "dilla://invite/b/code?token=secret",
            "dilla://invite/b/code#x",
            "dilla://invite/b/%2e%2e",
            "dilla://invite/b/%2F",
            "dilla://auth/b/token",
            "dilla://invite/b/",
            "https://invite/b/code",
            "dilla://invite/b/code\n",
            "dilla://invite/b./code",
            "--hidden",
            "dilla-desktop",
        ] {
            assert_eq!(parse(bad), None, "{bad:?}");
        }
        let long = format!("dilla://invite/b/{}", "a".repeat(129));
        assert_eq!(parse(&long), None);
        let link = parse("dilla://invite/chat.example:8443/abc_DEF").unwrap();
        assert_eq!(parse(&to_url(&link)), Some(link));
    }

    fn install_host(root: &std::path::Path, cx: &mut TestAppContext) {
        let launch = Launch::live(Some(root.to_path_buf()), Arc::new(MemoryKeychain::new()));
        cx.update(|cx| {
            crate::install(cx);
            NativeShell::of(cx).host = Some(Host {
                launch,
                allow_loopback: false,
            });
            NativeShell::on_link(Box::new(route), cx);
        });
    }

    fn manager(cx: &mut TestAppContext) -> gpui::WindowHandle<ManagerView> {
        cx.windows()
            .into_iter()
            .find_map(|w| w.downcast::<ManagerView>())
            .expect("the instances window is open")
    }

    /// beta.5 `confirmInstance`: a link to an instance that is not listed opens the instances window on the question,
    /// and nothing is stored before Connect (Cancel stores nothing; Connect adds it and opens it with the invite).
    #[gpui::test]
    fn unknown_instance_asks_first(cx: &mut TestAppContext) {
        let root = temp_root("links-unknown");
        install_host(&root, cx);
        cx.update(|cx| {
            NativeShell::open_link(parse("dilla://invite/new.example/CODE-1").unwrap(), cx)
        });
        cx.run_until_parked();
        let window = manager(cx);
        assert!(
            window.read_with(cx, |v, _| v.confirming()).unwrap(),
            "the unknown instance is asked about"
        );
        assert!(
            Instances::load(&root).unwrap().list().is_empty(),
            "nothing is stored before Connect"
        );
        window
            .update(cx, |v, window, cx| v.cancel_confirmation(window, cx))
            .unwrap();
        assert!(Instances::load(&root).unwrap().list().is_empty());
        cx.update(windows::release_all);
    }

    /// beta.5 `openInstance(instance, '/welcome?invite=<code>')`: a listed instance asks nothing; its window opens with
    /// the invite, which its welcome or join flow takes.
    #[gpui::test]
    fn a_listed_instance_opens_on_the_invite(cx: &mut TestAppContext) {
        let root = temp_root("links-listed");
        let cfg = Instances::load(&root)
            .unwrap()
            .add(url::Url::parse("https://chat.example:8443/").unwrap())
            .unwrap();
        install_host(&root, cx);
        // The window's client start runs on a real OS thread.
        cx.executor().allow_parking();
        cx.update(|cx| {
            NativeShell::open_link(
                parse("dilla://invite/chat.example:8443/CODE-7").unwrap(),
                cx,
            )
        });
        let window = cx
            .windows()
            .into_iter()
            .find_map(|w| w.downcast::<crate::AppView>())
            .expect("the instance's window opened");
        assert!(
            cx.windows()
                .iter()
                .all(|w| w.downcast::<ManagerView>().is_none())
        );
        let (invite, instance) = window
            .read_with(cx, |v, _| {
                (v.pending_invite().map(str::to_owned), v.instance())
            })
            .unwrap();
        assert_eq!(invite.as_deref(), Some("CODE-7"));
        assert_eq!(instance, Some(cfg.id));
        cx.update(windows::release_all);
    }

    /// No link opens anything while the app asks to quit or quits (beta.5 `openInvite`: `if (askingQuit || quitting)
    /// return`): no window opens and the link is not kept for later.
    #[gpui::test]
    fn no_link_opens_during_quit(cx: &mut TestAppContext) {
        let root = temp_root("links-quit");
        install_host(&root, cx);
        cx.update(|cx| {
            NativeShell::set_quit_prompt(Box::new(|_, _| {}), cx);
            NativeShell::add_quit_guard(
                dilla_client::seams::InstanceId(0),
                Box::new(|_| super::super::GuardState::UNANSWERED),
                cx,
            );
            NativeShell::request_quit(cx);
            assert!(NativeShell::is_asking_quit(cx));
            assert!(!NativeShell::open_link(
                parse("dilla://invite/new.example/CODE-2").unwrap(),
                cx
            ));
        });
        cx.run_until_parked();
        assert!(
            cx.windows().is_empty(),
            "no window opened while asking to quit"
        );
        cx.update(|cx| {
            NativeShell::finish_quit(super::super::QuitAnswer::Stay, cx);
            NativeShell::quit_now(cx);
            assert!(!NativeShell::open_link(
                parse("dilla://invite/new.example/CODE-3").unwrap(),
                cx
            ));
        });
        cx.run_until_parked();
        assert!(cx.windows().is_empty(), "no window opened while quitting");
    }
}
