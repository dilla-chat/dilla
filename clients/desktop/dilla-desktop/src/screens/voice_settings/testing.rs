//! A fake audio host for the Voice section's tests (and the parity world): microphones and speakers that need no sound
//! card. A test feeds the microphone with [`FakeHost::push`], pulls what the speaker plays with [`FakeHost::pull`], pulls a
//! device out with [`FakeHost::unplug`] and counts what is open ([`FakeHost::inputs_open`], [`FakeHost::outputs_open`]),
//! so it can see every device released. Compiled for tests and the `automation` feature only.

use std::sync::{Arc, Mutex};

use dilla_media::MediaError;
use dilla_media::devices::{
    AudioHost, Device, DeviceId, DeviceList, InputSink, OnLost, OutputSource, Running,
};

struct InputSlot {
    id: u64,
    device: String,
    sink: Arc<Mutex<InputSink>>,
    lost: Option<OnLost>,
}

struct OutputSlot {
    id: u64,
    device: String,
    source: Arc<Mutex<OutputSource>>,
}

struct State {
    inputs: Vec<String>,
    outputs: Vec<String>,
    default_input: String,
    default_output: String,
    input: Option<InputSlot>,
    output: Option<OutputSlot>,
    next: u64,
    opened_inputs: usize,
    opened_outputs: usize,
    /// Opening a device blocks while this is set (a device that hangs).
    hold_opens: bool,
}

/// The fake host: cheap to clone, every clone is the same machine.
#[derive(Clone)]
pub struct FakeHost(Arc<Mutex<State>>);

struct Token {
    state: Arc<Mutex<State>>,
    id: u64,
}

impl Running for Token {}

impl Drop for Token {
    fn drop(&mut self) {
        let mut s = self.state.lock().unwrap_or_else(|p| p.into_inner());
        if s.input.as_ref().is_some_and(|i| i.id == self.id) {
            s.input = None;
        }
        if s.output.as_ref().is_some_and(|o| o.id == self.id) {
            s.output = None;
        }
    }
}

fn dev(id: &str) -> Device {
    Device {
        id: id.into(),
        name: id.into(),
    }
}

impl FakeHost {
    /// A machine with a USB headset and a built-in microphone (the default), and two speakers.
    pub fn new() -> FakeHost {
        FakeHost(Arc::new(Mutex::new(State {
            inputs: vec!["usb-headset".into(), "builtin-mic".into()],
            outputs: vec!["usb-headset".into(), "speakers".into()],
            default_input: "builtin-mic".into(),
            default_output: "speakers".into(),
            input: None,
            output: None,
            next: 1,
            opened_inputs: 0,
            opened_outputs: 0,
            hold_opens: false,
        })))
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        self.0.lock().unwrap_or_else(|p| p.into_inner())
    }

    /// From now on opening a device blocks until [`release_opens`](Self::release_opens): a device that hangs.
    pub fn hold_opens(&self) {
        self.lock().hold_opens = true;
    }

    pub fn release_opens(&self) {
        self.lock().hold_opens = false;
    }

    fn wait_while_held(&self) {
        while self.lock().hold_opens {
            std::thread::sleep(std::time::Duration::from_millis(5));
        }
    }

    /// Delivers microphone samples to whoever is capturing (nothing if nobody is).
    pub fn push(&self, samples: &[i16]) {
        let sink = self.lock().input.as_ref().map(|i| i.sink.clone());
        if let Some(sink) = sink {
            (sink.lock().unwrap_or_else(|p| p.into_inner()))(samples);
        }
    }

    /// What the speaker plays next: `n` samples (silence when nothing plays).
    pub fn pull(&self, n: usize) -> Vec<i16> {
        let source = self.lock().output.as_ref().map(|o| o.source.clone());
        let mut out = vec![0i16; n];
        if let Some(source) = source {
            (source.lock().unwrap_or_else(|p| p.into_inner()))(&mut out);
        }
        out
    }

    /// The microphone capturing now, or `None`.
    pub fn capturing_from(&self) -> Option<String> {
        self.lock().input.as_ref().map(|i| i.device.clone())
    }

    /// The speaker playing now, or `None`.
    pub fn playing_on(&self) -> Option<String> {
        self.lock().output.as_ref().map(|o| o.device.clone())
    }

    /// Whether a microphone stream is open.
    pub fn inputs_open(&self) -> usize {
        usize::from(self.lock().input.is_some())
    }

    /// Whether a speaker stream is open.
    pub fn outputs_open(&self) -> usize {
        usize::from(self.lock().output.is_some())
    }

    /// How many microphone streams were ever opened.
    pub fn inputs_opened(&self) -> usize {
        self.lock().opened_inputs
    }

    /// How many speaker streams were ever opened.
    pub fn outputs_opened(&self) -> usize {
        self.lock().opened_outputs
    }

    /// Pulls an input device out: it leaves the list and a stream on it fails.
    pub fn unplug_input(&self, id: &str) {
        let lost = {
            let mut s = self.lock();
            s.inputs.retain(|d| d != id);
            match s.input.as_mut() {
                Some(i) if i.device == id => i.lost.take(),
                _ => None,
            }
        };
        if let Some(f) = lost {
            f();
        }
    }

    /// The machine has no default microphone any more.
    pub fn remove_default_input(&self) {
        self.lock().default_input.clear();
    }

    /// Plugs an input device in.
    pub fn plug_input(&self, id: &str) {
        let mut s = self.lock();
        if !s.inputs.iter().any(|d| d == id) {
            s.inputs.push(id.into());
        }
    }
}

impl Default for FakeHost {
    fn default() -> Self {
        Self::new()
    }
}

impl AudioHost for FakeHost {
    fn list(&self) -> DeviceList {
        let s = self.lock();
        DeviceList {
            inputs: s.inputs.iter().map(|d| dev(d)).collect(),
            outputs: s.outputs.iter().map(|d| dev(d)).collect(),
            default_input: (!s.default_input.is_empty()).then(|| dev(&s.default_input)),
            default_output: (!s.default_output.is_empty()).then(|| dev(&s.default_output)),
        }
    }

    fn open_input(
        &self,
        device: Option<&DeviceId>,
        sink: InputSink,
        on_lost: OnLost,
    ) -> Result<Box<dyn Running>, MediaError> {
        self.wait_while_held();
        let mut s = self.lock();
        let name = match device {
            Some(d) => s.inputs.iter().find(|i| *i == d.as_str()).cloned(),
            None => Some(s.default_input.clone()).filter(|d| !d.is_empty()),
        };
        let Some(name) = name else {
            return Err(MediaError::Audio("microphone not found".into()));
        };
        let id = s.next;
        s.next += 1;
        s.opened_inputs += 1;
        s.input = Some(InputSlot {
            id,
            device: name,
            sink: Arc::new(Mutex::new(sink)),
            lost: Some(on_lost),
        });
        Ok(Box::new(Token {
            state: self.0.clone(),
            id,
        }))
    }

    fn open_output(
        &self,
        device: Option<&DeviceId>,
        source: OutputSource,
        _on_lost: OnLost,
    ) -> Result<Box<dyn Running>, MediaError> {
        let mut s = self.lock();
        let name = match device {
            Some(d) => s.outputs.iter().find(|o| *o == d.as_str()).cloned(),
            None => Some(s.default_output.clone()).filter(|d| !d.is_empty()),
        };
        let Some(name) = name else {
            return Err(MediaError::Audio("speaker not found".into()));
        };
        let id = s.next;
        s.next += 1;
        s.opened_outputs += 1;
        s.output = Some(OutputSlot {
            id,
            device: name,
            source: Arc::new(Mutex::new(source)),
        });
        Ok(Box::new(Token {
            state: self.0.clone(),
            id,
        }))
    }
}
