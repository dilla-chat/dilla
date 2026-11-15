//! Parity harness for the native desktop client: [`Driver`] runs the automation build of `dilla-desktop`,
//! [`assert_parity`] compares a screenshot with a reference image, and [`screen::assert_screen`] does the whole screen
//! round: a fixture JSON both shooters read, the GPUI side drawn from it, the R13 text mask from the app's own text
//! bounds, the committed tolerance.

pub mod a11y;
mod diff;
mod driver;
pub mod fixture;
pub mod screen;

pub use diff::{
    Os, ParityFailure, Rect, TextMask, Tolerance, assert_parity, assert_parity_masked,
    assert_parity_masked_with, assert_parity_with, chrome_difference_pct, tolerance_for,
};
pub use driver::{Driver, ElementInfo, NoReply, target_dir};
pub use image::RgbaImage;
pub use screen::{ScreenReport, assert_screen, text_mask};
