#include "../../native/process-parent.hpp"
#include <stdexcept>
#include <iostream>
using application_audio::ProcessBirth;
using Graph = std::map<uint32_t, uint32_t>;

static void require(bool condition) { if (!condition) throw std::runtime_error("process parent identity regression failed"); }

int main() {
    // The selected app's exited launcher had PID 30. Dilla later reuses 30;
    // this raw PPID edge must not hide the independent selected audio service.
    Graph graph{{10, 20}, {20, 30}, {30, 50}, {40, 30}};
    std::map<uint32_t, ProcessBirth> births{
        {10, {10, 20, 1000, 250, true, true}},
        {20, {20, 30, 900, 200, true, true}},
        {30, {30, 50, 800, 500, true, true}},
        {40, {40, 30, 700, 600, true, true}},
        {50, {50, 0, 600, 100, true, true}},
    };
    auto is_descendant = [&](uint32_t pid, uint32_t root) {
        return application_audio::descendant(pid, root, graph, [&](uint32_t p) { return births[p]; });
    };
    require(!is_descendant(10, 30));
    require(!is_descendant(30, 10));
    require(is_descendant(40, 30));
    require(is_descendant(30, 50));
    require(is_descendant(30, 30));
    // FILETIME identities deliberately run backwards above: wall-clock rollback
    // cannot sever genuine own ancestry or validate a reused parent PID.
    require(!application_audio::impossible_parent(40, 30, births[40], births[30]));
    require(application_audio::impossible_parent(20, 30, births[20], births[30]));
    const auto child = births[20], parent = births[30];
    for (int missing = 0; missing < 12; ++missing) {
        auto c = child, p = parent;
        switch (missing) {
            case 0: c.known = false; break;
            case 1: p.known = false; break;
            case 2: c.live = false; break;
            case 3: p.live = false; break;
            case 4: c.parent = 99; break;
            case 5: c.pid = 99; break;
            case 6: p.pid = 99; break;
            case 7: c.interruptTime = 0; break;
            case 8: p.interruptTime = c.interruptTime; break;
            case 9: p.identity = 0; break;
            case 10: c.identity = 0; break;
            case 11: p.interruptTime = 0; break;
        }
        require(!application_audio::impossible_parent(20, 30, c, p));
    }
    births[30].known = false;
    require(is_descendant(10, 30)); // Unknown information retains legacy exclusion.
    births[30] = parent;
    graph[50] = 30; births[50].parent = 30; births[50].known = false;
    require(!is_descendant(10, 99)); // Cycles stay bounded.
    std::cout << "process parent identity regression passed\n";
}
