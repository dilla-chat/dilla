#include "../../native/process-parent.hpp"
#include <stdexcept>
#include <iostream>
using application_audio::ProcessBirth;
using Graph = std::map<uint32_t, uint32_t>;
using Births = std::map<uint32_t, ProcessBirth>;
static void require(bool condition) { if (!condition) throw std::runtime_error("process parent identity regression failed"); }
static bool related(uint32_t a, uint32_t b, const Graph& graph, Births& births) {
    auto lookup = [&](uint32_t p) { return births[p]; };
    return application_audio::related(a, b, graph, lookup);
}
int main() {
    // Real historical own grandchild: R -> A -> B, then A's PID is reused by
    // another current child C of R. A newer C proves no ownership separation.
    {
        Graph graph{{10, 20}, {20, 30}, {30, 50}, {50, 0}};
        Births births{{10,{10,20,1000,250,true,true}}, {20,{20,30,900,400,true,true}},
            {30,{30,50,800,200,true,true}}, {50,{50,0,700,100,true,true}}};
        require(related(10, 30, graph, births));
    }
    // Historical own ancestor with two reused PIDs: X300 -> P100 -> A200 -> R300.
    // X and A exit; foreign C reuses 200. Raw forward P->R is the only remaining
    // exclusion. Endpoint birth ordering must not erase that protection either.
    {
        Graph graph{{100,300},{300,200},{200,400},{400,0}};
        Births births{{100,{100,300,4000,10,true,true}}, {300,{300,200,3000,30,true,true}},
            {200,{200,400,2000,40,true,true}}, {400,{400,0,1000,5,true,true}}};
        require(related(100, 300, graph, births));
    }
    // Independent apps have complete validated prefixes to the SAME live parent
    // 900, before an ancient dead parent PID was reused as Dilla 1000. Both
    // directions of the legacy walk alone wrongly connect those sibling branches.
    Graph graph{{1000,1700},{1700,900},{2000,1900},{1900,1800},{1800,900},{900,1000}};
    Births births{{1000,{1000,1700,6000,300,true,true}}, {1700,{1700,900,5000,260,true,true}},
        {2000,{2000,1900,4000,350,true,true}}, {1900,{1900,1800,3000,250,true,true}},
        {1800,{1800,900,2000,200,true,true}}, {900,{900,1000,1000,100,true,true}}};
    require(!related(2000,1000,graph,births));
    require(!related(1000,2000,graph,births));
    require(related(1000,1000,graph,births));
    require(related(1000,1700,graph,births));
    require(related(1700,1000,graph,births));
    // FILETIME identity values deliberately decrease as monotonic births rise.
    // A wall-clock correction cannot affect the disjoint-prefix proof.
    for (int missing = 0; missing < 12; ++missing) {
        auto changed = births;
        switch (missing) {
            case 0: changed[1000].known = false; break;
            case 1: changed[2000].known = false; break;
            case 2: changed[1700].live = false; break;
            case 3: changed[1800].known = false; break;
            case 4: changed[1900].parent = 99; break;
            case 5: changed[1700].pid = 99; break;
            case 6: changed[900].pid = 99; break;
            case 7: changed[1000].interruptTime = 0; break;
            case 8: changed[1800].interruptTime = changed[900].interruptTime; break;
            case 9: changed[1900].identity = 0; break;
            case 10: changed[900].live = false; break;
            case 11: changed[1900].interruptTime = 150; break;
        }
        require(related(2000,1000,graph,changed));
    }
    // No demonstrated common ancestor means no relaxation of an existing link.
    auto independent = births;
    independent[900].known = false;
    require(related(2000,1000,graph,independent));
    // A common PID without the SAME pinned generation does not establish separation.
    unsigned commonReads = 0;
    require(application_audio::related(2000, 1000, graph, [&](uint32_t pid) {
        auto birth = births[pid];
        if (pid == 900 && ++commonReads > 1) ++birth.identity;
        return birth;
    }));
    require(application_audio::related(1000, 1000, graph, [](uint32_t) -> ProcessBirth {
        throw std::runtime_error("self exclusion must precede metadata lookup");
    }));
    // A verified prefix is bounded even when a large graph supplies valid identities.
    Graph longGraph;
    Births longBirths;
    for (uint32_t pid = 1; pid <= 200; ++pid) {
        longGraph[pid] = pid + 1;
        longBirths[pid] = {pid, pid + 1, pid, 1000 - pid, true, true};
    }
    unsigned lookups = 0;
    const auto prefix = application_audio::validated_prefix(1, longGraph, [&](uint32_t pid) {
        ++lookups;
        return longBirths[pid];
    });
    require(prefix.size() == 128 && lookups <= 129);
    longGraph[200] = 1;
    longBirths[200].parent = 1;
    require(!application_audio::legacy_descendant(1, 999, longGraph));
    std::cout << "process parent identity regression passed\n";
}
