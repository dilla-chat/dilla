//! The app's logger: every `log::warn!` / `log::error!` (and, for dilla's own crates, `log::info!`) goes to stderr and
//! to a file in the OS's log location, so a Dilla.app launched through `open` (stderr lost) still leaves a trace.
//!
//! - Filter: `RUST_LOG` when set (`level`, `target=level`, comma lists; a bare level sets the default, the longest
//!   matching target prefix wins), else [`DEFAULT_FILTER`]: warn for everything, info for crates named `dilla*`.
//! - File: [`log_path`] per OS; rotated to `<name>.1` past [`ROTATE_BYTES`] (one old file kept). When the file can't be
//!   opened the logger keeps stderr alone.
//! - Lines: `<RFC 3339 local time> <LEVEL> <target>: <message>`.
//!
//! PRIVACY: dilla is end-to-end encrypted. A log call never carries message text, keys, tokens, passwords or recovery
//! codes: log an id, a kind or a status instead.

use std::fs::{File, OpenOptions};
use std::io::{BufWriter, Write};
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use log::{LevelFilter, Log, Metadata, Record};

/// The filter when `RUST_LOG` is unset or empty.
pub const DEFAULT_FILTER: &str = "warn,dilla=info";
/// The file's size at which it is moved to `<name>.1` and a new one started.
pub const ROTATE_BYTES: u64 = 5 * 1024 * 1024;
/// The log file's name in [`log_dir`].
pub const FILE_NAME: &str = "dilla-desktop.log";

/// A parsed filter: the default level and `(target prefix, level)` directives, longest prefix first.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Filter {
    default: LevelFilter,
    directives: Vec<(String, LevelFilter)>,
}

impl Filter {
    /// Parses `spec` (`RUST_LOG` syntax: comma-separated `level` or `target=level`; `target` alone means `trace` for
    /// it). Unknown levels and empty parts are skipped. With no bare level the default is `error`, as env_logger.
    pub fn parse(spec: &str) -> Filter {
        let mut default = LevelFilter::Error;
        let mut directives: Vec<(String, LevelFilter)> = Vec::new();
        for part in spec.split(',').map(str::trim).filter(|p| !p.is_empty()) {
            match part.split_once('=') {
                Some((target, level)) => {
                    let target = target.trim();
                    if let (false, Ok(level)) = (target.is_empty(), level.trim().parse()) {
                        directives.retain(|(t, _)| t != target);
                        directives.push((target.to_owned(), level));
                    }
                }
                None => {
                    if let Ok(level) = part.parse::<LevelFilter>() {
                        default = level;
                    } else {
                        directives.retain(|(t, _)| t != part);
                        directives.push((part.to_owned(), LevelFilter::Trace));
                    }
                }
            }
        }
        directives.sort_by_key(|d| std::cmp::Reverse(d.0.len()));
        Filter {
            default,
            directives,
        }
    }

    /// `RUST_LOG` when it is set and not blank, else [`DEFAULT_FILTER`].
    pub fn from_env_value(value: Option<&str>) -> Filter {
        match value.map(str::trim).filter(|v| !v.is_empty()) {
            Some(spec) => Filter::parse(spec),
            None => Filter::parse(DEFAULT_FILTER),
        }
    }

    /// The level that applies to `target`: the longest directive that is a prefix of it, else the default.
    pub fn level_for(&self, target: &str) -> LevelFilter {
        self.directives
            .iter()
            .find(|(prefix, _)| target.starts_with(prefix.as_str()))
            .map_or(self.default, |(_, level)| *level)
    }

    /// The most verbose level any target can reach (`log::set_max_level`).
    pub fn max_level(&self) -> LevelFilter {
        self.directives
            .iter()
            .map(|(_, l)| *l)
            .fold(self.default, Ord::max)
    }
}

/// The OS a log path is chosen for (a parameter so every branch is tested on any host).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Os {
    Macos,
    Linux,
    Windows,
}

impl Os {
    /// The OS this build runs on (the BSDs count as Linux: they follow XDG).
    pub fn current() -> Os {
        if cfg!(target_os = "macos") {
            Os::Macos
        } else if cfg!(target_os = "windows") {
            Os::Windows
        } else {
            Os::Linux
        }
    }
}

/// The log directory, from the environment `var` (`HOME`, `XDG_STATE_HOME`, `LOCALAPPDATA`):
/// macOS `~/Library/Logs/Dilla`, Linux `$XDG_STATE_HOME/dilla` (else `~/.local/state/dilla`), Windows
/// `%LOCALAPPDATA%\Dilla\logs`. `None` when the variable it needs is missing or not absolute.
pub fn log_dir(os: Os, var: impl Fn(&str) -> Option<String>) -> Option<PathBuf> {
    // Absolute by the target OS's rules, checked on the text so every branch tests the same on any host.
    let absolute = |v: &str| match os {
        Os::Windows => {
            v.starts_with(['\\', '/']) || v.get(1..3).is_some_and(|s| s == ":\\" || s == ":/")
        }
        Os::Macos | Os::Linux => v.starts_with('/'),
    };
    let abs = |name: &str| var(name).filter(|v| absolute(v)).map(PathBuf::from);
    match os {
        Os::Macos => abs("HOME").map(|h| h.join("Library").join("Logs").join("Dilla")),
        Os::Linux => abs("XDG_STATE_HOME")
            .or_else(|| abs("HOME").map(|h| h.join(".local").join("state")))
            .map(|s| s.join("dilla")),
        Os::Windows => abs("LOCALAPPDATA").map(|l| l.join("Dilla").join("logs")),
    }
}

/// [`log_dir`] joined with [`FILE_NAME`].
pub fn log_path(os: Os, var: impl Fn(&str) -> Option<String>) -> Option<PathBuf> {
    log_dir(os, var).map(|d| d.join(FILE_NAME))
}

/// The log file of this process, for the docs and a "where are my logs" answer.
pub fn current_log_path() -> Option<PathBuf> {
    log_path(Os::current(), |name| std::env::var(name).ok())
}

/// One log line (without the newline).
pub fn format_line(time: &str, record: &Record<'_>) -> String {
    format!(
        "{time} {:<5} {}: {}",
        record.level(),
        record.target(),
        record.args()
    )
}

struct FileSink {
    path: PathBuf,
    writer: BufWriter<File>,
    written: u64,
}

impl FileSink {
    fn open(path: &Path) -> std::io::Result<FileSink> {
        if let Some(dir) = path.parent() {
            std::fs::create_dir_all(dir)?;
        }
        let file = OpenOptions::new().create(true).append(true).open(path)?;
        let written = file.metadata().map(|m| m.len()).unwrap_or(0);
        Ok(FileSink {
            path: path.to_owned(),
            writer: BufWriter::new(file),
            written,
        })
    }

    fn write_line(&mut self, line: &str) {
        if self.written >= ROTATE_BYTES {
            self.rotate();
        }
        if self.writer.write_all(line.as_bytes()).is_ok() && self.writer.write_all(b"\n").is_ok() {
            self.written += line.len() as u64 + 1;
        }
    }

    /// Moves the file to `<name>.1` (replacing the old one) and starts a new one; on failure keeps appending.
    fn rotate(&mut self) {
        let _ = self.writer.flush();
        let mut old = self.path.clone().into_os_string();
        old.push(".1");
        let _ = std::fs::remove_file(&old);
        if std::fs::rename(&self.path, &old).is_err() {
            return;
        }
        if let Ok(file) = OpenOptions::new()
            .create(true)
            .append(true)
            .open(&self.path)
        {
            self.writer = BufWriter::new(file);
            self.written = 0;
        }
    }
}

/// The installed logger: the filter, stderr, and the file behind one mutex.
pub struct Logger {
    filter: Filter,
    file: Option<Mutex<FileSink>>,
}

impl Logger {
    /// A logger with `filter` writing to stderr and, when it opens, the file at `path`.
    pub fn new(filter: Filter, path: Option<&Path>) -> Logger {
        let file = path.and_then(|p| match FileSink::open(p) {
            Ok(sink) => Some(Mutex::new(sink)),
            Err(e) => {
                eprintln!(
                    "dilla-desktop: the log file {} did not open ({e}); logging to stderr only",
                    p.display()
                );
                None
            }
        });
        Logger { filter, file }
    }
}

impl Log for Logger {
    fn enabled(&self, metadata: &Metadata<'_>) -> bool {
        metadata.level() <= self.filter.level_for(metadata.target())
    }

    fn log(&self, record: &Record<'_>) {
        if !self.enabled(record.metadata()) {
            return;
        }
        let time = chrono::Local::now().format("%Y-%m-%dT%H:%M:%S%.3f%:z");
        let line = format_line(&time.to_string(), record);
        let _ = writeln!(std::io::stderr().lock(), "{line}");
        if let Some(file) = &self.file {
            let mut sink = file.lock().unwrap_or_else(|p| p.into_inner());
            sink.write_line(&line);
            // A warning or an error reaches the disk at once: the app may be about to fail.
            if record.level() <= log::Level::Warn {
                let _ = sink.writer.flush();
            }
        }
    }

    fn flush(&self) {
        if let Some(file) = &self.file {
            let mut sink = file.lock().unwrap_or_else(|p| p.into_inner());
            let _ = sink.writer.flush();
        }
    }
}

/// Installs the logger (once; a second call is a no-op) with `RUST_LOG` or [`DEFAULT_FILTER`], and logs where the
/// file is. Returns the file's path when it is in use.
pub fn install() -> Option<PathBuf> {
    let filter = Filter::from_env_value(std::env::var("RUST_LOG").ok().as_deref());
    let max = filter.max_level();
    let path = current_log_path();
    let logger = Logger::new(filter, path.as_deref());
    let used = logger.file.is_some().then_some(path).flatten();
    if log::set_boxed_logger(Box::new(logger)).is_ok() {
        log::set_max_level(max);
        log::info!(
            "Dilla {} started; log file {}",
            env!("CARGO_PKG_VERSION"),
            used.as_deref()
                .map_or_else(|| "none".to_owned(), |p| p.display().to_string())
        );
    }
    used
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    fn env(pairs: &[(&str, &str)]) -> impl Fn(&str) -> Option<String> {
        let map: HashMap<String, String> = pairs
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect();
        move |name| map.get(name).cloned()
    }

    #[test]
    fn default_filter_is_warn_and_info_for_dilla_crates() {
        let f = Filter::from_env_value(None);
        assert_eq!(f, Filter::from_env_value(Some("  ")));
        assert_eq!(
            f.level_for("dilla_desktop::screens::voice"),
            LevelFilter::Info
        );
        assert_eq!(f.level_for("dilla_client::calls::join"), LevelFilter::Info);
        assert_eq!(f.level_for("dilla_media"), LevelFilter::Info);
        assert_eq!(f.level_for("livekit::room"), LevelFilter::Warn);
        assert_eq!(f.level_for("gpui"), LevelFilter::Warn);
        assert_eq!(f.max_level(), LevelFilter::Info);
    }

    #[test]
    fn rust_log_bare_level_sets_the_default() {
        let f = Filter::from_env_value(Some("debug"));
        assert_eq!(f.level_for("anything"), LevelFilter::Debug);
        assert_eq!(f.level_for("dilla_client"), LevelFilter::Debug);
        assert_eq!(Filter::parse("OFF").level_for("x"), LevelFilter::Off);
    }

    #[test]
    fn rust_log_target_lists_pick_the_longest_prefix() {
        let f = Filter::parse("info, livekit=debug ,dilla_client::calls=trace,dilla=warn");
        assert_eq!(f.level_for("gpui"), LevelFilter::Info);
        assert_eq!(f.level_for("livekit::rtc_engine"), LevelFilter::Debug);
        assert_eq!(f.level_for("dilla_client::calls::join"), LevelFilter::Trace);
        assert_eq!(f.level_for("dilla_client::sync"), LevelFilter::Warn);
        assert_eq!(f.max_level(), LevelFilter::Trace);
    }

    #[test]
    fn rust_log_without_a_bare_level_defaults_to_error_and_skips_junk() {
        let f = Filter::parse("livekit=debug,,=info,x=loud,dilla_media");
        assert_eq!(f.level_for("gpui"), LevelFilter::Error);
        assert_eq!(f.level_for("livekit"), LevelFilter::Debug);
        assert_eq!(f.level_for("x"), LevelFilter::Error);
        assert_eq!(f.level_for("dilla_media::audio"), LevelFilter::Trace);
        // A later directive for the same target replaces the earlier one.
        assert_eq!(
            Filter::parse("a=info,a=debug").level_for("a"),
            LevelFilter::Debug
        );
    }

    #[test]
    fn log_path_per_os() {
        let home = env(&[("HOME", "/Users/jo")]);
        assert_eq!(
            log_path(Os::Macos, &home),
            Some(PathBuf::from(
                "/Users/jo/Library/Logs/Dilla/dilla-desktop.log"
            ))
        );
        assert_eq!(
            log_path(Os::Linux, env(&[("HOME", "/home/jo")])),
            Some(PathBuf::from(
                "/home/jo/.local/state/dilla/dilla-desktop.log"
            ))
        );
        assert_eq!(
            log_path(
                Os::Linux,
                env(&[("HOME", "/home/jo"), ("XDG_STATE_HOME", "/st")])
            ),
            Some(PathBuf::from("/st/dilla/dilla-desktop.log"))
        );
        // A relative XDG_STATE_HOME is invalid by the spec: fall back to HOME.
        assert_eq!(
            log_path(
                Os::Linux,
                env(&[("HOME", "/home/jo"), ("XDG_STATE_HOME", "rel")])
            ),
            Some(PathBuf::from(
                "/home/jo/.local/state/dilla/dilla-desktop.log"
            ))
        );
        let local = r"C:\Users\jo\AppData\Local";
        assert_eq!(
            log_dir(Os::Windows, env(&[("LOCALAPPDATA", local)])),
            Some(PathBuf::from(local).join("Dilla").join("logs"))
        );
        assert_eq!(
            log_dir(Os::Windows, env(&[("LOCALAPPDATA", "AppData")])),
            None
        );
        assert_eq!(log_path(Os::Macos, env(&[])), None);
        assert_eq!(log_path(Os::Linux, env(&[("HOME", "")])), None);
        assert_eq!(log_path(Os::Windows, env(&[("HOME", "/h")])), None);
    }

    #[test]
    fn lines_carry_time_level_and_target() {
        let args = format_args!("joined");
        let record = Record::builder()
            .args(args)
            .level(log::Level::Warn)
            .target("dilla_client::calls")
            .build();
        assert_eq!(
            format_line("2026-11-16T10:00:00.000+01:00", &record),
            "2026-11-16T10:00:00.000+01:00 WARN  dilla_client::calls: joined"
        );
    }

    #[test]
    fn the_file_rotates_and_keeps_one_old_copy() {
        let dir = std::env::temp_dir().join(format!("dilla-log-test-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        let path = dir.join("sub").join(FILE_NAME);
        let mut sink = FileSink::open(&path).unwrap();
        sink.write_line("first");
        sink.written = ROTATE_BYTES;
        sink.write_line("second");
        sink.writer.flush().unwrap();
        let mut old = path.clone().into_os_string();
        old.push(".1");
        assert_eq!(std::fs::read_to_string(&old).unwrap(), "first\n");
        assert_eq!(std::fs::read_to_string(&path).unwrap(), "second\n");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn an_unopenable_file_leaves_stderr_only() {
        let blocker = std::env::temp_dir().join(format!("dilla-log-block-{}", std::process::id()));
        std::fs::write(&blocker, b"").unwrap();
        let logger = Logger::new(Filter::parse("info"), Some(&blocker.join(FILE_NAME)));
        assert!(logger.file.is_none());
        let _ = std::fs::remove_file(&blocker);
    }
}
