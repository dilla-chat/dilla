//! The in-app updater (release plan Task 14b, decision D9: our own minimal updater on every OS, no Sparkle).
//!
//! - [`check`] reads the signed feed ([`FEED_URL`] and its `.sig`, at most 1 MiB and 128 bytes) and hands it to
//!   [`feed::verify`]: the signature under the pinned keys before anything parses it, then the channel, the expiry and
//!   a version strictly above this one ([`feed::select`]).
//! - [`download`] streams this platform's asset into a staging directory chosen by the platform (beside the AppImage or
//!   the app bundle, so the swap stays on one file system; the temporary directory on Windows), held to the feed's size
//!   and SHA-512 as it arrives ([`feed::Checked`]); anything else is deleted.
//! - The platform module prepares the download (`apply_linux`, `apply_mac`, `apply_win`) and applies it when the app
//!   quits for "Restart to update" ([`super::native::QuitReason::UpdateRestart`], which asks the quit guards first):
//!   an AppImage is renamed over itself and started again, a macOS bundle is checked against the Developer ID team and
//!   swapped in one step, and the Windows installer runs silently. An update that cannot be applied starts the old app
//!   again.
//! - **When:** at start and every [`CHECK_EVERY`] (a release build), and on "Check for updates".
//!
//! **UI** (beta.5 `main.ts` `refreshMenu`: the update rows of the tray and the macOS application menu, its tooltip and
//! its dialogs): "Check for updates" / "Checking for updates…" / "Update failed — retry"; for a copy the updater does
//! not replace, "Dilla {version} is available…" with "Update using your package manager" or the release page; once an
//! update is ready, "Restart to update". beta.5's message boxes after a check the person asked for ("Dilla is up to
//! date.", "Could not update Dilla") are OS notifications here. The copy is in `packages/web/src/strings/desktopUpdate.ts`.

#[cfg(not(any(target_os = "macos", target_os = "windows")))]
pub mod apply_linux;
pub mod apply_mac;
pub mod apply_win;
pub mod feed;

#[cfg(not(any(target_os = "macos", target_os = "windows")))]
use apply_linux as platform;
#[cfg(target_os = "macos")]
use apply_mac as platform;
#[cfg(target_os = "windows")]
use apply_win as platform;

use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::rc::Rc;
use std::time::Duration;

use gpui::{App, Global, SharedString, SystemNotification, Task};
use jiff::Timestamp;
use semver::Version;
use tokio::sync::mpsc;

use crate::native::{NativeShell, QuitReason, TrayItem, TraySection, TraySlot};
use crate::strings::{t, tf};
use feed::{Checked, DownloadError, FeedError, Trust, Update};

/// The signed feed (release plan Task 9).
pub const FEED_URL: &str = "https://dilla-chat.github.io/dilla/gpui-alpha/desktop-gpui-alpha.json";
/// How often a running app checks.
pub const CHECK_EVERY: Duration = Duration::from_secs(6 * 60 * 60);
/// The flag a launch started by the updater carries: it waits for the quitting app's single-instance lock
/// (`native::single_instance`).
pub const RELAUNCHED: &str = "--updated";
/// The name every staging directory starts with.
const STAGING_PREFIX: &str = ".dilla-update-";
/// The release page of a version (the tag `desktop-gpui-v<version>`, release plan D1).
const RELEASE_PAGE: &str = "https://github.com/dilla-chat/dilla/releases/tag/desktop-gpui-v";

/// This platform's asset (release plan: `dilla-<v>-<os>-<arch>.<ext>`).
pub const ASSET_SUFFIX: &str = if cfg!(target_os = "macos") {
    "mac-arm64.zip"
} else if cfg!(target_os = "windows") {
    "win-x64-setup.exe"
} else {
    "linux-x64.AppImage"
};

/// The running version (the crate version equals the release tag's, release plan D1).
pub fn current_version() -> Version {
    Version::parse(env!("CARGO_PKG_VERSION")).expect("the crate version is semver")
}

/// Why an update did not happen.
#[derive(Debug)]
pub enum Error {
    /// The feed or the download could not be fetched.
    Network(String),
    /// The feed did not pass its checks.
    Feed(FeedError),
    /// The download is not the feed's asset.
    Download(DownloadError),
    /// The staging directory could not be made: this copy cannot replace itself.
    NotWritable(std::io::Error),
    Io(std::io::Error),
    /// The platform step refused or failed.
    Apply(String),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Network(e) => write!(f, "network: {e}"),
            Self::Feed(e) => write!(f, "feed: {e}"),
            Self::Download(e) => write!(f, "download: {e}"),
            Self::NotWritable(e) => write!(f, "the staging directory: {e}"),
            Self::Io(e) => write!(f, "{e}"),
            Self::Apply(e) => write!(f, "{e}"),
        }
    }
}

impl std::error::Error for Error {}

impl From<std::io::Error> for Error {
    fn from(e: std::io::Error) -> Self {
        Self::Io(e)
    }
}

impl From<FeedError> for Error {
    fn from(e: FeedError) -> Self {
        Self::Feed(e)
    }
}

impl From<DownloadError> for Error {
    fn from(e: DownloadError) -> Self {
        Self::Download(e)
    }
}

impl Error {
    /// The person's message (a `desktop.update.error.*` key).
    fn message_key(&self) -> &'static str {
        match self {
            Self::Network(_) => "desktop.update.error.network",
            Self::Feed(_) | Self::Download(_) => "desktop.update.error.invalid",
            Self::NotWritable(_) | Self::Io(_) | Self::Apply(_) => "desktop.update.error.install",
        }
    }
}

/// How this copy of the app is installed, which decides what an update does.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Installed {
    /// The app replaces itself; downloads are staged under `staging`.
    SelfUpdating { staging: PathBuf },
    /// A deb or rpm (or AUR) install: the package manager updates it.
    PackageManager,
    /// Any other copy: the person downloads the new version.
    Manual,
}

/// How this copy is installed (the platform's rules).
pub fn installed() -> Installed {
    platform::installed()
}

// ---------------------------------------------------------------------------------------------------------------------
// Check and download
// ---------------------------------------------------------------------------------------------------------------------

fn client(follow_redirects: bool) -> Result<reqwest::Client, Error> {
    let redirects = if follow_redirects {
        // GitHub's release downloads redirect to its asset host; the bytes are held to the signed hash either way.
        reqwest::redirect::Policy::custom(|attempt| {
            if attempt.previous().len() >= 5 {
                attempt.error("too many redirects")
            } else if attempt.url().scheme() != "https" {
                attempt.error("a redirect left https")
            } else {
                attempt.follow()
            }
        })
    } else {
        // GitHub Pages serves the feed itself.
        reqwest::redirect::Policy::none()
    };
    reqwest::Client::builder()
        .https_only(true)
        .redirect(redirects)
        .connect_timeout(Duration::from_secs(15))
        .read_timeout(Duration::from_secs(60))
        .build()
        .map_err(|e| Error::Network(e.to_string()))
}

fn network(e: reqwest::Error) -> Error {
    Error::Network(e.without_url().to_string())
}

/// GETs `url` (status 200), refusing more than `max` bytes before reading them all.
async fn get_limited(client: &reqwest::Client, url: &str, max: usize) -> Result<Vec<u8>, Error> {
    let mut response = client.get(url).send().await.map_err(network)?;
    if response.status() != reqwest::StatusCode::OK {
        return Err(Error::Network(format!("HTTP {}", response.status())));
    }
    if response.content_length().is_some_and(|n| n > max as u64) {
        return Err(FeedError::Size.into());
    }
    let mut body = Vec::new();
    while let Some(chunk) = response.chunk().await.map_err(network)? {
        if body.len() + chunk.len() > max {
            return Err(FeedError::Size.into());
        }
        body.extend_from_slice(&chunk);
    }
    Ok(body)
}

/// Reads and checks the feed at `now`: the update for this platform when the feed offers a newer version.
pub async fn check(now: Timestamp) -> Result<Option<Update>, Error> {
    let client = client(false)?;
    let feed = get_limited(&client, FEED_URL, feed::MAX_FEED).await?;
    let sig = get_limited(&client, &format!("{FEED_URL}.sig"), feed::MAX_SIG).await?;
    let release = feed::verify(&feed, &sig, Trust::pinned(), now)?;
    Ok(feed::select(&release, &current_version(), ASSET_SUFFIX))
}

/// Downloads `update` into this copy's staging directory.
pub async fn download(update: &Update) -> Result<PathBuf, Error> {
    match installed() {
        Installed::SelfUpdating { staging } => download_into(update, &staging).await,
        _ => Err(Error::Apply("this copy does not replace itself".into())),
    }
}

/// A new private staging directory under `parent`.
fn staging_dir(parent: &Path) -> Result<PathBuf, Error> {
    let mut id = [0u8; 8];
    getrandom::fill(&mut id).map_err(|e| Error::Apply(e.to_string()))?;
    let hex: String = id.iter().map(|b| format!("{b:02x}")).collect();
    let dir = parent.join(format!("{STAGING_PREFIX}{hex}"));
    #[cfg_attr(not(unix), expect(unused_mut, reason = "only Unix sets the mode"))]
    let mut builder = std::fs::DirBuilder::new();
    #[cfg(unix)]
    std::os::unix::fs::DirBuilderExt::mode(&mut builder, 0o700);
    builder.create(&dir).map_err(Error::NotWritable)?;
    Ok(dir)
}

/// Streams `update` into a new staging directory under `parent`; on any failure the directory is removed.
pub async fn download_into(update: &Update, parent: &Path) -> Result<PathBuf, Error> {
    let dir = staging_dir(parent)?;
    let path = dir.join(&update.asset.name);
    let result = fetch_checked(update, &path).await;
    if result.is_err() {
        let _ = std::fs::remove_dir_all(&dir);
    }
    result.map(|()| path)
}

async fn fetch_checked(update: &Update, path: &Path) -> Result<(), Error> {
    let client = client(true)?;
    let mut response = client.get(update.url()).send().await.map_err(network)?;
    if response.status() != reqwest::StatusCode::OK {
        return Err(Error::Network(format!("HTTP {}", response.status())));
    }
    if response
        .content_length()
        .is_some_and(|n| n != update.asset.size)
    {
        return Err(DownloadError::Size.into());
    }
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(path)?;
    let mut checked = Checked::new(&update.asset);
    while let Some(chunk) = response.chunk().await.map_err(network)? {
        checked.push(&chunk)?;
        file.write_all(&chunk)?;
    }
    checked.finish()?;
    file.sync_all()?;
    Ok(())
}

/// Re-reads a staged file and holds it to the asset again (right before an AppImage or installer is used).
pub fn recheck(path: &Path, update: &Update) -> Result<(), Error> {
    use std::io::Read as _;
    let mut file = std::fs::File::open(path)?;
    let mut checked = Checked::new(&update.asset);
    let mut buf = vec![0u8; 1 << 16];
    loop {
        let n = file.read(&mut buf)?;
        if n == 0 {
            break;
        }
        checked.push(&buf[..n])?;
    }
    Ok(checked.finish()?)
}

/// Removes the staging directories a previous run left (a crash, a refused update) under `parent`.
fn sweep(parent: &Path) {
    let Ok(entries) = std::fs::read_dir(parent) else {
        return;
    };
    for e in entries.flatten() {
        if e.file_name().to_string_lossy().starts_with(STAGING_PREFIX)
            && e.file_type().is_ok_and(|t| t.is_dir())
        {
            let _ = std::fs::remove_dir_all(e.path());
        }
    }
}

/// Applies a staged update (the app is quitting): the platform's step, after the staged file is checked again where it
/// is a single file. When it fails, the app as it was starts again.
fn apply(staged: &Path, update: &Update) {
    let result = if cfg!(target_os = "macos") {
        // The bundle's code signature is checked again inside the platform step.
        platform::apply(staged)
    } else {
        recheck(staged, update).and_then(|()| platform::apply(staged))
    };
    if let Err(e) = result {
        log::error!("dilla-desktop: the update could not be applied: {e}");
        platform::relaunch_current();
    }
}

// ---------------------------------------------------------------------------------------------------------------------
// The app's side: state, schedule, tray rows
// ---------------------------------------------------------------------------------------------------------------------

/// What the updater is doing (beta.5 `UpdateState`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Phase {
    Idle,
    Checking,
    Downloading,
    /// A newer version this copy does not install itself.
    Available {
        version: String,
        package_manager: bool,
    },
    /// Downloaded, checked and staged: "Restart to update".
    Ready {
        version: String,
    },
    /// The last check or download failed; `message` is a `desktop.update.error.*` key.
    Failed {
        message: &'static str,
    },
}

/// What the worker thread reports.
enum Event {
    Found(Update),
    Staged(PathBuf, Update),
    UpToDate,
    Failed(Error),
}

struct Updater {
    installed: Installed,
    phase: Phase,
    /// The staged update while [`Phase::Ready`].
    staged: Option<(PathBuf, Update)>,
    /// The check in progress was asked for ("Check for updates"): its result is told.
    manual: bool,
    /// The version the last check found (kept while it downloads).
    found: Option<String>,
    /// A check (or its download) is running.
    running: bool,
    _schedule: Option<Task<()>>,
}

impl Global for Updater {}

/// Puts the updater in place: its tray rows, the apply step at quit, and (a release build) the checks at start and
/// every [`CHECK_EVERY`].
pub fn install(cx: &mut App) {
    let installed = installed();
    if let Installed::SelfUpdating { staging } = &installed {
        let staging = staging.clone();
        std::thread::spawn(move || sweep(&staging));
    }
    cx.set_global(Updater {
        installed,
        phase: Phase::Idle,
        staged: None,
        manual: false,
        found: None,
        running: false,
        _schedule: None,
    });
    NativeShell::add_tray_section(
        TraySection::new(TraySlot::Update, |cx| {
            cx.try_global::<Updater>()
                .map_or_else(Vec::new, |u| rows(&u.phase))
        }),
        cx,
    );
    cx.on_app_quit(|cx| {
        if NativeShell::quit_reason(cx) == Some(QuitReason::UpdateRestart)
            && let Some((staged, update)) =
                cx.try_global::<Updater>().and_then(|u| u.staged.clone())
        {
            apply(&staged, &update);
        }
        async {}
    })
    .detach();
    if !cfg!(debug_assertions) {
        let schedule = cx.spawn(async move |cx| {
            loop {
                cx.update(|cx| start_check(false, cx));
                cx.background_executor().timer(CHECK_EVERY).await;
            }
        });
        cx.global_mut::<Updater>()._schedule = Some(schedule);
    }
}

/// The tray rows for `phase` (beta.5 `refreshMenu`).
pub fn rows(phase: &Phase) -> Vec<TrayItem> {
    let busy = matches!(phase, Phase::Checking | Phase::Downloading);
    let check = match phase {
        Phase::Checking => t("desktop.update.checking"),
        Phase::Failed { .. } => t("desktop.update.retry"),
        _ => t("desktop.update.check"),
    };
    let mut rows = vec![TrayItem::Action {
        label: check.into(),
        enabled: !busy,
        run: Rc::new(|cx| start_check(true, cx)),
    }];
    match phase {
        Phase::Available {
            version,
            package_manager,
        } => {
            let (message, action) = if *package_manager {
                ("desktop.update.available", "desktop.update.packageManager")
            } else {
                (
                    "desktop.update.availableDownload",
                    "desktop.update.releasePage",
                )
            };
            let page = format!("{RELEASE_PAGE}{version}");
            rows.push(TrayItem::Action {
                label: tf(message, &[("version", version)]).into(),
                enabled: false,
                run: Rc::new(|_| {}),
            });
            rows.push(TrayItem::action(t(action), move |cx| cx.open_url(&page)));
        }
        Phase::Ready { .. } => rows.push(TrayItem::action(t("desktop.update.restart"), |cx| {
            NativeShell::request_quit_for(QuitReason::UpdateRestart, cx);
        })),
        _ => {}
    }
    rows
}

fn set_phase(phase: Phase, cx: &mut App) {
    let failed = matches!(phase, Phase::Failed { .. });
    cx.global_mut::<Updater>().phase = phase;
    crate::native::tray::set_update_failed(failed, cx);
    NativeShell::refresh_tray(cx);
}

fn notify(title: &str, body: &str, cx: &mut App) {
    cx.show_system_notification(SystemNotification {
        tag: SharedString::new_static("dilla-update"),
        title: SharedString::from(title.to_owned()),
        body: SharedString::from(body.to_owned()),
        actions: Vec::new(),
    });
}

/// Starts a check (and, for a copy that replaces itself, the download) unless one is running. `manual`: the person
/// asked ("Check for updates"), so the result is told.
pub fn start_check(manual: bool, cx: &mut App) {
    let Some(u) = cx.try_global::<Updater>() else {
        return;
    };
    if u.running || NativeShell::is_quitting(cx) {
        return;
    }
    let installed = u.installed.clone();
    let staged_version = u.staged.as_ref().map(|(_, s)| s.version.clone());
    let previous = u.phase.clone();
    let u = cx.global_mut::<Updater>();
    u.manual = manual;
    u.running = true;
    // A ready update stays offered while the next check runs.
    if !matches!(previous, Phase::Ready { .. }) {
        set_phase(Phase::Checking, cx);
    }
    let (tx, mut rx) = mpsc::unbounded_channel::<Event>();
    let spawned = std::thread::Builder::new()
        .name("dilla-updater".into())
        .spawn(move || work(installed, staged_version, tx));
    if spawned.is_err() {
        cx.global_mut::<Updater>().running = false;
        set_phase(
            Phase::Failed {
                message: "desktop.update.error.install",
            },
            cx,
        );
        return;
    }
    cx.spawn(async move |cx| {
        while let Some(event) = rx.recv().await {
            cx.update(|cx| on_event(event, cx));
        }
    })
    .detach();
}

/// The worker thread: check, then (a copy that replaces itself, a version not staged yet) download and prepare.
fn work(installed: Installed, staged: Option<Version>, tx: mpsc::UnboundedSender<Event>) {
    let rt = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(e) => {
            let _ = tx.send(Event::Failed(Error::Io(e)));
            return;
        }
    };
    let last = rt.block_on(async {
        let update = match check(Timestamp::now()).await {
            Err(e) => return Event::Failed(e),
            Ok(None) => return Event::UpToDate,
            Ok(Some(update)) => update,
        };
        let _ = tx.send(Event::Found(update.clone()));
        let Installed::SelfUpdating { staging } = installed else {
            return Event::UpToDate;
        };
        if staged.as_ref() == Some(&update.version) {
            return Event::UpToDate;
        }
        match download_into(&update, &staging).await {
            Ok(path) => match platform::prepare(&path) {
                Ok(prepared) => Event::Staged(prepared, update),
                Err(e) => {
                    remove_staging(&path);
                    Event::Failed(e)
                }
            },
            Err(e) => Event::Failed(e),
        }
    });
    // `UpToDate` after `Found` only ends the run; the UI keeps what `Found` set.
    let _ = tx.send(last);
}

/// The staging directory of a staged path (the asset's or the unpacked bundle's), found by its name.
fn remove_staging(path: &Path) {
    if let Some(dir) = path.ancestors().find(|p| {
        p.file_name()
            .is_some_and(|n| n.to_string_lossy().starts_with(STAGING_PREFIX))
    }) {
        let _ = std::fs::remove_dir_all(dir);
    }
}

fn on_event(event: Event, cx: &mut App) {
    if !matches!(event, Event::Found(_)) {
        // Every other event ends the run.
        cx.global_mut::<Updater>().running = false;
    }
    let u = cx.global::<Updater>();
    let manual = u.manual;
    let installed = u.installed.clone();
    let phase = u.phase.clone();
    let staged_version = u.staged.as_ref().map(|(_, s)| s.version.clone());
    match event {
        Event::Found(update) => {
            let version = update.version.to_string();
            cx.global_mut::<Updater>().found = Some(version.clone());
            match installed {
                Installed::SelfUpdating { .. } => {
                    if staged_version.as_ref() != Some(&update.version) {
                        set_phase(Phase::Downloading, cx);
                    }
                }
                Installed::PackageManager | Installed::Manual => set_phase(
                    Phase::Available {
                        version,
                        package_manager: installed == Installed::PackageManager,
                    },
                    cx,
                ),
            }
        }
        Event::Staged(path, update) => {
            let version = update.version.to_string();
            if let Some((old, _)) = cx.global_mut::<Updater>().staged.replace((path, update)) {
                remove_staging(&old);
            }
            set_phase(Phase::Ready { version }, cx);
        }
        Event::UpToDate => match phase {
            Phase::Checking => {
                set_phase(Phase::Idle, cx);
                if manual {
                    notify(t("desktop.update.upToDate"), "", cx);
                }
            }
            // A staged update (or an available one) stays offered.
            _ => NativeShell::refresh_tray(cx),
        },
        Event::Failed(e) => {
            log::warn!("dilla-desktop: update: {e}");
            if let (Error::NotWritable(_), Phase::Downloading) = (&e, &phase) {
                // This copy sits where this user cannot write: offer the release page instead.
                if let Some(version) = cx.global::<Updater>().found.clone() {
                    set_phase(
                        Phase::Available {
                            version,
                            package_manager: false,
                        },
                        cx,
                    );
                    return;
                }
            }
            if let Some(version) = staged_version {
                // A failed later check leaves the staged update offered.
                set_phase(
                    Phase::Ready {
                        version: version.to_string(),
                    },
                    cx,
                );
            } else {
                set_phase(
                    Phase::Failed {
                        message: e.message_key(),
                    },
                    cx,
                );
            }
            if manual {
                notify(t("desktop.update.failedTitle"), t(e.message_key()), cx);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn labels(phase: &Phase) -> Vec<(String, bool)> {
        rows(phase)
            .into_iter()
            .map(|r| match r {
                TrayItem::Action { label, enabled, .. } => (label.to_string(), enabled),
                other => panic!("{other:?}"),
            })
            .collect()
    }

    #[test]
    fn the_tray_rows_follow_beta5() {
        assert_eq!(labels(&Phase::Idle), [("Check for updates".into(), true)]);
        assert_eq!(
            labels(&Phase::Checking),
            [("Checking for updates…".into(), false)]
        );
        assert_eq!(
            labels(&Phase::Downloading),
            [("Check for updates".into(), false)]
        );
        assert_eq!(
            labels(&Phase::Failed {
                message: "desktop.update.error.network"
            }),
            [("Update failed — retry".into(), true)]
        );
        assert_eq!(
            labels(&Phase::Available {
                version: "0.2.0-alpha.2".into(),
                package_manager: true
            }),
            [
                ("Check for updates".into(), true),
                (
                    "Dilla 0.2.0-alpha.2 is available. Update using your package manager.".into(),
                    false
                ),
                ("Update using your package manager".into(), true),
            ]
        );
        assert_eq!(
            labels(&Phase::Ready {
                version: "0.2.0-alpha.2".into()
            }),
            [
                ("Check for updates".into(), true),
                ("Restart to update".into(), true)
            ]
        );
    }

    #[test]
    fn every_error_has_a_message() {
        for e in [
            Error::Network("x".into()),
            Error::Feed(FeedError::Signature),
            Error::Download(DownloadError::Hash),
            Error::Apply("x".into()),
        ] {
            assert!(crate::strings::try_t(e.message_key()).is_some(), "{e}");
        }
    }

    #[test]
    fn the_running_version_is_the_crate_version() {
        assert_eq!(current_version().to_string(), env!("CARGO_PKG_VERSION"));
        assert!(
            FEED_URL.starts_with("https://")
                && FEED_URL.ends_with("/gpui-alpha/desktop-gpui-alpha.json")
        );
    }

    #[test]
    fn staged_files_are_rechecked_and_left_over_staging_is_swept() {
        use sha2::{Digest as _, Sha512};
        let parent = crate::native::links::tests::temp_root("updater-staging");
        let dir = staging_dir(&parent).unwrap();
        let path = dir.join("dilla-0.2.0-alpha.2-linux-x64.AppImage");
        std::fs::write(&path, b"hello").unwrap();
        let update = Update {
            version: Version::parse("0.2.0-alpha.2").unwrap(),
            asset: feed::Asset {
                name: "dilla-0.2.0-alpha.2-linux-x64.AppImage".into(),
                size: 5,
                sha512: Sha512::digest(b"hello").into(),
            },
        };
        recheck(&path, &update).unwrap();
        std::fs::write(&path, b"jello").unwrap();
        assert!(matches!(
            recheck(&path, &update),
            Err(Error::Download(DownloadError::Hash))
        ));
        std::fs::write(&path, b"hello!").unwrap();
        assert!(matches!(
            recheck(&path, &update),
            Err(Error::Download(DownloadError::Size))
        ));
        let other = parent.join("keep-me");
        std::fs::create_dir(&other).unwrap();
        sweep(&parent);
        assert!(!dir.exists());
        assert!(other.exists());
    }
}
