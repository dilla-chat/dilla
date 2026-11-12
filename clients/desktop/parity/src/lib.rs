//! Parity harness for the native desktop client: [`Driver`] runs the automation build of
//! `dilla-desktop`, [`assert_parity`] compares a screenshot with a reference image.

mod diff;
mod driver;

pub use diff::{
    Os, ParityFailure, Rect, TextMask, Tolerance, assert_parity, assert_parity_masked,
    assert_parity_masked_with, assert_parity_with, chrome_difference_pct, tolerance_for,
};
pub use driver::{Driver, ElementInfo, target_dir};
pub use image::RgbaImage;
