#pragma once
#include <cstdint>
#include <map>

namespace application_audio {
struct ProcessBirth {
    uint32_t pid = 0, parent = 0;
    uint64_t identity = 0, interruptTime = 0;
    bool live = false, known = false;
};
inline bool live_identity(uint32_t pid, const ProcessBirth& birth) {
    return birth.known && birth.live && birth.pid == pid && birth.identity && birth.interruptTime;
}
inline bool same_identity(const ProcessBirth& a, const ProcessBirth& b) {
    return a.pid == b.pid && a.identity == b.identity && a.interruptTime == b.interruptTime && a.parent == b.parent;
}
inline bool legacy_descendant(uint32_t pid, uint32_t root, const std::map<uint32_t, uint32_t>& graph) {
    for (int limit = 0; pid && limit < 128; ++limit) {
        if (pid == root) return true;
        auto edge = graph.find(pid);
        if (edge == graph.end() || edge->second == pid) break;
        pid = edge->second;
    }
    return false;
}
// Only these prefixes can establish disjoint ownership: every edge binds the
// actual current parent to the snapshot and to an older, held live identity.
// No invalid intermediate edge is ever pruned from the legacy exclusion walk.
template<class Lookup>
std::map<uint32_t, ProcessBirth> validated_prefix(uint32_t pid, const std::map<uint32_t, uint32_t>& graph, Lookup lookup) {
    std::map<uint32_t, ProcessBirth> prefix;
    auto current = lookup(pid);
    for (int limit = 0; pid && limit < 128; ++limit) {
        if (!live_identity(pid, current) || prefix.find(pid) != prefix.end()) break;
        prefix.emplace(pid, current);
        auto edge = graph.find(pid);
        if (edge == graph.end() || !edge->second || current.parent != edge->second) break;
        auto parent = lookup(edge->second);
        if (!live_identity(edge->second, parent) || parent.interruptTime >= current.interruptTime) break;
        pid = edge->second;
        current = parent;
    }
    return prefix;
}
template<class Lookup>
bool related(uint32_t a, uint32_t b, const std::map<uint32_t, uint32_t>& graph, Lookup lookup) {
    if (a == b) return true;
    const auto left = validated_prefix(a, graph, lookup);
    const auto right = validated_prefix(b, graph, lookup);
    if (left.find(b) == left.end() && right.find(a) == right.end()) {
        for (const auto& entry : left) {
            auto common = right.find(entry.first);
            if (common != right.end() && same_identity(entry.second, common->second)) return false;
        }
    }
    // Missing, equal-time, exited, inconsistent or historically broken prefixes
    // prove no separation. Retain the ENTIRE legacy exclusion in both directions.
    return legacy_descendant(a, b, graph) || legacy_descendant(b, a, graph);
}
}
