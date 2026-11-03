#pragma once
#include <cstdint>
#include <map>

namespace application_audio {
struct ProcessBirth {
    uint32_t pid = 0, parent = 0;
    uint64_t identity = 0, interruptTime = 0;
    bool live = false, known = false;
};
// A genuine live parent cannot be born after its child on the interrupt clock.
// Unknown, equal-time, exited, or mismatched identities keep the legacy edge.
inline bool impossible_parent(uint32_t childPid, uint32_t snapshotParent, const ProcessBirth& child, const ProcessBirth& parent) {
    return child.known && parent.known && child.live && parent.live
        && child.pid == childPid && parent.pid == snapshotParent && child.parent == snapshotParent
        && child.identity && parent.identity && child.interruptTime && parent.interruptTime
        && parent.interruptTime > child.interruptTime;
}

template<class Lookup>
bool descendant(uint32_t pid, uint32_t root, const std::map<uint32_t, uint32_t>& graph, Lookup lookup) {
    for (int limit = 0; pid && limit < 128; ++limit) {
        if (pid == root) return true;
        auto edge = graph.find(pid);
        if (edge == graph.end() || edge->second == pid) break;
        if (impossible_parent(pid, edge->second, lookup(pid), lookup(edge->second))) break;
        pid = edge->second;
    }
    return false;
}
}
