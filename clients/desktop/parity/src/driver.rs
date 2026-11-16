use std::io::{BufRead, BufReader, Write};
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::OnceLock;
use std::sync::mpsc::{Receiver, RecvTimeoutError, channel};
use std::time::Duration;

use image::RgbaImage;

/// The painted bounds of an element, in logical pixels from the window's top-left corner.
#[derive(Clone, Debug, PartialEq)]
pub struct ElementInfo {
    pub id: String,
    pub x: f32,
    pub y: f32,
    pub width: f32,
    pub height: f32,
}

/// A running automation build of `dilla-desktop` (see `dilla-desktop/src/automation.rs`).
pub struct Driver {
    child: Child,
    stdin: ChildStdin,
    lines: Receiver<String>,
    shots: tempfile::TempDir,
    counter: u32,
    timeout: Duration,
}

/// Why a request got no reply.
#[derive(Debug, PartialEq, Eq)]
pub enum NoReply {
    /// The app did not answer in time; the driver killed it.
    Timeout(Duration),
    /// The app exited.
    Exited,
}

fn workspace_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../..")
        .canonicalize()
        .expect("workspace root")
}

/// How long the app may take to answer one request (GPU start-up included) before the driver gives up.
const REPLY_TIMEOUT: Duration = Duration::from_secs(60);

/// The cargo target directory of this workspace, as `cargo metadata` reports it (honours
/// `CARGO_TARGET_DIR` and `build.target-dir`).
pub fn target_dir() -> PathBuf {
    let cargo = std::env::var_os("CARGO").unwrap_or_else(|| "cargo".into());
    let output = Command::new(cargo)
        .args(["metadata", "--format-version", "1", "--no-deps"])
        .current_dir(workspace_root())
        .output()
        .expect("cargo metadata runs");
    assert!(output.status.success(), "cargo metadata failed");
    let metadata: serde_json::Value =
        serde_json::from_slice(&output.stdout).expect("cargo metadata is JSON");
    PathBuf::from(
        metadata["target_directory"]
            .as_str()
            .expect("target_directory in metadata"),
    )
}

/// Builds (once per process) and returns the automation binary. `DILLA_AUTOMATION_BIN` overrides it.
fn automation_binary() -> &'static Path {
    static BIN: OnceLock<PathBuf> = OnceLock::new();
    BIN.get_or_init(|| {
        if let Some(path) = std::env::var_os("DILLA_AUTOMATION_BIN") {
            return PathBuf::from(path);
        }
        let cargo = std::env::var_os("CARGO").unwrap_or_else(|| "cargo".into());
        let status = Command::new(cargo)
            .args(["build", "-p", "dilla-desktop", "--features", "automation"])
            .current_dir(workspace_root())
            .status()
            .expect("cargo runs");
        assert!(
            status.success(),
            "building dilla-desktop with automation failed"
        );
        let name = format!("dilla-desktop{}", std::env::consts::EXE_SUFFIX);
        target_dir().join("debug").join(name)
    })
}

impl Driver {
    /// Starts the automation build against the instance fixture directory and waits until it is ready.
    pub fn launch(instance_fixture: &Path) -> Self {
        Self::launch_with_env(instance_fixture, &[])
    }

    /// [`Driver::launch`] with extra environment variables for the app (test controls such as the fallback font).
    pub fn launch_with_env(instance_fixture: &Path, env: &[(&str, &str)]) -> Self {
        Self::spawn(&["--instance".as_ref(), instance_fixture.as_os_str()], env)
    }

    /// Starts the automation build on a screen fixture (`fixtures/<area>/<state>.json`); `shot` is the reference's
    /// sidecar (`<state>.shot.json`), whose ids and time shift the web shooter recorded.
    pub fn launch_fixture(fixture: &Path, shot: Option<&Path>, env: &[(&str, &str)]) -> Self {
        Self::try_launch_fixture(fixture, shot, env).unwrap_or_else(|why| panic!("{why}"))
    }

    /// [`Driver::launch_fixture`] that answers why the app refused the fixture (a route it cannot draw, a bad sidecar)
    /// instead of panicking.
    pub fn try_launch_fixture(
        fixture: &Path,
        shot: Option<&Path>,
        env: &[(&str, &str)],
    ) -> Result<Self, String> {
        let mut args: Vec<&std::ffi::OsStr> = vec!["--instance".as_ref(), fixture.as_os_str()];
        if let Some(shot) = shot {
            args.push("--shot".as_ref());
            args.push(shot.as_os_str());
        }
        Self::try_spawn(&args, env)
    }

    /// Starts the automation build's own harness view (two focusable fields, a hover box and a scroll box), which
    /// proves the interface itself without any screen of the app.
    pub fn launch_harness() -> Self {
        Self::spawn(&["--harness".as_ref()], &[])
    }

    fn spawn(args: &[&std::ffi::OsStr], env: &[(&str, &str)]) -> Self {
        Self::try_spawn(args, env).unwrap_or_else(|why| panic!("{why}"))
    }

    fn try_spawn(args: &[&std::ffi::OsStr], env: &[(&str, &str)]) -> Result<Self, String> {
        let mut child = Command::new(automation_binary())
            .arg("--automation")
            .args(args)
            .envs(env.iter().copied())
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .expect("the automation binary starts");
        let stdin = child.stdin.take().expect("piped stdin");
        let stdout = BufReader::new(child.stdout.take().expect("piped stdout"));
        let (sender, lines) = channel();
        std::thread::spawn(move || {
            for line in stdout.lines().map_while(Result::ok) {
                if sender.send(line).is_err() {
                    break;
                }
            }
        });
        let mut driver = Self {
            child,
            stdin,
            lines,
            shots: tempfile::tempdir().expect("temp dir"),
            counter: 0,
            timeout: REPLY_TIMEOUT,
        };
        let hello = driver.read_line();
        match hello.strip_prefix("error ") {
            Some(why) => Err(why.to_owned()),
            None if hello.starts_with("ready ") => Ok(driver),
            None => Err(format!("unexpected greeting: {hello}")),
        }
    }

    /// How long one request may take before the driver gives up and kills the app (60 s by default).
    pub fn set_reply_timeout(&mut self, timeout: Duration) {
        self.timeout = timeout;
    }

    /// Sends `command` and waits at most `timeout` for its one-line reply. On a timeout the app is killed (it is in an
    /// unknown state) and [`NoReply::Timeout`] answers.
    pub fn request_within(&mut self, command: &str, timeout: Duration) -> Result<String, NoReply> {
        let sent = writeln!(self.stdin, "{command}").and_then(|()| self.stdin.flush());
        if sent.is_err() {
            return Err(NoReply::Exited);
        }
        match self.lines.recv_timeout(timeout) {
            Ok(line) => Ok(line.trim_end().to_owned()),
            Err(RecvTimeoutError::Timeout) => {
                let _ = self.child.kill();
                let _ = self.child.wait();
                Err(NoReply::Timeout(timeout))
            }
            Err(RecvTimeoutError::Disconnected) => Err(NoReply::Exited),
        }
    }

    /// Whether the app process has ended (it was killed after a timeout, or quit).
    pub fn has_exited(&mut self) -> bool {
        matches!(self.child.try_wait(), Ok(Some(_)))
    }

    fn read_line(&mut self) -> String {
        match self.lines.recv_timeout(self.timeout) {
            Ok(line) => line.trim_end().to_owned(),
            Err(RecvTimeoutError::Timeout) => {
                let _ = self.child.kill();
                panic!(
                    "the automation app did not answer within {:?}; killed it",
                    self.timeout
                );
            }
            Err(RecvTimeoutError::Disconnected) => panic!("the automation app exited"),
        }
    }

    /// One request, one reply line. Panics if the app does not answer within the reply timeout.
    pub fn request(&mut self, command: &str) -> String {
        writeln!(self.stdin, "{command}")
            .and_then(|()| self.stdin.flush())
            .expect("write to the app");
        self.read_line()
    }

    fn expect_ok(&mut self, command: &str) {
        let reply = self.request(command);
        assert_eq!(reply, "ok", "`{command}` failed: {reply}");
    }

    /// Clicks the centre of the element with GPUI element id `id`. Panics if there is no such element.
    pub fn click(&mut self, id: &str) {
        self.expect_ok(&format!("click {id}"));
    }

    /// Moves the pointer to the centre of the element with GPUI element id `id`.
    pub fn hover(&mut self, id: &str) {
        self.expect_ok(&format!("hover {id}"));
    }

    /// The id of the focused element (see `automation::focus_probe`; `-` when an unregistered element holds the focus), or
    /// `None` when nothing holds the focus.
    pub fn focused(&mut self) -> Option<String> {
        let reply = self.request("focused");
        if reply == "none" {
            return None;
        }
        Some(
            reply
                .strip_prefix("ok ")
                .unwrap_or_else(|| panic!("`focused` failed: {reply}"))
                .to_owned(),
        )
    }

    /// A wheel step of `dy` logical px over the centre of the element `id`.
    pub fn scroll(&mut self, id: &str, dy: f32) {
        self.expect_ok(&format!("scroll {id} {dy}"));
    }

    /// What the web shooter does before its frame: the pointer to the window's corner, off any content, and the focus
    /// cleared.
    pub fn rest(&mut self) {
        self.expect_ok("rest");
    }

    /// The pointer's position, logical px.
    pub fn pointer(&mut self) -> (f32, f32) {
        let reply = self.request("pointer");
        let n: Vec<f32> = reply
            .strip_prefix("ok ")
            .unwrap_or_else(|| panic!("`pointer` failed: {reply}"))
            .split(' ')
            .map(|n| n.parse().expect("a number"))
            .collect();
        (n[0], n[1])
    }

    /// Waits until nothing on screen changes (two identical frames in a row); answers how many frames it drew. Panics
    /// when the screen never settles (the app answers `err not idle` after 60 frames).
    pub fn wait_idle(&mut self) -> usize {
        let reply = self.request("wait-idle");
        reply
            .strip_prefix("ok ")
            .and_then(|n| n.parse().ok())
            .unwrap_or_else(|| panic!("`wait-idle` failed: {reply}"))
    }

    /// The fixture line the app drew its screen from (`fixture <name> <digest> <ids>`), `None` for a screen that is
    /// not a fixture.
    pub fn fixture_line(&mut self) -> Option<String> {
        let reply = self.request("fixture");
        if reply == "none" {
            return None;
        }
        Some(
            reply
                .strip_prefix("ok ")
                .unwrap_or_else(|| panic!("`fixture` failed: {reply}"))
                .to_owned(),
        )
    }

    /// The harness view's received events, oldest first (`field-a:enter`, `hover-box:enter`, ...).
    pub fn harness_log(&mut self) -> Vec<String> {
        let reply = self.request("log");
        reply
            .strip_prefix("ok")
            .unwrap_or_else(|| panic!("`log` failed: {reply}"))
            .trim()
            .split(';')
            .filter(|e| !e.is_empty())
            .map(str::to_owned)
            .collect()
    }

    /// Types `text` into whatever element holds the focus (no click: after [`Driver::press`] of `tab`, say).
    pub fn type_focused(&mut self, text: &str) {
        self.type_text("-", text);
    }

    /// Clicks the element to focus it, then types `text`.
    pub fn type_text(&mut self, id: &str, text: &str) {
        let hex: String = text.bytes().map(|b| format!("{b:02x}")).collect();
        self.expect_ok(&format!("type {id} {hex}"));
    }

    /// The painted bounds of the element with GPUI element id `id`, or `None` if it is not on screen.
    pub fn element(&mut self, id: &str) -> Option<ElementInfo> {
        let reply = self.request(&format!("element {id}"));
        if reply == "none" {
            return None;
        }
        let numbers: Vec<f32> = reply
            .strip_prefix("ok ")
            .unwrap_or_else(|| panic!("`element {id}` failed: {reply}"))
            .split(' ')
            .map(|n| n.parse().expect("a number"))
            .collect();
        let [x, y, width, height] = numbers[..] else {
            panic!("bad element reply: {reply}");
        };
        Some(ElementInfo {
            id: id.to_owned(),
            x,
            y,
            width,
            height,
        })
    }

    /// Sends one keystroke to the focused element: a named key or a chord (`enter`, `tab`, `shift-tab`, `escape`, `up`,
    /// `ctrl-k`, `ctrl-shift-m`, ...; the grammar is `spec::validate_chord`). A chord outside it fails here, before the
    /// app sees it.
    pub fn press(&mut self, key: &str) {
        dilla_client::testing::spec::validate_chord(key)
            .unwrap_or_else(|e| panic!("not a key chord: {e}"));
        self.expect_ok(&format!("key {key}"));
    }

    /// The window's accessibility tree, as a screen reader gets it ([`crate::a11y`]). The first call switches
    /// accessibility on, as an assistive technology connecting does.
    pub fn a11y_tree(&mut self) -> crate::a11y::Tree {
        let reply = self.request("a11y");
        let json = reply
            .strip_prefix("ok ")
            .unwrap_or_else(|| panic!("`a11y` failed: {reply}"));
        crate::a11y::Tree::parse(json).unwrap_or_else(|e| panic!("`a11y` sent bad JSON: {e}"))
    }

    /// Walks the tab order from where the focus is: presses Tab up to `max` times and returns, at each stop, the
    /// automation id of the focused element (`focused`) and the tree, until the focus comes back to the first stop.
    pub fn tab_walk(&mut self, max: usize) -> Vec<(Option<String>, crate::a11y::Tree)> {
        let mut stops = Vec::new();
        let mut first = None;
        for _ in 0..max {
            self.press("tab");
            let id = self.focused();
            let tree = self.a11y_tree();
            // A stop is known by its node and its automation id. One with neither (no node, an unregistered element)
            // could be any of several, so it is kept (the lint flags it) but never starts or ends the walk.
            let key = (tree.focus, id.clone());
            let ambiguous = Some(tree.focus) == tree.root && id.as_deref().is_none_or(|i| i == "-");
            if !ambiguous && first.as_ref() == Some(&key) {
                break;
            }
            if first.is_none() && !ambiguous {
                first = Some(key);
            }
            stops.push((id, tree));
        }
        stops
    }

    /// The line boxes of every text line on screen, in logical pixels: the input of the chrome gate's text mask
    /// ([`crate::TextMask::from_rects`] after scaling to device pixels).
    pub fn text_bounds(&mut self) -> Vec<ElementInfo> {
        let reply = self.request("text-bounds");
        let rest = reply
            .strip_prefix("ok")
            .unwrap_or_else(|| panic!("`text-bounds` failed: {reply}"))
            .trim();
        rest.split(';')
            .filter(|r| !r.is_empty())
            .map(|r| {
                let n: Vec<f32> = r.split(' ').map(|n| n.parse().expect("a number")).collect();
                let [x, y, width, height] = n[..] else {
                    panic!("bad text-bounds reply: {reply}");
                };
                ElementInfo {
                    id: String::from("text"),
                    x,
                    y,
                    width,
                    height,
                }
            })
            .collect()
    }

    /// Every tracked element of the latest frame (`elements`), sorted by id, logical pixels.
    pub fn elements(&mut self) -> Vec<ElementInfo> {
        let reply = self.request("elements");
        let rest = reply
            .strip_prefix("ok")
            .unwrap_or_else(|| panic!("`elements` failed: {reply}"))
            .trim();
        rest.split(';')
            .filter(|r| !r.is_empty())
            .map(|r| {
                let parts: Vec<&str> = r.split(' ').collect();
                let [id, x, y, width, height] = parts[..] else {
                    panic!("bad elements reply: {reply}");
                };
                let n = |v: &str| v.parse::<f32>().expect("a number");
                ElementInfo {
                    id: id.to_owned(),
                    x: n(x),
                    y: n(y),
                    width: n(width),
                    height: n(height),
                }
            })
            .collect()
    }

    /// Where macOS draws the workspace window's traffic lights (`traffic-lights`: the app's `MAC_TRAFFIC_LIGHTS`),
    /// logical pixels.
    pub fn traffic_lights(&mut self) -> ElementInfo {
        let reply = self.request("traffic-lights");
        let n: Vec<f32> = reply
            .strip_prefix("ok ")
            .unwrap_or_else(|| panic!("`traffic-lights` failed: {reply}"))
            .split(' ')
            .map(|v| v.parse().expect("a number"))
            .collect();
        let [x, y, width, height] = n[..] else {
            panic!("bad traffic-lights reply: {reply}");
        };
        ElementInfo {
            id: String::from("traffic-lights"),
            x,
            y,
            width,
            height,
        }
    }

    /// Waits until the element `id` is on screen (the app keeps working between requests), at most `timeout`.
    pub fn wait_for(&mut self, id: &str, timeout: Duration) -> ElementInfo {
        let deadline = std::time::Instant::now() + timeout;
        loop {
            if let Some(found) = self.element(id) {
                return found;
            }
            assert!(
                std::time::Instant::now() < deadline,
                "`{id}` did not appear within {timeout:?}"
            );
            std::thread::sleep(Duration::from_millis(100));
        }
    }

    /// The rendered window as device pixels (the headless platform renders at scale 2.0).
    pub fn screenshot(&mut self) -> RgbaImage {
        self.counter += 1;
        let path = self.shots.path().join(format!("shot-{}.png", self.counter));
        let reply = self.request(&format!("screenshot {}", path.display()));
        assert!(reply.starts_with("ok "), "screenshot failed: {reply}");
        image::open(&path)
            .expect("the screenshot decodes")
            .to_rgba8()
    }
}

impl Drop for Driver {
    fn drop(&mut self) {
        let _ = writeln!(self.stdin, "quit");
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}
