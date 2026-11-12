//! The microphone's noise suppression, the desktop's counterpart of the web client's Voice settings
//! (experiments/noise/README.md, e2e/voice/enhanced-noise.spec.ts):
//!
//! - [`NoiseMode::Off`]: the samples pass unchanged.
//! - [`NoiseMode::Standard`]: libwebrtc's own noise suppressor (the audio processing module LiveKit
//!   ships; on a device microphone the ADM's software or hardware suppressor).
//! - [`NoiseMode::Enhanced`]: DeepFilterNet3 through `deep-filter-rt` (MIT OR Apache-2.0, the model
//!   embedded; its weights are licensed like the code since upstream 7b14b89, NOTICE.md of the
//!   crate), full band, 10 ms frames at 48 kHz, about 30 ms of added delay.
//!
//! Every mode takes and gives 10 ms of 48 kHz mono 16-bit samples ([`FRAME_SAMPLES`]), the frame
//! LiveKit's audio source takes.

use deep_filter_rt::tract::DfTract;
use livekit::webrtc::native::apm::AudioProcessingModule;
use ndarray::Array2;

use crate::MediaError;

pub const RATE: u32 = 48_000;
/// 10 ms of mono audio at 48 kHz.
pub const FRAME_SAMPLES: usize = (RATE / 100) as usize;
/// The enhanced processor's per-frame budget the web client's evaluation enforces (p99 inference
/// under 8 ms of the 10 ms frame, e2e/voice/enhanced-noise.spec.ts).
pub const ENHANCED_P99_BUDGET_MS: f64 = 8.0;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub enum NoiseMode {
    Off,
    #[default]
    Standard,
    Enhanced,
}

impl NoiseMode {
    pub const ALL: [NoiseMode; 3] = [NoiseMode::Off, NoiseMode::Standard, NoiseMode::Enhanced];

    /// The processor's name as the settings show it.
    pub fn processor(self) -> &'static str {
        match self {
            NoiseMode::Off => "off",
            NoiseMode::Standard => "WebRTC",
            NoiseMode::Enhanced => "DeepFilterNet3",
        }
    }
}

enum Engine {
    Off,
    Standard(AudioProcessingModule),
    Enhanced {
        df: Box<DfTract>,
        input: Array2<f32>,
        output: Array2<f32>,
    },
}

/// One microphone's noise processor.
pub struct NoiseProcessor {
    mode: NoiseMode,
    engine: Engine,
}

impl NoiseProcessor {
    pub fn new(mode: NoiseMode) -> Result<Self, MediaError> {
        let engine = match mode {
            NoiseMode::Off => Engine::Off,
            // Noise suppression alone: echo cancellation and gain control belong to the device path.
            NoiseMode::Standard => {
                Engine::Standard(AudioProcessingModule::new(false, false, false, true))
            }
            NoiseMode::Enhanced => {
                let df = DfTract::default();
                if df.sr != RATE as usize || df.hop_size != FRAME_SAMPLES || df.ch != 1 {
                    return Err(MediaError::Audio(format!(
                        "DeepFilterNet3 runs at {} Hz, {} samples, {} channels",
                        df.sr, df.hop_size, df.ch
                    )));
                }
                Engine::Enhanced {
                    input: Array2::zeros((1, FRAME_SAMPLES)),
                    output: Array2::zeros((1, FRAME_SAMPLES)),
                    df: Box::new(df),
                }
            }
        };
        Ok(Self { mode, engine })
    }

    pub fn mode(&self) -> NoiseMode {
        self.mode
    }

    /// Processes one 10 ms frame in place. A frame of another length, or a processor failure, is
    /// an error and the frame is silenced: never a half-processed or unprocessed frame in a mode
    /// that promised suppression.
    pub fn process(&mut self, frame: &mut [i16]) -> Result<(), MediaError> {
        if frame.len() != FRAME_SAMPLES {
            frame.fill(0);
            return Err(MediaError::Audio(format!(
                "a frame is {FRAME_SAMPLES} samples, not {}",
                frame.len()
            )));
        }
        let result = match &mut self.engine {
            Engine::Off => Ok(()),
            Engine::Standard(apm) => apm
                .process_stream(frame, RATE as i32, 1)
                .map_err(|e| MediaError::Audio(e.message)),
            Engine::Enhanced { df, input, output } => {
                for (d, s) in input.iter_mut().zip(frame.iter()) {
                    *d = f32::from(*s) / 32768.0;
                }
                match df.process(input.view(), output.view_mut()) {
                    Ok(_) => {
                        for (d, s) in frame.iter_mut().zip(output.iter()) {
                            *d = (s.clamp(-1.0, 1.0) * 32767.0) as i16;
                        }
                        Ok(())
                    }
                    Err(e) => Err(MediaError::Audio(e.to_string())),
                }
            }
        };
        if result.is_err() {
            frame.fill(0);
        }
        result
    }
}
