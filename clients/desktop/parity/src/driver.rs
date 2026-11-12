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
    let text = String::from_utf8(output.stdout).expect("utf-8 metadata");
    let key = "\"target_directory\":\"";
    let start = text.find(key).expect("target_directory in metadata") + key.len();
    let end = start + text[start..].find('"').expect("end of target_directory");
    PathBuf::from(text[start..end].replace("\\\\", "\\"))
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
        let mut child = Command::new(automation_binary())
            .arg("--automation")
            .arg("--instance")
            .arg(instance_fixture)
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
        };
        let hello = driver.read_line();
        assert!(hello.starts_with("ready "), "unexpected greeting: {hello}");
        driver
    }

    fn read_line(&mut self) -> String {
        match self.lines.recv_timeout(REPLY_TIMEOUT) {
            Ok(line) => line.trim_end().to_owned(),
            Err(RecvTimeoutError::Timeout) => {
                let _ = self.child.kill();
                panic!("the automation app did not answer within {REPLY_TIMEOUT:?}; killed it");
            }
            Err(RecvTimeoutError::Disconnected) => panic!("the automation app exited"),
        }
    }

    fn request(&mut self, command: &str) -> String {
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
