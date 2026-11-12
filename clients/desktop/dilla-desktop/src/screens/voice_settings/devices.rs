//! The devices the Voice section lists (packages/web/src/call/devices.ts, screens/settings/Voice.tsx `useDevices`): the
//! host's microphones and speakers, read at once and again whenever the list changes (a headset plugged in or pulled out),
//! through dilla-media's `watch_host`. Listing opens no device.

use std::sync::Arc;
use std::time::Duration;

use dilla_client::state::voice_prefs::{AudioDevice, AudioDevices};
use dilla_media::devices::{AudioHost, Device, DeviceList, watch_host};

/// How often the host is asked for its list (cpal reports no device-change events).
pub const POLL: Duration = Duration::from_secs(1);

fn state_of(devices: &[Device]) -> Vec<AudioDevice> {
    devices
        .iter()
        .map(|d| AudioDevice {
            id: d.id.clone(),
            name: d.name.clone(),
        })
        .collect()
}

/// The list as the settings hold it.
pub fn to_state(list: &DeviceList) -> AudioDevices {
    AudioDevices {
        inputs: state_of(&list.inputs),
        outputs: state_of(&list.outputs),
    }
}

/// Watches the host's devices on a thread of its own. Dropping it stops the poller.
pub struct DeviceMonitor {
    stop: std::sync::mpsc::Sender<()>,
}

impl DeviceMonitor {
    /// `on_list` is called with the first list and with each change, from the monitor's thread.
    pub fn start(
        host: Arc<dyn AudioHost>,
        every: Duration,
        on_list: impl Fn(AudioDevices) + Send + 'static,
    ) -> Self {
        let (stop, stopped) = std::sync::mpsc::channel::<()>();
        let spawned = std::thread::Builder::new()
            .name("dilla-device-list".into())
            .spawn(move || {
                let watch = watch_host(host, every);
                loop {
                    match watch.recv_timeout(Duration::from_millis(100)) {
                        Ok(list) => on_list(to_state(&list)),
                        Err(std::sync::mpsc::RecvTimeoutError::Timeout) => {}
                        Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => return,
                    }
                    // A message or a dropped monitor ends the thread; dropping `watch` stops the poller.
                    if !matches!(
                        stopped.try_recv(),
                        Err(std::sync::mpsc::TryRecvError::Empty)
                    ) {
                        return;
                    }
                }
            });
        if let Err(e) = spawned {
            log::warn!("dilla: the device list thread did not start: {e}");
        }
        Self { stop }
    }
}

impl Drop for DeviceMonitor {
    fn drop(&mut self) {
        let _ = self.stop.send(());
    }
}
