//! Task 13's components (`components/dms/`): the trust alert (packages/ui TrustAlert), the safety-number comparison's
//! body (packages/ui SafetyCompare), the Privacy section's contact states (settings/Privacy.tsx `dw-trust-*`) and the DM
//! picker's rows (Shell.tsx `DmPicker`, `dw-dm-picker`). Every size is a token or the web CSS's value; no state lives
//! here, and no English: every label comes from the caller.

pub mod dm_list;
pub mod dm_picker;
pub mod privacy;
pub mod safety_compare;
pub mod trust_alert;

pub use dm_list::{DmItem, DmList};
pub use dm_picker::{DmPickerList, PickerMember};
pub use privacy::{TrustTone, removed_note, trust_key, trust_state};
pub use safety_compare::SafetyCompareBody;
pub use trust_alert::TrustAlert;
