//! Non-secret preferences, as JSON objects on disk: one per instance (`<data_dir>/prefs.json`: drafts, layout, voice
//! settings, notification defaults) and one for the whole app (`<root>/desktop.json`: what the instance manager and the
//! window keep). Keys are dotted names owned by one area each (`layout.*` is the shell's; clients/desktop/OWNERSHIP.md).
//!
//! A write keeps every key it did not set, also keys this build does not know (a newer build's, another area's written
//! through another handle): it reads the file again, changes its one key and replaces the file whole (written to a
//! temporary file, then renamed). A missing or unreadable file reads as empty.
//!
//! Never a secret: tokens, keys and passwords live in the OS keychain and the encrypted store. A key that names one
//! (a word `token`, `key` or `secret`, see [`names_a_secret`]) panics in debug builds.

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use serde::Serialize;
use serde::de::DeserializeOwned;
use serde_json::{Map, Value};

/// Why a preference was not written.
#[derive(Debug, thiserror::Error)]
pub enum PrefsError {
    #[error("the preference could not be encoded: {0}")]
    Encode(String),
    #[error("the preferences file could not be written: {0}")]
    Io(String),
}

/// One preferences file. Cheap to clone; clones share the cache.
#[derive(Clone, Debug)]
pub struct Prefs {
    path: PathBuf,
    cache: Arc<Mutex<Map<String, Value>>>,
}

/// Serialises read-modify-write of every preferences file in this process.
static WRITE: Mutex<()> = Mutex::new(());

impl Prefs {
    /// The instance's preferences, `<data_dir>/prefs.json`.
    pub fn instance(data_dir: &Path) -> Prefs {
        Self::open(data_dir.join("prefs.json"))
    }

    /// The app-wide preferences, `<root>/desktop.json`.
    pub fn app(root: &Path) -> Prefs {
        Self::open(root.join("desktop.json"))
    }

    /// The preferences in `path`, read once now.
    pub fn open(path: PathBuf) -> Prefs {
        let cache = Arc::new(Mutex::new(read(&path)));
        Prefs { path, cache }
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    /// The value under `key`, or `None` when it is absent or not a `T`.
    pub fn get<T: DeserializeOwned>(&self, key: &str) -> Option<T> {
        let cache = self
            .cache
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        cache
            .get(key)
            .and_then(|v| serde_json::from_value(v.clone()).ok())
    }

    /// Writes `v` under `key`, keeping every other key in the file.
    pub fn set<T: Serialize>(&self, key: &str, v: &T) -> Result<(), PrefsError> {
        debug_assert!(
            !names_a_secret(key),
            "preference {key:?} names a secret: secrets go to the keychain, never to prefs"
        );
        let value = serde_json::to_value(v).map_err(|e| PrefsError::Encode(e.to_string()))?;
        self.update(|map| {
            map.insert(key.to_owned(), value);
        })
    }

    /// Removes `key`, keeping every other key in the file.
    pub fn remove(&self, key: &str) -> Result<(), PrefsError> {
        self.update(|map| {
            map.remove(key);
        })
    }

    fn update(&self, f: impl FnOnce(&mut Map<String, Value>)) -> Result<(), PrefsError> {
        let _write = WRITE
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let mut map = read(&self.path);
        f(&mut map);
        let bytes = serde_json::to_vec_pretty(&Value::Object(map.clone()))
            .map_err(|e| PrefsError::Encode(e.to_string()))?;
        if let Some(dir) = self.path.parent() {
            std::fs::create_dir_all(dir).map_err(|e| PrefsError::Io(e.to_string()))?;
        }
        let tmp = self.path.with_extension("json.tmp");
        std::fs::write(&tmp, bytes).map_err(|e| PrefsError::Io(e.to_string()))?;
        std::fs::rename(&tmp, &self.path).map_err(|e| PrefsError::Io(e.to_string()))?;
        *self
            .cache
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = map;
        Ok(())
    }
}

fn read(path: &Path) -> Map<String, Value> {
    std::fs::read(path)
        .ok()
        .and_then(|bytes| serde_json::from_slice::<Value>(&bytes).ok())
        .and_then(|v| match v {
            Value::Object(map) => Some(map),
            _ => None,
        })
        .unwrap_or_default()
}

/// Whether a preference key names a secret: one of its words (split at `.`, `_`, `-` and lower-to-upper case changes)
/// is `token`, `key` or `secret`. `gifs.apiKey`, `session_token` and `push.secret` do; `layout.sidebarWidth` and
/// `keyboard.hints` do not.
pub fn names_a_secret(key: &str) -> bool {
    let mut words = Vec::new();
    let mut word = String::new();
    let mut prev_lower = false;
    for c in key.chars() {
        if matches!(c, '.' | '_' | '-') {
            words.push(std::mem::take(&mut word));
            prev_lower = false;
            continue;
        }
        if c.is_uppercase() && prev_lower {
            words.push(std::mem::take(&mut word));
        }
        prev_lower = c.is_lowercase() || c.is_ascii_digit();
        word.extend(c.to_lowercase());
    }
    words.push(word);
    words
        .iter()
        .any(|w| matches!(w.as_str(), "token" | "key" | "secret"))
}

/// The shell's own preferences, the `layout.*` keys (Task 8): the two pane widths the person dragged to (CSS px; absent =
/// the default), the categories collapsed per server and the channels muted from their row's menu. A wrong-typed or
/// non-positive width reads as absent, as `readPaneWidths` does on the web.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct LayoutPrefs {
    pub sidebar: Option<f32>,
    pub aside: Option<f32>,
}

impl LayoutPrefs {
    pub const SIDEBAR: &'static str = "layout.sidebarWidth";
    pub const ASIDE: &'static str = "layout.asideWidth";
    pub const COLLAPSED: &'static str = "layout.collapsedCategories";
    pub const MUTED: &'static str = "layout.mutedChannels";

    /// The widths `prefs` keeps.
    pub fn read(prefs: &Prefs) -> Self {
        let width = |key| {
            prefs
                .get::<f64>(key)
                .filter(|w| w.is_finite() && *w > 0.0)
                .map(|w| w as f32)
        };
        Self {
            sidebar: width(Self::SIDEBAR),
            aside: width(Self::ASIDE),
        }
    }

    /// Keeps `width` under `key` (`None` forgets it, back to the default).
    fn write_width(prefs: &Prefs, key: &str, width: Option<f32>) -> Result<(), PrefsError> {
        match width {
            Some(w) => prefs.set(key, &f64::from(w.round())),
            None => prefs.remove(key),
        }
    }

    pub fn write_sidebar(prefs: &Prefs, width: Option<f32>) -> Result<(), PrefsError> {
        Self::write_width(prefs, Self::SIDEBAR, width)
    }

    pub fn write_aside(prefs: &Prefs, width: Option<f32>) -> Result<(), PrefsError> {
        Self::write_width(prefs, Self::ASIDE, width)
    }

    /// The hex ids (of categories, of channels) kept in the list under `key`.
    pub fn ids(prefs: &Prefs, key: &str) -> Vec<String> {
        prefs.get::<Vec<String>>(key).unwrap_or_default()
    }

    /// Adds `id` to the list under `key`, or takes it out when it is there (`on` says which); the list stays sorted.
    pub fn set_id(prefs: &Prefs, key: &str, id: &str, on: bool) -> Result<(), PrefsError> {
        let mut ids = Self::ids(prefs, key);
        ids.retain(|i| i != id);
        if on {
            ids.push(id.to_owned());
        }
        ids.sort();
        if ids.is_empty() {
            prefs.remove(key)
        } else {
            prefs.set(key, &ids)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scratch(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "dilla-prefs-{tag}-{}-{:?}",
            std::process::id(),
            std::thread::current().id()
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn prefs_round_trip_keeps_unknown_keys() {
        let dir = scratch("round-trip");
        let path = dir.join("prefs.json");
        // A newer build's key and an unknown nested value are in the file before this build writes.
        std::fs::write(
            &path,
            r#"{"future.flag": {"nested": [1, 2]}, "layout.sidebarWidth": 240}"#,
        )
        .unwrap();
        let prefs = Prefs::instance(&dir);
        assert_eq!(prefs.get::<u32>("layout.sidebarWidth"), Some(240));
        prefs.set("layout.sidebarWidth", &300u32).unwrap();
        prefs.set("drafts.general", &"half a thought").unwrap();
        // Another handle on the same file, written after the first one read it: neither loses the other's key.
        let other = Prefs::instance(&dir);
        other.set("voice.noiseMode", &"enhanced").unwrap();
        prefs.set("layout.asideWidth", &340u32).unwrap();

        let again = Prefs::instance(&dir);
        assert_eq!(again.get::<u32>("layout.sidebarWidth"), Some(300));
        assert_eq!(again.get::<u32>("layout.asideWidth"), Some(340));
        assert_eq!(
            again.get::<String>("drafts.general").as_deref(),
            Some("half a thought")
        );
        assert_eq!(
            again.get::<String>("voice.noiseMode").as_deref(),
            Some("enhanced")
        );
        let raw: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        assert_eq!(raw["future.flag"], serde_json::json!({"nested": [1, 2]}));
        // A wrong type reads as absent; a removed key is gone, the rest stays.
        assert_eq!(again.get::<bool>("layout.sidebarWidth"), None);
        again.remove("drafts.general").unwrap();
        assert_eq!(Prefs::instance(&dir).get::<String>("drafts.general"), None);
        assert_eq!(
            Prefs::instance(&dir).get::<u32>("layout.sidebarWidth"),
            Some(300)
        );
        // The app-wide file is a separate one.
        let app = Prefs::app(&dir);
        assert_eq!(app.path(), dir.join("desktop.json"));
        assert_eq!(app.get::<u32>("layout.sidebarWidth"), None);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_missing_or_broken_file_reads_as_empty() {
        let dir = scratch("broken");
        assert_eq!(Prefs::instance(&dir).get::<u32>("a"), None);
        std::fs::write(dir.join("prefs.json"), b"[not an object").unwrap();
        let prefs = Prefs::instance(&dir);
        assert_eq!(prefs.get::<u32>("a"), None);
        prefs.set("a", &1u32).unwrap();
        assert_eq!(Prefs::instance(&dir).get::<u32>("a"), Some(1));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn secret_names_are_words_not_substrings() {
        for key in [
            "auth.token",
            "session_token",
            "sessionToken",
            "gifs.apiKey",
            "gifs.api-key",
            "push.secret",
            "Key",
            "voice.pttKey",
        ] {
            assert!(names_a_secret(key), "{key}");
        }
        for key in [
            "layout.sidebarWidth",
            "keyboard.hints",
            "voice.pttChord",
            "monkey",
            "tokens.seen",
        ] {
            assert!(!names_a_secret(key), "{key}");
        }
    }

    #[test]
    fn layout_prefs_keep_widths_and_id_lists() {
        let dir = scratch("layout");
        let prefs = Prefs::instance(&dir);
        assert_eq!(LayoutPrefs::read(&prefs), LayoutPrefs::default());
        LayoutPrefs::write_sidebar(&prefs, Some(324.4)).unwrap();
        LayoutPrefs::write_aside(&prefs, Some(300.0)).unwrap();
        let again = Prefs::instance(&dir);
        assert_eq!(
            LayoutPrefs::read(&again),
            LayoutPrefs {
                sidebar: Some(324.0),
                aside: Some(300.0)
            }
        );
        // A reset forgets the width; a junk width reads as absent.
        LayoutPrefs::write_sidebar(&again, None).unwrap();
        again.set(LayoutPrefs::ASIDE, &-4.0f64).unwrap();
        assert_eq!(LayoutPrefs::read(&again), LayoutPrefs::default());
        // Id lists toggle and stay sorted and unique.
        LayoutPrefs::set_id(&again, LayoutPrefs::COLLAPSED, "bb", true).unwrap();
        LayoutPrefs::set_id(&again, LayoutPrefs::COLLAPSED, "aa", true).unwrap();
        LayoutPrefs::set_id(&again, LayoutPrefs::COLLAPSED, "aa", true).unwrap();
        assert_eq!(
            LayoutPrefs::ids(&again, LayoutPrefs::COLLAPSED),
            ["aa", "bb"]
        );
        LayoutPrefs::set_id(&again, LayoutPrefs::COLLAPSED, "aa", false).unwrap();
        LayoutPrefs::set_id(&again, LayoutPrefs::COLLAPSED, "bb", false).unwrap();
        assert!(LayoutPrefs::ids(&again, LayoutPrefs::COLLAPSED).is_empty());
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    #[cfg(debug_assertions)]
    #[should_panic(expected = "names a secret")]
    fn prefs_never_hold_secrets() {
        let dir = scratch("secret");
        Prefs::instance(&dir)
            .set("auth.token", &"abc")
            .expect("never reached: the write panics first");
    }
}
