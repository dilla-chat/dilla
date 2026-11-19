//! Operating-system permissions (Package 7, decision D1: OS prompts only, no per-instance in-app prompts).
//!
//! The OS asks the person itself the first time the app opens the microphone or the camera (macOS, with the usage
//! strings of [`MACOS_USAGE`]) or records the screen. What the app does is read the answer, so a denied device is
//! shown as such instead of sending silence: [`status`]. A call joins listen-only with the "microphone is blocked"
//! notice when the microphone is denied (`screens/voice/session.rs`, beta.5 `voice.micDenied`).
//!
//! - macOS: `AVCaptureDevice.authorizationStatusForMediaType:` (audio, video) and `CGPreflightScreenCaptureAccess`
//!   (screen; a check that never prompts).
//! - Windows: the privacy settings' consent store (`HKLM`/`HKCU …\CapabilityAccessManager\ConsentStore\<cap>`, and
//!   `HKCU …\<cap>\NonPackaged` for desktop apps). Windows does not prompt desktop apps; a "Deny" there is final.
//! - Linux: no per-app device permission outside a sandbox ([`OsPermission::NotGated`]).
//!
//! beta.5's per-instance grants ("Allow {host} to use microphone or camera?", `preferences.ts`) are not ported (D1):
//! they guarded a renderer that loaded each instance's web code, and the native app runs only shipped code.

use gpui::App;

/// A device the OS may gate.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Device {
    Microphone,
    Camera,
    Screen,
}

/// What the OS says about a [`Device`] for this app.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum OsPermission {
    /// Allowed.
    Granted,
    /// Denied (or restricted by policy): opening it gives nothing or silence.
    Denied,
    /// Not asked yet: opening it shows the OS prompt.
    NotDetermined,
    /// The OS does not gate it for this app.
    NotGated,
}

/// The macOS Info.plist usage strings (beta.5 `electron-builder.cjs` `extendInfo`, word for word), for the release
/// plan's Task 12. Under the hardened runtime the bundle also needs the entitlements
/// `com.apple.security.device.audio-input` and `com.apple.security.device.camera` (beta.5
/// `assets/entitlements.mac.plist`); screen capture needs none. `NSAudioCaptureUsageDescription` is new (GPUI gaps
/// Task 5): the "system audio recording" prompt of the CoreAudio process tap that shares an application's sound.
pub const MACOS_USAGE: &[(&str, &str)] = &[
    (
        "NSMicrophoneUsageDescription",
        "Dilla uses your microphone in encrypted calls.",
    ),
    (
        "NSCameraUsageDescription",
        "Dilla uses your camera when you enable video in a call.",
    ),
    (
        "NSScreenCaptureUsageDescription",
        "Dilla shares the screen or application you choose in a call.",
    ),
    (
        "NSAudioCaptureUsageDescription",
        "Dilla shares the sound of the application you choose when you share your screen.",
    ),
    (
        "NSLocalNetworkUsageDescription",
        "Dilla connects to your chosen instance and encrypted calls on your local network.",
    ),
];

/// What the OS says about `device` now. Never prompts; safe on any thread.
pub fn status(device: Device) -> OsPermission {
    imp::status(device)
}

/// The microphone is denied: a call joins listen-only and says so.
pub fn microphone_denied() -> bool {
    status(Device::Microphone) == OsPermission::Denied
}

/// Called once by [`super::install`], first: logs what the OS says, so a support log shows a blocked device.
pub fn install(_cx: &mut App) {
    for device in [Device::Microphone, Device::Camera, Device::Screen] {
        let s = status(device);
        if s == OsPermission::Denied {
            log::warn!("dilla-desktop: the OS denies Dilla the {device:?}");
        } else {
            log::info!("dilla-desktop: OS permission for the {device:?}: {s:?}");
        }
    }
}

#[cfg(not(any(target_os = "macos", windows)))]
mod imp {
    use super::{Device, OsPermission};

    pub fn status(_device: Device) -> OsPermission {
        OsPermission::NotGated
    }
}

#[cfg(target_os = "macos")]
#[allow(unsafe_code)]
mod imp {
    use objc2::msg_send;
    use objc2::runtime::AnyClass;
    use objc2_foundation::NSString;

    use super::{Device, OsPermission};

    #[link(name = "AVFoundation", kind = "framework")]
    unsafe extern "C" {
        static AVMediaTypeAudio: &'static NSString;
        static AVMediaTypeVideo: &'static NSString;
    }

    #[link(name = "CoreGraphics", kind = "framework")]
    unsafe extern "C" {
        fn CGPreflightScreenCaptureAccess() -> bool;
    }

    pub fn status(device: Device) -> OsPermission {
        match device {
            Device::Microphone => media(false),
            Device::Camera => media(true),
            // SAFETY: a plain C query (macOS 10.15+) that never prompts.
            Device::Screen => {
                if unsafe { CGPreflightScreenCaptureAccess() } {
                    OsPermission::Granted
                } else {
                    // Not granted: never asked and denied look the same here; the capture's own start prompts.
                    OsPermission::NotDetermined
                }
            }
        }
    }

    /// `AVAuthorizationStatus`: 0 not determined, 1 restricted, 2 denied, 3 authorized.
    fn media(video: bool) -> OsPermission {
        let Some(class) = AnyClass::get(c"AVCaptureDevice") else {
            return OsPermission::NotGated;
        };
        // SAFETY: AVFoundation's media-type constants are immutable NSStrings for the process's life.
        let media: &NSString = unsafe {
            if video {
                AVMediaTypeVideo
            } else {
                AVMediaTypeAudio
            }
        };
        // SAFETY: a class method taking an AVMediaType and returning NSInteger; it never prompts.
        let status: isize = unsafe { msg_send![class, authorizationStatusForMediaType: media] };
        match status {
            3 => OsPermission::Granted,
            1 | 2 => OsPermission::Denied,
            _ => OsPermission::NotDetermined,
        }
    }
}

#[cfg(windows)]
#[allow(unsafe_code)]
mod imp {
    use windows_sys::Win32::System::Registry::{
        HKEY, HKEY_CURRENT_USER, HKEY_LOCAL_MACHINE, RRF_RT_REG_SZ, RegGetValueW,
    };

    use super::{Device, OsPermission};

    const STORE: &str =
        r"Software\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore";

    pub fn status(device: Device) -> OsPermission {
        let capability = match device {
            Device::Microphone => "microphone",
            Device::Camera => "webcam",
            // Windows does not gate screen capture for desktop apps.
            Device::Screen => return OsPermission::NotGated,
        };
        let keys = [
            (HKEY_LOCAL_MACHINE, format!(r"{STORE}\{capability}")),
            (HKEY_CURRENT_USER, format!(r"{STORE}\{capability}")),
            (
                HKEY_CURRENT_USER,
                format!(r"{STORE}\{capability}\NonPackaged"),
            ),
        ];
        let values: Vec<Option<String>> =
            keys.iter().map(|(root, key)| value(*root, key)).collect();
        if values
            .iter()
            .flatten()
            .any(|v| v.eq_ignore_ascii_case("Deny"))
        {
            OsPermission::Denied
        } else if values
            .iter()
            .flatten()
            .any(|v| v.eq_ignore_ascii_case("Allow"))
        {
            OsPermission::Granted
        } else {
            OsPermission::NotGated
        }
    }

    /// The string `Value` under `root\key`, if there is one.
    fn value(root: HKEY, key: &str) -> Option<String> {
        let key: Vec<u16> = key.encode_utf16().chain([0]).collect();
        let name: Vec<u16> = "Value".encode_utf16().chain([0]).collect();
        let mut buf = [0u16; 64];
        let mut len = std::mem::size_of_val(&buf) as u32;
        // SAFETY: NUL-terminated names, and a buffer of `len` bytes the call writes a NUL-terminated string into.
        let err = unsafe {
            RegGetValueW(
                root,
                key.as_ptr(),
                name.as_ptr(),
                RRF_RT_REG_SZ,
                std::ptr::null_mut(),
                buf.as_mut_ptr().cast(),
                &mut len,
            )
        };
        if err != 0 {
            return None;
        }
        let chars = (len as usize / 2).min(buf.len());
        let text = String::from_utf16_lossy(&buf[..chars]);
        Some(text.trim_end_matches('\0').to_owned())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Linux gates no device for an unsandboxed app, so a call there never goes listen-only for a permission.
    #[cfg(target_os = "linux")]
    #[test]
    fn linux_gates_no_device() {
        for device in [Device::Microphone, Device::Camera, Device::Screen] {
            assert_eq!(status(device), OsPermission::NotGated);
        }
        assert!(!microphone_denied());
    }

    /// The usage strings the release plan's Info.plist takes: every gated device has one.
    #[test]
    fn every_gated_device_has_a_macos_usage_string() {
        for key in [
            "NSMicrophoneUsageDescription",
            "NSCameraUsageDescription",
            "NSScreenCaptureUsageDescription",
            "NSAudioCaptureUsageDescription",
        ] {
            assert!(
                MACOS_USAGE
                    .iter()
                    .any(|(k, v)| *k == key && v.starts_with("Dilla ")),
                "{key}"
            );
        }
    }
}
