//! The platform-independent half of application audio, shared by every capture (Linux, Windows, macOS): frame
//! cutting with carry-over, the finite check and clamp on every sample, Dilla's own process tree left out of a list,
//! and the source list's bounds. Pure: compiled and tested on every OS.

use std::collections::{HashMap, HashSet};

use super::{AppAudioError, AppSource, FRAME_SAMPLES, MAX_CHUNK_BYTES, MAX_LABEL, MAX_SOURCES};

/// One sample, checked finite and clamped to [-1, 1] (beta.5 stops the capture on an invalid sample).
pub(crate) fn checked(v: f32) -> Result<f32, AppAudioError> {
    if v.is_finite() {
        Ok(v.clamp(-1.0, 1.0))
    } else {
        Err(AppAudioError::Invalid("sample"))
    }
}

/// beta.5 `PCMFrames`: assembles little-endian f32 stereo bytes into [`FRAME_SAMPLES`]-sample frames, each sample checked
/// finite and clamped to [-1, 1]. It never keeps more than one frame's worth of bytes between chunks.
#[derive(Default)]
pub struct PcmFrames {
    tail: Vec<u8>,
}

impl PcmFrames {
    pub fn push(
        &mut self,
        chunk: &[u8],
        mut emit: impl FnMut(Vec<f32>),
    ) -> Result<(), AppAudioError> {
        if chunk.len() > MAX_CHUNK_BYTES {
            return Err(AppAudioError::Invalid("chunk"));
        }
        const FRAME_BYTES: usize = FRAME_SAMPLES * 4;
        let mut bytes = std::mem::take(&mut self.tail);
        bytes.extend_from_slice(chunk);
        let mut at = 0;
        while at + FRAME_BYTES <= bytes.len() {
            let mut frame = Vec::with_capacity(FRAME_SAMPLES);
            for raw in bytes[at..at + FRAME_BYTES].as_chunks::<4>().0 {
                frame.push(checked(f32::from_le_bytes(*raw))?);
            }
            emit(frame);
            at += FRAME_BYTES;
        }
        self.tail = bytes[at..].to_vec();
        Ok(())
    }
}

/// [`PcmFrames`] for a capture that hands over f32 samples (WASAPI, CoreAudio): interleaved stereo samples of any count
/// become exact [`FRAME_SAMPLES`]-sample frames, each sample checked finite and clamped; the rest waits for the next
/// buffer (never more than one frame's worth).
#[cfg_attr(not(any(target_os = "windows", target_os = "macos")), allow(dead_code))]
#[derive(Default)]
pub(crate) struct SampleFrames {
    carry: Vec<f32>,
}

#[cfg_attr(not(any(target_os = "windows", target_os = "macos")), allow(dead_code))]
impl SampleFrames {
    pub(crate) fn push(
        &mut self,
        samples: &[f32],
        mut emit: impl FnMut(Vec<f32>),
    ) -> Result<(), AppAudioError> {
        if samples.len() > MAX_CHUNK_BYTES / 4 {
            return Err(AppAudioError::Invalid("chunk"));
        }
        for &v in samples {
            self.carry.push(checked(v)?);
            if self.carry.len() == FRAME_SAMPLES {
                emit(std::mem::replace(
                    &mut self.carry,
                    Vec::with_capacity(FRAME_SAMPLES),
                ));
            }
        }
        Ok(())
    }
}

/// Whether `pid` is `root` or one of its descendants (beta.5 `excluded`: the audio service may run in a child of the
/// app, so the whole tree is left out), walking at most 128 parents through `parent_of`.
pub fn in_tree(pid: u32, root: u32, parent_of: impl Fn(u32) -> Option<u32>) -> bool {
    let mut pid = pid;
    for _ in 0..128 {
        if pid <= 1 {
            return false;
        }
        if pid == root {
            return true;
        }
        match parent_of(pid) {
            Some(parent) if parent != pid => pid = parent,
            _ => return false,
        }
    }
    false
}

/// `sources` without Dilla's own process tree (`root`), given every process's parent (`parents`: pid to parent pid, a
/// snapshot of the system's process table).
#[cfg_attr(not(any(target_os = "windows", target_os = "macos")), allow(dead_code))]
pub(crate) fn exclude_tree(
    sources: Vec<AppSource>,
    parents: &HashMap<u32, u32>,
    root: u32,
) -> Vec<AppSource> {
    sources
        .into_iter()
        .filter(|s| !in_tree(s.pid, root, |pid| parents.get(&pid).copied()))
        .collect()
}

/// Dilla's ancestors (`root`'s parent, its parent, and so on), given every process's parent (`parents`). Windows'
/// process loopback captures the chosen process's whole tree (`PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE`), so
/// choosing an ancestor (explorer.exe, a terminal, a launcher) would capture Dilla's own call audio: these are left out
/// too. The walk stops at pid 0, a missing parent, a pid already seen (a cycle, or a reused pid) or after
/// [`MAX_ANCESTORS`] steps. A reused pid can only add a process to the set, never let Dilla's tree in.
#[cfg_attr(not(target_os = "windows"), allow(dead_code))]
pub(crate) fn ancestors(root: u32, parents: &HashMap<u32, u32>) -> HashSet<u32> {
    let mut up = HashSet::new();
    let mut pid = root;
    for _ in 0..MAX_ANCESTORS {
        match parents.get(&pid) {
            Some(&parent) if parent != 0 && parent != root && up.insert(parent) => pid = parent,
            _ => break,
        }
    }
    up
}

/// How far [`ancestors`] walks up.
#[cfg_attr(not(target_os = "windows"), allow(dead_code))]
const MAX_ANCESTORS: usize = 64;

/// `sources` without Dilla's own process tree and without its ancestors ([`ancestors`]): what Windows lists.
#[cfg_attr(not(target_os = "windows"), allow(dead_code))]
pub(crate) fn exclude_tree_and_ancestors(
    sources: Vec<AppSource>,
    parents: &HashMap<u32, u32>,
    root: u32,
) -> Vec<AppSource> {
    let up = ancestors(root, parents);
    exclude_tree(sources, parents, root)
        .into_iter()
        .filter(|s| !up.contains(&s.pid))
        .collect()
}

/// The playing application a window owned by `owner` brings (Task 44, D21). `own` is Dilla's pid and `sources` is the
/// list [`super::sources`] gave, in its order. The match is:
/// - `owner` itself, or else
/// - the closest descendant of `owner` that plays (a browser's audio service, an Electron renderer), the earlier
///   in `sources` on a tie (Linux: the lower serial, the first playing stream).
///
/// Never matched:
/// - a parent or a sibling of the owner (a launcher's, a terminal's or Steam's own sound must not ride along);
/// - anything when `owner` is 0 or 1, Dilla, in Dilla's tree, or one of Dilla's ancestors (the terminal or launcher
///   that started Dilla: its descendants include Dilla's siblings);
/// - a source in Dilla's tree (the lists already leave it out; checked again).
///
/// Walks at most 128 parents per pid and 64 of Dilla's ancestors, stopping at 0, 1, a missing parent or a repeat.
pub fn app_of_window(
    owner: u32,
    own: u32,
    sources: &[AppSource],
    parent_of: impl Fn(u32) -> Option<u32>,
) -> Option<AppSource> {
    if owner <= 1 || in_tree(owner, own, &parent_of) {
        return None;
    }
    let mut up = HashSet::new();
    let mut pid = own;
    for _ in 0..MAX_ANCESTORS {
        match parent_of(pid) {
            Some(parent) if parent > 1 && parent != own && up.insert(parent) => pid = parent,
            _ => break,
        }
    }
    if up.contains(&owner) {
        return None;
    }
    sources
        .iter()
        .filter(|s| !in_tree(s.pid, own, &parent_of))
        .filter_map(|s| depth_below(s.pid, owner, &parent_of).map(|depth| (depth, s)))
        // The first of the closest: `min_by_key` keeps the earlier element on a tie.
        .min_by_key(|(depth, _)| *depth)
        .map(|(_, s)| s.clone())
}

/// How many parents up from `pid` `owner` is (0: `pid` is `owner`); `None` when `owner` is neither.
fn depth_below(pid: u32, owner: u32, parent_of: &impl Fn(u32) -> Option<u32>) -> Option<usize> {
    let mut seen = HashSet::new();
    let mut pid = pid;
    for depth in 0..128 {
        if pid == owner {
            return Some(depth);
        }
        if pid <= 1 || !seen.insert(pid) {
            return None;
        }
        pid = parent_of(pid)?;
    }
    None
}

/// `links` (pid to parent pid) without the stale ones: Windows never updates a process's recorded parent when that
/// parent exits and reuses pids, so a link is kept only when both creation times (`created`) are known and the parent
/// was created no later than the child. A reused pid is always newer than the orphans it would otherwise adopt.
#[cfg_attr(not(target_os = "windows"), allow(dead_code))]
pub(crate) fn keep_fresh_links(
    links: HashMap<u32, u32>,
    created: impl Fn(u32) -> Option<u64>,
) -> HashMap<u32, u32> {
    links
        .into_iter()
        .filter(|&(pid, parent)| match (created(parent), created(pid)) {
            (Some(parent), Some(child)) => parent <= child,
            _ => false,
        })
        .collect()
}

/// A source's label: at most [`MAX_LABEL`] characters, and a name for a source that gives none.
pub(crate) fn cap_label(label: &str, pid: u32) -> String {
    if label.trim().is_empty() {
        format!("Application {pid}")
    } else {
        label.chars().take(MAX_LABEL).collect()
    }
}

/// beta.5 `parseAudioSources`'s limits: at most [`MAX_SOURCES`], one per id, labels of 1 to [`MAX_LABEL`] characters.
pub fn cap_sources(sources: Vec<AppSource>) -> Vec<AppSource> {
    let mut seen = HashSet::new();
    sources
        .into_iter()
        .filter(|s| s.serial != 0 && s.pid != 0 && seen.insert((s.serial, s.pid)))
        .map(|mut s| {
            s.label = cap_label(&s.label, s.pid);
            s
        })
        .take(MAX_SOURCES)
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn le(samples: &[f32]) -> Vec<u8> {
        samples.iter().flat_map(|s| s.to_le_bytes()).collect()
    }

    fn source(serial: u64, pid: u32, label: &str) -> AppSource {
        AppSource {
            serial,
            pid,
            label: label.to_owned(),
        }
    }

    /// 1 -> 50 (session) -> 100 (Dilla) -> 110 (Dilla's helper); 50 -> 200 (a browser) -> 220 (audio service) and
    /// 200 -> 230 (renderer) -> 231; 50 -> 300 (a music player, its own parent).
    fn tree(pid: u32) -> Option<u32> {
        match pid {
            50 => Some(1),
            100 | 200 => Some(50),
            110 => Some(100),
            220 | 230 => Some(200),
            231 => Some(230),
            300 => Some(300),
            _ => None,
        }
    }

    fn pick(owner: u32, list: &[(u32, u64)]) -> Option<u32> {
        let sources: Vec<AppSource> = list.iter().map(|&(p, s)| source(s, p, "app")).collect();
        app_of_window(owner, 100, &sources, tree).map(|a| a.pid)
    }

    #[test]
    fn a_window_brings_its_owner_s_sound_or_its_closest_playing_descendant_s() {
        assert_eq!(pick(200, &[(300, 5), (220, 9)]), Some(220));
        // The owner wins over a lower serial.
        assert_eq!(pick(200, &[(220, 9), (200, 12)]), Some(200));
        // Depth 1 beats depth 2.
        assert_eq!(pick(200, &[(231, 3), (220, 9)]), Some(220));
        // The same depth: the earlier in the list.
        assert_eq!(pick(200, &[(230, 7), (220, 9)]), Some(230));
        assert_eq!(pick(200, &[(300, 5)]), None);
    }

    #[test]
    fn a_window_of_dilla_its_tree_or_its_ancestors_brings_no_sound() {
        for owner in [100, 110, 50] {
            assert_eq!(pick(owner, &[(220, 9), (300, 5)]), None, "owner {owner}");
        }
        assert_eq!(pick(0, &[(220, 9)]), None);
        assert_eq!(pick(1, &[(220, 9)]), None);
        // A source in Dilla's tree is never returned.
        assert_eq!(pick(50, &[(110, 1), (100, 2)]), None);
        assert_eq!(pick(110, &[(110, 1)]), None);
    }

    #[test]
    fn a_link_to_a_parent_created_after_its_child_is_stale() {
        // 200's recorded parent 50 was created after it (the pid was reused), 220's parent 200 before it, 230 has no
        // known creation time.
        let created = |pid: u32| match pid {
            50 => Some(900),
            200 => Some(100),
            220 => Some(300),
            _ => None,
        };
        let links = HashMap::from([(200, 50), (220, 200), (230, 200)]);
        assert_eq!(
            keep_fresh_links(links, created),
            HashMap::from([(220, 200)])
        );
    }

    #[test]
    fn a_sibling_or_a_parent_of_the_owner_is_never_matched() {
        assert_eq!(pick(220, &[(200, 1), (230, 2)]), None);
        // A self-parent ends the walk.
        assert_eq!(pick(300, &[(200, 1), (230, 2)]), None);
        assert_eq!(pick(300, &[(300, 4)]), Some(300));
    }

    /// audio.test.ts / app-audio-lifetime.test.ts (beta.5 `PCMFrames`): frames are 960 samples, split chunks join, the
    /// samples are clamped, and a partial frame waits for its rest.
    #[test]
    fn pcm_chunks_become_clamped_960_sample_frames() {
        let mut p = PcmFrames::default();
        let mut out = Vec::new();
        let mut samples = vec![0.25f32; FRAME_SAMPLES * 2 + 10];
        samples[1] = 3.0;
        samples[2] = -7.5;
        let bytes = le(&samples);
        // Split mid-sample: the tail carries over.
        p.push(&bytes[..1001], |f| out.push(f)).unwrap();
        assert!(out.is_empty());
        p.push(&bytes[1001..], |f| out.push(f)).unwrap();
        assert_eq!(out.len(), 2);
        assert!(out.iter().all(|f| f.len() == FRAME_SAMPLES));
        assert_eq!((out[0][1], out[0][2]), (1.0, -1.0), "clamped");
        assert_eq!(p.tail.len(), 40, "ten samples wait for their frame");
    }

    /// A buffer of any length (WASAPI hands over what the period produced) becomes exact 960-sample frames in order,
    /// clamped, the rest carried into the next buffer.
    #[test]
    fn frames_are_cut_to_960_samples() {
        let mut f = SampleFrames::default();
        let mut out: Vec<Vec<f32>> = Vec::new();
        let stream: Vec<f32> = (0..FRAME_SAMPLES * 3 + 7)
            .map(|i| (i % 1000) as f32 / 1000.0)
            .collect();
        let mut fed = 0;
        for len in [1, 441, 959, 2, 1400, 3] {
            f.push(&stream[fed..fed + len], |fr| out.push(fr)).unwrap();
            fed += len;
        }
        f.push(&stream[fed..], |fr| out.push(fr)).unwrap();
        assert_eq!(out.len(), 3);
        assert!(out.iter().all(|fr| fr.len() == FRAME_SAMPLES));
        assert_eq!(
            out.concat(),
            stream[..FRAME_SAMPLES * 3],
            "in order, none lost"
        );
        assert_eq!(f.carry, stream[FRAME_SAMPLES * 3..], "seven samples wait");
        let mut f = SampleFrames::default();
        let mut out = Vec::new();
        let mut loud = vec![0.5f32; FRAME_SAMPLES];
        loud[0] = 4.0;
        loud[1] = -2.0;
        f.push(&loud, |fr| out.push(fr)).unwrap();
        assert_eq!(
            (out[0][0], out[0][1], out[0][2]),
            (1.0, -1.0, 0.5),
            "clamped"
        );
        assert!(f.carry.is_empty());
    }

    #[test]
    fn non_finite_samples_end_the_capture() {
        for bad in [f32::NAN, f32::INFINITY, f32::NEG_INFINITY] {
            assert_eq!(checked(bad), Err(AppAudioError::Invalid("sample")));
            let mut f = SampleFrames::default();
            let mut samples = vec![0.0f32; 10];
            samples[4] = bad;
            assert_eq!(
                f.push(&samples, |_| panic!("no frame")),
                Err(AppAudioError::Invalid("sample"))
            );
        }
        assert_eq!(checked(0.5), Ok(0.5));
        assert_eq!(checked(-9.0), Ok(-1.0));
        let mut p = PcmFrames::default();
        let mut bad = vec![0.0f32; FRAME_SAMPLES];
        bad[5] = f32::NAN;
        assert_eq!(
            p.push(&le(&bad), |_| {}),
            Err(AppAudioError::Invalid("sample"))
        );
        assert_eq!(
            PcmFrames::default().push(&vec![0; MAX_CHUNK_BYTES + 1], |_| {}),
            Err(AppAudioError::Invalid("chunk"))
        );
        assert_eq!(
            SampleFrames::default().push(&vec![0.0; MAX_CHUNK_BYTES / 4 + 1], |_| {}),
            Err(AppAudioError::Invalid("chunk"))
        );
    }

    /// beta.5 `excluded`, from a process-table snapshot: Dilla (100), its children and grandchildren are dropped; another
    /// application, one whose parent is gone, and one in a parent cycle stay.
    #[test]
    fn own_process_tree_is_excluded() {
        let parents: HashMap<u32, u32> = [
            (100, 1),
            (200, 100),
            (300, 200),
            (400, 1),
            (500, 4242),
            (5, 6),
            (6, 5),
        ]
        .into_iter()
        .collect();
        let sessions = vec![
            source(1, 100, "Dilla"),
            source(2, 300, "Dilla's audio child"),
            source(3, 400, "Music"),
            source(4, 500, "Orphan"),
            source(5, 5, "Cycle"),
            source(6, 200, "Dilla helper"),
        ];
        let kept: Vec<u32> = exclude_tree(sessions, &parents, 100)
            .iter()
            .map(|s| s.pid)
            .collect();
        assert_eq!(kept, vec![400, 500, 5]);
    }

    /// Windows captures a chosen process's whole tree, so an ancestor of Dilla (explorer 40 started a terminal 50, which
    /// started Dilla 100) is left out with Dilla's own tree; a sibling application (400, also explorer's) stays.
    #[test]
    fn an_ancestor_of_dilla_is_excluded() {
        let parents: HashMap<u32, u32> =
            [(100, 50), (50, 40), (40, 4), (4, 0), (200, 100), (400, 40)]
                .into_iter()
                .collect();
        assert_eq!(ancestors(100, &parents), HashSet::from([50, 40, 4]));
        let sessions = vec![
            source(1, 40, "Explorer"),
            source(2, 50, "Terminal"),
            source(3, 400, "Music"),
            source(4, 200, "Dilla helper"),
            source(5, 100, "Dilla"),
        ];
        let kept: Vec<u32> = exclude_tree_and_ancestors(sessions, &parents, 100)
            .iter()
            .map(|s| s.pid)
            .collect();
        assert_eq!(kept, vec![400]);
    }

    /// A parent cycle (a reused pid) ends the walk at the first repeat, and a long chain stops after 64 steps.
    #[test]
    fn the_ancestor_walk_ends_on_a_cycle_and_is_bounded() {
        let cycle: HashMap<u32, u32> = [(100, 7), (7, 8), (8, 7)].into_iter().collect();
        assert_eq!(ancestors(100, &cycle), HashSet::from([7, 8]));
        let back_to_root: HashMap<u32, u32> = [(100, 7), (7, 100)].into_iter().collect();
        assert_eq!(ancestors(100, &back_to_root), HashSet::from([7]));
        let chain: HashMap<u32, u32> = (2..1000).map(|pid| (pid, pid + 1)).collect();
        assert_eq!(ancestors(2, &chain).len(), MAX_ANCESTORS);
        assert!(ancestors(100, &HashMap::new()).is_empty());
    }

    #[test]
    fn labels_are_capped_at_512() {
        let long = "å".repeat(600);
        assert_eq!(cap_label(&long, 7).chars().count(), MAX_LABEL);
        assert_eq!(cap_label(&"x".repeat(MAX_LABEL), 7), "x".repeat(MAX_LABEL));
        assert_eq!(cap_label("Spotify", 7), "Spotify");
        assert_eq!(cap_label("  ", 7), "Application 7");
        assert_eq!(cap_label("", 9), "Application 9");
    }
}
