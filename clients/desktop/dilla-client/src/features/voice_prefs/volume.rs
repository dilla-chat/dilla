//! Per-person volume (packages/web/src/call/volume.ts, L-TS-54 `voice.volume.<user>`, flow 06 § Tiles): 0..200 % in steps
//! of 10, 100 by default. Deafened or paused, every gain is 0.

use std::collections::BTreeMap;

/// Volumes by user id (32 lower-case hex digits).
pub type Volumes = BTreeMap<String, u32>;

/// A per-person volume: a whole multiple of 10 from 0 to 200.
pub fn is_volume(v: u32) -> bool {
    v <= 200 && v.is_multiple_of(10)
}

/// The input and output volume of Settings -> Voice: a whole percent 0..200.
pub fn is_percent(v: u32) -> bool {
    v <= 200
}

/// A user id as the routes spell it.
pub fn is_user_id(hex: &str) -> bool {
    hex.len() == 32
        && hex
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

/// What silences the call: deafened or paused (RV-3) are 0; `output` is the output volume of Settings -> Voice (100 when
/// it is not a percent).
pub struct GainState {
    pub deafened: bool,
    pub paused: bool,
    pub output: Option<u32>,
}

/// How loud `user` plays: `volumes[user]` (100 when absent or malformed) times the output volume.
pub fn gain_of(user: &str, volumes: &Volumes, s: &GainState) -> f32 {
    if s.deafened || s.paused {
        return 0.0;
    }
    let v = volumes
        .get(user)
        .copied()
        .filter(|v| is_volume(*v))
        .unwrap_or(100);
    let output = s.output.filter(|o| is_percent(*o)).unwrap_or(100);
    (v as f32 / 100.0) * (output as f32 / 100.0)
}

#[cfg(test)]
mod tests {
    use super::*;

    const ALICE: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";

    #[test]
    fn volumes_are_tens_up_to_200() {
        assert!(is_volume(0) && is_volume(100) && is_volume(200));
        assert!(!is_volume(5) && !is_volume(210) && !is_volume(201));
        assert!(is_percent(57) && !is_percent(201));
    }

    #[test]
    fn the_gain_multiplies_person_and_output_and_deafened_is_silence() {
        let volumes = Volumes::from([(ALICE.to_owned(), 50)]);
        let on = GainState {
            deafened: false,
            paused: false,
            output: Some(200),
        };
        assert_eq!(gain_of(ALICE, &volumes, &on), 1.0);
        assert_eq!(gain_of("someone-else", &volumes, &on), 2.0);
        for (deafened, paused) in [(true, false), (false, true)] {
            let s = GainState {
                deafened,
                paused,
                output: None,
            };
            assert_eq!(gain_of(ALICE, &volumes, &s), 0.0);
        }
    }

    #[test]
    fn a_malformed_volume_reads_as_100() {
        let volumes = Volumes::from([(ALICE.to_owned(), 55)]);
        let s = GainState {
            deafened: false,
            paused: false,
            output: Some(999),
        };
        assert_eq!(gain_of(ALICE, &volumes, &s), 1.0);
    }

    #[test]
    fn a_user_id_is_32_lowercase_hex_digits() {
        assert!(is_user_id(ALICE));
        assert!(!is_user_id("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"));
        assert!(!is_user_id("abc"));
    }
}
