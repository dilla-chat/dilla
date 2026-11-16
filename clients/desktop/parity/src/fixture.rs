//! Screen fixtures on disk: `fixtures/<area>/<state>[-b].json` (the schema of `dilla_client::testing::spec`), the
//! reference PNG `references/<os>/<area>/<state>[-b]-2x.png` and its optional sidecar `<state>[-b].shot.json`.

use std::path::{Path, PathBuf};

pub use dilla_client::testing::spec::{
    ScreenFixture, Shot, SpecError, Step, WorldSpec, derive_id, digest, ids_line, validate_chord,
};

use crate::Os;

/// A fixture read from disk, with where its reference and sidecar live.
#[derive(Clone, Debug)]
pub struct LoadedFixture {
    pub fixture: ScreenFixture,
    pub path: PathBuf,
    /// SHA-256 of the fixture file's bytes.
    pub digest: String,
    /// `references/<os>/<name>-2x.png`.
    pub reference: PathBuf,
    /// The reference's sidecar, when one was shot with it.
    pub shot_path: Option<PathBuf>,
    pub shot: Option<Shot>,
}

/// Why a fixture on disk is refused.
#[derive(Debug, thiserror::Error)]
pub enum FixtureError {
    #[error("{path}: {reason}")]
    Io { path: String, reason: String },
    #[error("{path}: {source}")]
    Spec { path: String, source: SpecError },
    #[error(
        "{path}: the fixture's name `{name}` does not match where the file is (`<fixtures>/{name}.json`)"
    )]
    Name { path: String, name: String },
    #[error("{path}: not a sidecar: {reason}")]
    Shot { path: String, reason: String },
}

/// The directory name of `os` under `references/`.
pub fn os_dir(os: Os) -> &'static str {
    match os {
        Os::Linux => "linux",
        Os::MacOs => "macos",
        Os::Windows => "windows",
    }
}

/// The directory under `references` that holds `name`'s reference on `os`. macOS compares against the Linux web
/// references (decision D-A of the macOS CI plan, with macOS-measured tolerances): `references/macos/<name>` when there
/// is one (the GPUI-only frames, blessed on macOS), else `references/linux/<name>`.
pub fn reference_dir(references: &Path, os: Os, name: &str) -> std::path::PathBuf {
    let own = references.join(os_dir(os));
    if os == Os::MacOs && !own.join(format!("{name}-2x.png")).exists() {
        return references.join(os_dir(Os::Linux));
    }
    own
}

/// Reads and validates the fixture at `path`, which must sit at `<parity>/fixtures/<name>.json`.
pub fn load(path: &Path, os: Os) -> Result<LoadedFixture, FixtureError> {
    let shown = path.display().to_string();
    let text = std::fs::read_to_string(path).map_err(|e| FixtureError::Io {
        path: shown.clone(),
        reason: e.to_string(),
    })?;
    let fixture = ScreenFixture::parse(&text).map_err(|source| FixtureError::Spec {
        path: shown.clone(),
        source,
    })?;
    // `<parity>/fixtures/<area>/<state>.json`: two levels up is the fixtures directory.
    let name_ok = path.with_extension("").ends_with(Path::new(&fixture.name));
    let parity = path
        .parent()
        .and_then(Path::parent)
        .and_then(Path::parent)
        .filter(|_| name_ok);
    let Some(parity) = parity else {
        return Err(FixtureError::Name {
            path: shown,
            name: fixture.name.clone(),
        });
    };
    let base = reference_dir(&parity.join("references"), os, &fixture.name);
    let reference = base.join(format!("{}-2x.png", fixture.name));
    let sidecar = base.join(format!("{}.shot.json", fixture.name));
    let (shot_path, shot) = if sidecar.exists() {
        let raw = std::fs::read_to_string(&sidecar).map_err(|e| FixtureError::Io {
            path: sidecar.display().to_string(),
            reason: e.to_string(),
        })?;
        let shot = serde_json::from_str::<Shot>(&raw).map_err(|e| FixtureError::Shot {
            path: sidecar.display().to_string(),
            reason: e.to_string(),
        })?;
        (Some(sidecar), Some(shot))
    } else {
        (None, None)
    };
    Ok(LoadedFixture {
        digest: digest(text.as_bytes()),
        fixture,
        path: path.to_owned(),
        reference,
        shot_path,
        shot,
    })
}

/// Reads and validates a fixture wherever it is (a test's variant in a temp directory): no reference, no sidecar.
pub fn load_unchecked(path: &Path) -> Result<ScreenFixture, FixtureError> {
    let shown = path.display().to_string();
    let text = std::fs::read_to_string(path).map_err(|e| FixtureError::Io {
        path: shown.clone(),
        reason: e.to_string(),
    })?;
    ScreenFixture::parse(&text).map_err(|source| FixtureError::Spec {
        path: shown,
        source,
    })
}
