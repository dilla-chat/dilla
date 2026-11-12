//! Parity harness for the native desktop client: [`Driver`] runs the automation build of
//! `dilla-desktop`, [`assert_parity`] compares a screenshot with a reference image.

mod diff;
mod driver;

pub use diff::{Os, ParityFailure, Tolerance, assert_parity, assert_parity_with, tolerance_for};
pub use driver::{Driver, ElementInfo};
pub use image::RgbaImage;
