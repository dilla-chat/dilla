//! The fixture's density (Task 19b): `"density"` in the fixture JSON. The web shooter puts it on the document root
//! (`data-density`) and leaves the viewer's saved setting alone; here the window takes it ([`density_of`], set by the app
//! when it opens the fixture) and the setting stays as the world has it, so Appearance shows what the web's shows.

use dilla_client::testing::spec::{FixtureDensity, ScreenFixture};
use dilla_desktop_ui::metrics::Density;

/// The density the fixture's window draws at.
pub fn density_of(fixture: &ScreenFixture) -> Density {
    match fixture.density {
        FixtureDensity::Compact => Density::Compact,
        FixtureDensity::Regular => Density::Regular,
        FixtureDensity::Cozy => Density::Cozy,
    }
}
