//! Application audio on Linux through PipeWire (beta.5 `native/linux.cpp`, in-process).
//!
//! Listing: the registry's `Stream/Output/Audio` nodes, each with its process (the node's `application.process.id`,
//! else its client's), one entry per process, Dilla's own process tree left out.
//!
//! Capture: a stream named `dilla-application-capture` that never autoconnects (so it is never linked to a microphone, a
//! sink or a monitor) takes 48 kHz stereo f32; each output port of the chosen process's playback nodes is linked to the
//! stream's input port of the same channel (a mono port to both), including nodes the process opens later. Links are
//! not lingering: they go with this client. The chosen stream (by serial) going away ends the capture, as beta.5.

use std::cell::{Cell, RefCell};
use std::collections::{BTreeSet, HashMap};
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use pipewire as pw;
use pw::spa;
use pw::types::ObjectType;

use super::{
    AppAudioError, AppCapture, AppSource, CHANNELS, PcmFrames, RATE, in_tree, parent_from_stat,
};

const NODE_NAME: &str = "dilla-application-capture";

fn parent(pid: u32) -> Option<u32> {
    std::fs::read_to_string(format!("/proc/{pid}/stat"))
        .ok()
        .and_then(|s| parent_from_stat(&s))
}

fn excluded(pid: u32, root: u32) -> bool {
    in_tree(pid, root, parent)
}

#[derive(Clone, Debug)]
struct Node {
    serial: u64,
    pid: u32,
    client: u32,
    label: String,
}

#[derive(Clone, Debug)]
struct Port {
    node: u32,
    output: bool,
    channel: String,
}

/// What the registry said so far.
#[derive(Default)]
struct Graph {
    nodes: HashMap<u32, Node>,
    ports: HashMap<u32, Port>,
    client_pids: HashMap<u32, u32>,
}

impl Graph {
    fn global(&mut self, id: u32, kind: &ObjectType, props: &spa::utils::dict::DictRef) {
        let num = |k: &str| props.get(k).and_then(|v| v.parse::<u64>().ok());
        match kind {
            ObjectType::Client => {
                if let Some(pid) = num("application.process.id").or_else(|| num("pipewire.sec.pid"))
                {
                    let pid = u32::try_from(pid).unwrap_or(0);
                    self.client_pids.insert(id, pid);
                    for n in self.nodes.values_mut() {
                        if n.client == id && n.pid == 0 {
                            n.pid = pid;
                        }
                    }
                }
            }
            ObjectType::Node if props.get("media.class") == Some("Stream/Output/Audio") => {
                let client = num("client.id")
                    .and_then(|c| u32::try_from(c).ok())
                    .unwrap_or(0);
                let pid = num("application.process.id")
                    .and_then(|p| u32::try_from(p).ok())
                    .or_else(|| self.client_pids.get(&client).copied())
                    .unwrap_or(0);
                let label = props
                    .get("application.name")
                    .or_else(|| props.get("node.description"))
                    .unwrap_or("")
                    .to_owned();
                self.nodes.insert(
                    id,
                    Node {
                        serial: num("object.serial").unwrap_or(0),
                        pid,
                        client,
                        label,
                    },
                );
            }
            ObjectType::Port => {
                if let Some(node) = num("node.id").and_then(|n| u32::try_from(n).ok()) {
                    self.ports.insert(
                        id,
                        Port {
                            node,
                            output: props.get("port.direction") == Some("out"),
                            channel: props.get("audio.channel").unwrap_or("").to_owned(),
                        },
                    );
                }
            }
            _ => {}
        }
    }

    /// One source per process that plays, Dilla's tree (`root`) left out; the lowest serial names it.
    fn sources(&self, root: u32) -> Vec<AppSource> {
        let mut by_pid: HashMap<u32, AppSource> = HashMap::new();
        let mut excluded_pids = BTreeSet::new();
        for n in self.nodes.values() {
            if n.pid == 0 || excluded_pids.contains(&n.pid) {
                continue;
            }
            if excluded(n.pid, root) {
                excluded_pids.insert(n.pid);
                continue;
            }
            let e = by_pid.entry(n.pid).or_insert_with(|| AppSource {
                serial: n.serial,
                pid: n.pid,
                label: n.label.clone(),
            });
            if n.serial < e.serial {
                e.serial = n.serial;
                if !n.label.is_empty() {
                    e.label = n.label.clone();
                }
            }
        }
        let mut out: Vec<AppSource> = by_pid.into_values().collect();
        out.sort_by_key(|s| s.serial);
        out
    }

    /// The links to make from the chosen process's output ports to our stream's input ports (`own`): same channel, a mono
    /// or unnamed port to every input.
    fn wanted_links(&self, pid: u32, own: u32) -> Vec<(u32, u32, u32, u32)> {
        let inputs: Vec<(&u32, &Port)> = self
            .ports
            .iter()
            .filter(|(_, p)| !p.output && p.node == own)
            .collect();
        let mut out = Vec::new();
        for (out_id, port) in &self.ports {
            let Some(node) = self.nodes.get(&port.node) else {
                continue;
            };
            if !port.output || node.pid != pid {
                continue;
            }
            for (in_id, input) in &inputs {
                if port.channel != input.channel
                    && port.channel != "MONO"
                    && !port.channel.is_empty()
                {
                    continue;
                }
                out.push((port.node, *out_id, own, **in_id));
            }
        }
        out.sort_unstable();
        out
    }
}

// The prebuilt libwebrtc linked into this binary carries weak stubs for the libpipewire functions its own screen capture
// uses (`pipewire_stubs.o`: `pw_init`, `pw_stream_*`, `pw_context_*`, ...), and the linker binds those names to the stubs,
// not to libpipewire: until libwebrtc loads libpipewire into them, calling one jumps to null. Its loader is called once
// before any PipeWire call here; the stubs then forward to the same libpipewire everything else of this module calls.
#[allow(unsafe_code)]
mod stubs {
    unsafe extern "C" {
        /// `bool webrtc::InitializePipeWire()` (modules/portal/pipewire_utils.cc): loads `libpipewire-0.3.so.0` into
        /// the stubs, once; false when it is not there.
        #[link_name = "_ZN6webrtc18InitializePipeWireEv"]
        fn webrtc_initialize_pipewire() -> bool;
    }

    pub(super) fn load() -> bool {
        // SAFETY: a C++ function of the linked libwebrtc taking nothing and returning `bool` (one byte, the C ABI's
        // `_Bool` on the Linux targets), thread-safe (a function-local static initialised once).
        unsafe { webrtc_initialize_pipewire() }
    }
}

/// libpipewire, loaded into libwebrtc's stubs and initialised.
fn init() -> Result<(), AppAudioError> {
    static LOADED: std::sync::OnceLock<bool> = std::sync::OnceLock::new();
    if !*LOADED.get_or_init(stubs::load) {
        return Err(AppAudioError::Server(
            "libpipewire is not available".to_owned(),
        ));
    }
    pw::init();
    Ok(())
}

fn server(e: impl std::fmt::Display) -> AppAudioError {
    AppAudioError::Server(e.to_string())
}

/// Runs `mainloop` until the core answered everything asked so far, or 5 s.
fn roundtrip(
    mainloop: &pw::main_loop::MainLoopRc,
    core: &pw::core::CoreRc,
) -> Result<(), AppAudioError> {
    let done = Rc::new(Cell::new(false));
    let pending = core.sync(0).map_err(server)?;
    let (d, l) = (done.clone(), mainloop.clone());
    let _listener = core
        .add_listener_local()
        .done(move |id, seq| {
            if id == pw::core::PW_ID_CORE && seq == pending {
                d.set(true);
                l.quit();
            }
        })
        .register();
    let l = mainloop.clone();
    let timer = mainloop.loop_().add_timer(move |_| l.quit());
    let _ = timer.update_timer(Some(Duration::from_secs(5)), None);
    mainloop.run();
    if done.get() {
        Ok(())
    } else {
        Err(AppAudioError::Server("no answer".to_owned()))
    }
}

pub(super) fn sources(root: u32) -> Result<Vec<AppSource>, AppAudioError> {
    init()?;
    let mainloop = pw::main_loop::MainLoopRc::new(None).map_err(server)?;
    let context = pw::context::ContextRc::new(&mainloop, None).map_err(server)?;
    let core = context.connect_rc(None).map_err(server)?;
    let registry = core.get_registry().map_err(server)?;
    let graph = Rc::new(RefCell::new(Graph::default()));
    let g = graph.clone();
    let _listener = registry
        .add_listener_local()
        .global(move |global| {
            if let Some(props) = global.props {
                g.borrow_mut().global(global.id, &global.type_, props);
            }
        })
        .register();
    roundtrip(&mainloop, &core)?;
    let out = graph.borrow().sources(root);
    Ok(out)
}

pub(super) fn start(
    source: &AppSource,
    root: u32,
    frames: Box<dyn FnMut(Vec<f32>) + Send>,
    ended: Box<dyn FnOnce() + Send>,
) -> Result<AppCapture, AppAudioError> {
    if excluded(source.pid, root) {
        return Err(AppAudioError::Gone);
    }
    let stop = Arc::new(AtomicBool::new(false));
    let (ready_tx, ready_rx) = std::sync::mpsc::channel();
    let (source_c, stop_c) = (source.clone(), stop.clone());
    std::thread::Builder::new()
        .name("dilla-app-audio".to_owned())
        .spawn(move || {
            let result = run(&source_c, root, frames, stop_c.clone(), &ready_tx);
            let stopped = stop_c.load(Ordering::Acquire);
            match result {
                Err(e) => {
                    let _ = ready_tx.send(Err(e));
                }
                Ok(()) if !stopped => ended(),
                Ok(()) => {}
            }
        })
        .map_err(server)?;
    match ready_rx.recv_timeout(Duration::from_secs(10)) {
        Ok(Ok(())) => Ok(AppCapture::new(stop, source.label.clone())),
        Ok(Err(e)) => Err(e),
        Err(_) => {
            stop.store(true, Ordering::Release);
            Err(AppAudioError::Server(
                "the capture did not start".to_owned(),
            ))
        }
    }
}

/// The capture thread: until `stop`, the chosen stream going away, or an error.
fn run(
    source: &AppSource,
    root: u32,
    mut frames: Box<dyn FnMut(Vec<f32>) + Send>,
    stop: Arc<AtomicBool>,
    ready: &std::sync::mpsc::Sender<Result<(), AppAudioError>>,
) -> Result<(), AppAudioError> {
    init()?;
    let mainloop = pw::main_loop::MainLoopRc::new(None).map_err(server)?;
    let context = pw::context::ContextRc::new(&mainloop, None).map_err(server)?;
    let core = context.connect_rc(None).map_err(server)?;
    let registry = core.get_registry_rc().map_err(server)?;
    let graph = Rc::new(RefCell::new(Graph::default()));
    let own = Rc::new(Cell::new(None::<u32>));
    let linked = Rc::new(RefCell::new(BTreeSet::<(u32, u32, u32, u32)>::new()));
    let links: Rc<RefCell<Vec<pw::link::Link>>> = Rc::default();
    let failed = Rc::new(Cell::new(None::<AppAudioError>));
    let (pid, serial) = (source.pid, source.serial);

    // Links every wanted (output, input) pair not linked yet.
    let link_all = {
        let (graph, own, linked, links, core, failed, mainloop) = (
            graph.clone(),
            own.clone(),
            linked.clone(),
            links.clone(),
            core.clone(),
            failed.clone(),
            mainloop.clone(),
        );
        Rc::new(move || {
            let Some(own) = own.get() else { return };
            let wanted = graph.borrow().wanted_links(pid, own);
            for (out_node, out_port, in_node, in_port) in wanted {
                if !linked
                    .borrow_mut()
                    .insert((out_node, out_port, in_node, in_port))
                {
                    continue;
                }
                let props = pw::properties::properties! {
                    "link.output.node" => out_node.to_string(),
                    "link.output.port" => out_port.to_string(),
                    "link.input.node" => in_node.to_string(),
                    "link.input.port" => in_port.to_string(),
                    "object.linger" => "false",
                };
                match core.create_object::<pw::link::Link>("link-factory", &props) {
                    Ok(link) => links.borrow_mut().push(link),
                    Err(e) => {
                        failed.set(Some(server(e)));
                        mainloop.quit();
                        return;
                    }
                }
            }
        })
    };

    let _registry_listener = {
        let (graph, link_all) = (graph.clone(), link_all.clone());
        let (graph_rm, mainloop_rm) = (graph.clone(), mainloop.clone());
        registry
            .add_listener_local()
            .global(move |global| {
                if let Some(props) = global.props {
                    graph.borrow_mut().global(global.id, &global.type_, props);
                    // A node of Dilla's own tree is never linked, whatever pid it claims.
                    let mut g = graph.borrow_mut();
                    if let Some(n) = g.nodes.get(&global.id)
                        && n.pid != 0
                        && excluded(n.pid, root)
                    {
                        g.nodes.remove(&global.id);
                    }
                }
                link_all();
            })
            .global_remove(move |id| {
                let mut g = graph_rm.borrow_mut();
                if g.nodes.get(&id).is_some_and(|n| n.serial == serial) {
                    mainloop_rm.quit();
                }
                g.nodes.remove(&id);
                g.ports.remove(&id);
            })
            .register()
    };

    let stream = pw::stream::StreamRc::new(
        core.clone(),
        NODE_NAME,
        pw::properties::properties! {
            *pw::keys::MEDIA_TYPE => "Audio",
            *pw::keys::MEDIA_CATEGORY => "Capture",
            *pw::keys::MEDIA_ROLE => "Music",
            *pw::keys::NODE_NAME => NODE_NAME,
            "node.autoconnect" => "false",
            "node.dont-reconnect" => "true",
        },
    )
    .map_err(server)?;
    let pcm = Rc::new(RefCell::new(PcmFrames::default()));
    let _stream_listener = {
        let (own, link_all, failed, mainloop) = (
            own.clone(),
            link_all.clone(),
            failed.clone(),
            mainloop.clone(),
        );
        let (failed_p, mainloop_p) = (failed.clone(), mainloop.clone());
        stream
            .add_local_listener_with_user_data(())
            .state_changed(move |stream, _, _, state| match state {
                pw::stream::StreamState::Error(e) => {
                    failed.set(Some(AppAudioError::Server(e)));
                    mainloop.quit();
                }
                pw::stream::StreamState::Paused | pw::stream::StreamState::Streaming => {
                    own.set(Some(stream.node_id()));
                    link_all();
                }
                _ => {}
            })
            .process(move |stream, _| {
                let Some(mut buffer) = stream.dequeue_buffer() else {
                    return;
                };
                let datas = buffer.datas_mut();
                let Some(data) = datas.first_mut() else {
                    return;
                };
                let (offset, size) = (data.chunk().offset() as usize, data.chunk().size() as usize);
                let Some(bytes) = data.data() else { return };
                let Some(chunk) = offset
                    .checked_add(size)
                    .and_then(|end| bytes.get(offset..end))
                else {
                    return;
                };
                if let Err(e) = pcm.borrow_mut().push(chunk, &mut frames) {
                    failed_p.set(Some(e));
                    mainloop_p.quit();
                }
            })
            .register()
            .map_err(server)?
    };

    let mut info = spa::param::audio::AudioInfoRaw::new();
    info.set_format(spa::param::audio::AudioFormat::F32LE);
    info.set_rate(RATE);
    info.set_channels(CHANNELS);
    let mut position = [0u32; 64];
    position[0] = spa::sys::SPA_AUDIO_CHANNEL_FL;
    position[1] = spa::sys::SPA_AUDIO_CHANNEL_FR;
    info.set_position(position);
    let values: Vec<u8> = spa::pod::serialize::PodSerializer::serialize(
        std::io::Cursor::new(Vec::new()),
        &spa::pod::Value::Object(spa::pod::Object {
            type_: spa::utils::SpaTypes::ObjectParamFormat.as_raw(),
            id: spa::param::ParamType::EnumFormat.as_raw(),
            properties: info.into(),
        }),
    )
    .map_err(|e| AppAudioError::Server(format!("{e:?}")))?
    .0
    .into_inner();
    let mut params = [spa::pod::Pod::from_bytes(&values)
        .ok_or_else(|| AppAudioError::Server("format".to_owned()))?];
    stream
        .connect(
            spa::utils::Direction::Input,
            None,
            pw::stream::StreamFlags::MAP_BUFFERS,
            &mut params,
        )
        .map_err(server)?;

    // The chosen stream must be there: one roundtrip lists the graph.
    roundtrip(&mainloop, &core)?;
    let present = graph
        .borrow()
        .nodes
        .values()
        .any(|n| n.pid == pid && n.serial == serial);
    if !present {
        return Err(AppAudioError::Gone);
    }
    let _ = ready.send(Ok(()));

    // Every 100 ms: stop when asked.
    let l = mainloop.clone();
    let timer = mainloop.loop_().add_timer(move |_| {
        if stop.load(Ordering::Acquire) {
            l.quit();
        }
    });
    let _ = timer.update_timer(
        Some(Duration::from_millis(100)),
        Some(Duration::from_millis(100)),
    );
    mainloop.run();
    let _ = stream.disconnect();
    links.borrow_mut().clear();
    match failed.take() {
        Some(e) => Err(e),
        None => Ok(()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn port(node: u32, output: bool, channel: &str) -> Port {
        Port {
            node,
            output,
            channel: channel.to_owned(),
        }
    }

    fn node(serial: u64, pid: u32, label: &str) -> Node {
        Node {
            serial,
            pid,
            client: 0,
            label: label.to_owned(),
        }
    }

    /// Only the chosen process's output ports are linked, channel to channel (a mono port to both inputs); another
    /// application's ports and our own outputs never are.
    #[test]
    fn only_the_chosen_processs_ports_are_linked_by_channel() {
        let mut g = Graph::default();
        g.nodes.insert(10, node(100, 4000, "Music"));
        g.nodes.insert(11, node(101, 4000, "Music"));
        g.nodes.insert(20, node(200, 5000, "Other"));
        g.ports.insert(1, port(10, true, "FL"));
        g.ports.insert(2, port(10, true, "FR"));
        g.ports.insert(3, port(11, true, "MONO"));
        g.ports.insert(4, port(20, true, "FL"));
        // Our stream (node 99): two inputs.
        g.ports.insert(7, port(99, false, "FL"));
        g.ports.insert(8, port(99, false, "FR"));
        let links = g.wanted_links(4000, 99);
        assert_eq!(
            links,
            vec![
                (10, 1, 99, 7),
                (10, 2, 99, 8),
                (11, 3, 99, 7),
                (11, 3, 99, 8)
            ]
        );
        assert!(g.wanted_links(4000, 98).is_empty(), "no inputs, no links");
    }

    /// One source per process, named by its first stream; Dilla's own process (this test's) left out.
    #[test]
    fn sources_are_one_per_process_without_dilla() {
        let me = std::process::id();
        let mut g = Graph::default();
        g.nodes.insert(10, node(105, 4000, "Late"));
        g.nodes.insert(11, node(101, 4000, "Music"));
        g.nodes.insert(12, node(102, me, "Dilla"));
        g.nodes.insert(13, node(103, 0, "No pid"));
        let s = g.sources(me);
        assert_eq!(
            s,
            vec![AppSource {
                serial: 101,
                pid: 4000,
                label: "Music".to_owned()
            }]
        );
    }
}
