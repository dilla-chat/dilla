// Dilla application-only capture. No sink/system-monitor capture or automatic fallback.
#include "common.hpp"
#include <pipewire/pipewire.h>
#include <spa/param/audio/format-utils.h>
#include <unistd.h>
#include <signal.h>
#include <map>
#include <set>
#include <vector>
#include <fstream>
#include <sstream>
#include <algorithm>
#include <memory>
struct Node { uint32_t id; uint64_t serial; uint32_t pid; uint32_t client; std::string label; };
struct Port { uint32_t id, node; bool output; std::string channel; };
struct Capture;
struct Client { Capture* capture; uint32_t id; pw_client* proxy; spa_hook hook{}; uint32_t pid = 0; };
struct Capture {
    pw_main_loop* loop = nullptr; pw_context* context = nullptr; pw_core* core = nullptr;
    pw_registry* registry = nullptr; pw_stream* stream = nullptr;
    spa_hook registryHook{}, coreHook{}, streamHook{};
    std::map<uint32_t,std::unique_ptr<Client>> clients; std::map<uint32_t,Node> nodes; std::map<uint32_t,Port> ports;
    std::set<std::pair<uint32_t,uint32_t>> linked; std::vector<pw_proxy*> links;
    uint32_t excluded = 0, own = PW_ID_ANY, selectedPid = 0; uint64_t selectedSerial = 0;
    int sync = 0; bool listing = true; bool found = false; bool failed = false;
};
static bool excluded(uint32_t pid, uint32_t root) {
    // The audio service may run in a Chromium utility child, so exclude the entire app process tree.
    for (int limit = 0; pid > 1 && limit < 128; ++limit) {
        if (pid == root) return true;
        std::ifstream file("/proc/" + std::to_string(pid) + "/stat"); std::string line; std::getline(file, line);
        const auto end = line.rfind(')'); if (end == std::string::npos) break;
        std::istringstream fields(line.substr(end + 2)); char state; uint32_t parent = 0; fields >> state >> parent;
        if (parent == pid) break;
        pid = parent;
    }
    return false;
}
static void linkPorts(Capture& c) {
    if (c.listing || c.own == PW_ID_ANY || !c.selectedPid) return;
    std::vector<Port> inputs;
    for (const auto& [id, port] : c.ports) if (!port.output && port.node == c.own) inputs.push_back(port);
    for (const auto& [id, out] : c.ports) {
        const auto node = c.nodes.find(out.node);
        if (!out.output || node == c.nodes.end() || node->second.pid != c.selectedPid) continue;
        for (const auto& in : inputs) {
            if (out.channel != in.channel && out.channel != "MONO" && !out.channel.empty()) continue;
            if (!c.linked.insert({out.id, in.id}).second) continue;
            auto props = pw_properties_new(nullptr, nullptr);
            pw_properties_setf(props, PW_KEY_LINK_OUTPUT_NODE, "%u", out.node);
            pw_properties_setf(props, PW_KEY_LINK_OUTPUT_PORT, "%u", out.id);
            pw_properties_setf(props, PW_KEY_LINK_INPUT_NODE, "%u", in.node);
            pw_properties_setf(props, PW_KEY_LINK_INPUT_PORT, "%u", in.id);
            pw_properties_set(props, PW_KEY_OBJECT_LINGER, "false");
            auto proxy = static_cast<pw_proxy*>(pw_core_create_object(c.core, "link-factory", PW_TYPE_INTERFACE_Link, PW_VERSION_LINK, &props->dict, 0));
            pw_properties_free(props);
            if (!proxy) { c.failed = true; pw_main_loop_quit(c.loop); return; }
            c.links.push_back(proxy);
        }
    }
}
static void clientInfo(void* data, const pw_client_info* info) {
    auto& client = *static_cast<Client*>(data); if (!info->props) return;
    const auto text = spa_dict_lookup(info->props, PW_KEY_APP_PROCESS_ID); if (!text) return;
    try { auto pid = number(text); if (pid > UINT32_MAX) return; client.pid = uint32_t(pid); } catch (...) { return; }
    auto& c = *client.capture;
    for (auto& [id, node] : c.nodes) if (node.client == client.id && !node.pid) {
        if (excluded(client.pid, c.excluded)) continue;
        node.pid = client.pid;
        if (node.pid && node.serial == c.selectedSerial) { c.selectedPid = node.pid; c.found = true; }
    }
    linkPorts(c);
}
static void global(void* data, uint32_t id, uint32_t, const char* type, uint32_t, const spa_dict* props) {
    auto& c = *static_cast<Capture*>(data); if (!props) return;
    auto get = [&](const char* key) -> const char* { auto value = spa_dict_lookup(props, key); return value ? value : ""; };
    if (!std::strcmp(type, PW_TYPE_INTERFACE_Node) && !std::strcmp(get(PW_KEY_NODE_NAME), "dilla-application-capture")) {
        c.own = id;
    } else if (!std::strcmp(type, PW_TYPE_INTERFACE_Client)) {
        auto client = std::make_unique<Client>(); client->capture = &c; client->id = id;
        client->proxy = static_cast<pw_client*>(pw_registry_bind(c.registry, id, type, PW_VERSION_CLIENT, 0));
        if (!client->proxy) return;
        static const pw_client_events events = [] { pw_client_events e{}; e.version = PW_VERSION_CLIENT_EVENTS; e.info = clientInfo; return e; }();
        pw_client_add_listener(client->proxy, &client->hook, &events, client.get()); c.clients[id] = std::move(client);
    } else if (!std::strcmp(type, PW_TYPE_INTERFACE_Node) && !std::strcmp(get(PW_KEY_MEDIA_CLASS), "Stream/Output/Audio")) {
        try {
            auto client = uint32_t(std::strtoul(get(PW_KEY_CLIENT_ID), nullptr, 10));
            uint64_t pid64 = 0;
            if (*get(PW_KEY_APP_PROCESS_ID)) pid64 = number(get(PW_KEY_APP_PROCESS_ID));
            else if (c.clients.count(client)) pid64 = c.clients[client]->pid;
            if (pid64 > UINT32_MAX || (pid64 && excluded(uint32_t(pid64), c.excluded))) return;
            Node node{id, number(get(PW_KEY_OBJECT_SERIAL)), uint32_t(pid64), client, get(PW_KEY_APP_NAME)};
            if (node.label.empty()) node.label = get(PW_KEY_NODE_DESCRIPTION);
            if (node.label.empty()) node.label = "Application " + std::to_string(node.pid);
            if (node.label.size() > 256) node.label.resize(256);
            c.nodes[id] = node;
            if (node.pid && node.serial == c.selectedSerial) { c.selectedPid = node.pid; c.found = true; }
        } catch (...) { return; }
    } else if (!std::strcmp(type, PW_TYPE_INTERFACE_Port)) {
        const char* node = get(PW_KEY_NODE_ID); if (!*node) return;
        c.ports[id] = {id, uint32_t(std::strtoul(node, nullptr, 10)), !std::strcmp(get(PW_KEY_PORT_DIRECTION), "out"), get(PW_KEY_AUDIO_CHANNEL)};
    }
    linkPorts(c);
}
static void removed(void* data, uint32_t id) {
    auto& c = *static_cast<Capture*>(data);
    const auto node = c.nodes.find(id);
    if (node != c.nodes.end() && node->second.serial == c.selectedSerial && !c.listing) pw_main_loop_quit(c.loop);
    c.nodes.erase(id); c.ports.erase(id);
}
static void coreDone(void* data, uint32_t id, int seq) {
    auto& c = *static_cast<Capture*>(data);
    if (id == PW_ID_CORE && seq == c.sync) pw_main_loop_quit(c.loop);
}
static void coreError(void* data, uint32_t, int, int result, const char*) {
    auto& c = *static_cast<Capture*>(data);
    if (result < 0) { c.failed = true; pw_main_loop_quit(c.loop); }
}
static void streamState(void* data, pw_stream_state, pw_stream_state state, const char*) {
    auto& c = *static_cast<Capture*>(data);
    if (state == PW_STREAM_STATE_ERROR) { c.failed = true; pw_main_loop_quit(c.loop); }
    if (state == PW_STREAM_STATE_PAUSED || state == PW_STREAM_STATE_STREAMING) { c.own = pw_stream_get_node_id(c.stream); linkPorts(c); }
}
static void process(void* data) {
    auto& c = *static_cast<Capture*>(data); auto b = pw_stream_dequeue_buffer(c.stream); if (!b) return;
    const auto& samples = b->buffer->datas[0];
    if (samples.data && samples.chunk && samples.chunk->size <= samples.maxsize && samples.chunk->offset <= samples.maxsize - samples.chunk->size) {
        auto p = static_cast<const unsigned char*>(samples.data) + samples.chunk->offset;
        auto n = size_t(samples.chunk->size);
        while (n) { auto sent = ::write(STDOUT_FILENO, p, n); if (sent < 0 && errno == EINTR) continue; if (sent <= 0) _exit(1); p += sent; n -= size_t(sent); }
    }
    pw_stream_queue_buffer(c.stream, b);
}
static void quit(void* data, int) { pw_main_loop_quit(static_cast<Capture*>(data)->loop); }
int main(int argc, char** argv) {
    try {
        if (argc != 3 && argc != 4) throw std::runtime_error("usage: list excluded-pid | capture serial:pid excluded-pid");
        Capture c; c.listing = !std::strcmp(argv[1], "list");
        if ((!c.listing && std::strcmp(argv[1], "capture")) || (c.listing ? argc != 3 : argc != 4)) throw std::runtime_error("invalid command");
        c.excluded = uint32_t(number(argv[argc - 1]));
        uint32_t expectedPid = 0;
        if (!c.listing) { std::string id = argv[2]; auto colon = id.find(':'); if (colon == std::string::npos) throw std::runtime_error("invalid source"); c.selectedSerial = number(id.substr(0, colon).c_str()); expectedPid = uint32_t(number(id.substr(colon + 1).c_str())); }
        pw_init(nullptr, nullptr); c.loop = pw_main_loop_new(nullptr);
        c.context = pw_context_new(pw_main_loop_get_loop(c.loop), nullptr, 0); c.core = pw_context_connect(c.context, nullptr, 0);
        if (!c.core) throw std::runtime_error("PipeWire connection refused");
        pw_core_events ce{}; ce.version = PW_VERSION_CORE_EVENTS; ce.done = coreDone; ce.error = coreError;
        pw_core_add_listener(c.core, &c.coreHook, &ce, &c);
        c.registry = pw_core_get_registry(c.core, PW_VERSION_REGISTRY, 0);
        pw_registry_events re{}; re.version = PW_VERSION_REGISTRY_EVENTS; re.global = global; re.global_remove = removed;
        pw_registry_add_listener(c.registry, &c.registryHook, &re, &c);
        c.sync = pw_core_sync(c.core, PW_ID_CORE, 0); pw_main_loop_run(c.loop);
        c.sync = pw_core_sync(c.core, PW_ID_CORE, c.sync); pw_main_loop_run(c.loop);
        if (c.failed) throw std::runtime_error("PipeWire enumeration failed");
        if (c.listing) {
            std::printf("["); bool first = true;
            for (const auto& [id, node] : c.nodes) { if (!node.pid || excluded(node.pid, c.excluded)) continue; if (!first) std::printf(","); first = false; auto key = std::to_string(node.serial) + ':' + std::to_string(node.pid); std::printf("{\"id\":%s,\"label\":%s}", quote(key).c_str(), quote(node.label).c_str()); }
            std::printf("]\n");
        } else {
            if (!c.found || c.selectedPid != expectedPid) throw std::runtime_error("selected application is no longer available");
            auto props = pw_properties_new(PW_KEY_APP_NAME, "Dilla application capture", PW_KEY_MEDIA_TYPE, "Audio", PW_KEY_MEDIA_CATEGORY, "Capture", PW_KEY_MEDIA_CLASS, "Stream/Input/Audio", PW_KEY_NODE_NAME, "dilla-application-capture", PW_KEY_NODE_DONT_RECONNECT, "true", nullptr);
            c.stream = pw_stream_new(c.core, "Dilla application capture", props);
            pw_stream_events se{}; se.version = PW_VERSION_STREAM_EVENTS; se.state_changed = streamState; se.process = process;
            pw_stream_add_listener(c.stream, &c.streamHook, &se, &c);
            uint8_t buffer[1024]; spa_pod_builder builder = SPA_POD_BUILDER_INIT(buffer, sizeof buffer);
            spa_audio_info_raw format{}; format.format = SPA_AUDIO_FORMAT_F32_LE; format.rate = 48000; format.channels = 2; format.position[0] = SPA_AUDIO_CHANNEL_FL; format.position[1] = SPA_AUDIO_CHANNEL_FR;
            const spa_pod* params[] = {spa_format_audio_raw_build(&builder, SPA_PARAM_EnumFormat, &format)};
            if (pw_stream_connect(c.stream, PW_DIRECTION_INPUT, PW_ID_ANY, PW_STREAM_FLAG_MAP_BUFFERS, params, 1) < 0) throw std::runtime_error("application capture refused");
            pw_loop_add_signal(pw_main_loop_get_loop(c.loop), SIGTERM, quit, &c); pw_loop_add_signal(pw_main_loop_get_loop(c.loop), SIGINT, quit, &c);
            pw_main_loop_run(c.loop);
            for (auto link : c.links) pw_proxy_destroy(link);
            pw_stream_destroy(c.stream);
        }
        pw_proxy_destroy(reinterpret_cast<pw_proxy*>(c.registry)); pw_core_disconnect(c.core); pw_context_destroy(c.context); pw_main_loop_destroy(c.loop); pw_deinit();
        return c.failed ? 1 : 0;
    } catch (const std::exception& e) { return failure(e); }
}
