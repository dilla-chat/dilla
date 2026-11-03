// Fixture AppImages use the same desktop filename as the inspected production
// image, while retaining their own app ID, product name, profile and trust.
export const fixtureDesktopMetadata = () => ({ desktopName: 'dev.dilla.desktop.desktop' });
export const fixtureLinuxDesktopOptions = () => ({ syncDesktopName: true });
