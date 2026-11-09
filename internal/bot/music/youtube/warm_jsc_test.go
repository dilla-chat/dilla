package youtube

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The fake yt-dlp below has the parts of yt-dlp's node challenge provider the worker routes (Bot v2 Task 19 fix rounds
// 4 and 5): EJSBaseJCP._construct_stdin with yt-dlp's exact stdin template over the bundled solver (yt_dlp_ejs), and
// NodeJCP._run_js_runtime, which here stands for yt-dlp's own per-lookup node and answers "own-node". Its solver
// reports what it saw: the request type (a player, or the preprocessed player the long-lived solver kept), a
// per-context counter (1 in a fresh context) and whether node's process object was reachable. The search text steers
// a lookup: "killsolver" kills the worker's solver first (a crash), "stopsolver" stops it and solves nothing (a hung
// solver, found by the worker's ping), "hangsolver" stops it and then solves (a hung solver, found by the request's
// deadline), "microloop" sends a player whose solve leaves an endless Promise job, "otherscript" carries a script
// other than the bundled one.
const fakeJscCore = `function jsc(data) {
  globalThis.__count = (globalThis.__count || 0) + 1;
  if (data.player === 'MICROLOOP') Promise.resolve().then(function () { for (;;) {} });
  var result = { type: 'result', responses: [{ type: 'result', data: { seen: data.type, count: globalThis.__count, process: typeof process } }] };
  if (data.type === 'player') result.preprocessed_player = 'PRE:' + data.player;
  return result;
}`

const fakeJscSolver = `def lib():
    return "var lib = {};"


def core():
    return ` + "'''" + fakeJscCore + "'''" + `
`

const fakeJscLibrary = `import json, os, signal, sys
PRINT = ` + "'''" + fakePrintJSON + "'''" + `


def solver_pids():
    for pid in os.listdir("/proc"):
        if not pid.isdigit():
            continue
        try:
            with open("/proc/%s/cmdline" % pid, "rb") as f:
                args = f.read().split(b"\0")
        except OSError:
            continue
        if len(args) > 3 and args[1] == b"--permission" and args[2] == b"-e":
            yield int(pid)


def main(argv):
    from yt_dlp.extractor.youtube.jsc._builtin import ejs as ejs_module
    from yt_dlp.extractor.youtube.jsc._builtin import node as node_module
    split = argv.index("--")
    url = argv[split + 1] if len(argv) > split + 1 else ""
    for marker, sig in (("killsolver", signal.SIGKILL), ("stopsolver", signal.SIGSTOP), ("hangsolver", signal.SIGSTOP)):
        if marker in url:
            for pid in solver_pids():
                os.kill(pid, sig)
    info = json.loads(PRINT)
    if "stopsolver" in url:
        info["title"] = "stopped"
        sys.stdout.write(json.dumps(info) + "\n")
        return
    provider = node_module.NodeJCP()
    if "otherscript" in url:
        provider._core_script = ejs_module._Script(provider._core_script.code + "\n// another script")
    player = "MICROLOOP" if "microloop" in url else "PLAYER-ONE"
    stdin = provider._construct_stdin(player, False, [node_module.Request("n", ["abc"])])
    seen = json.loads(provider._run_js_runtime(stdin))["responses"][0]["data"]
    info["title"] = "seen=%s count=%s process=%s" % (seen["seen"], seen["count"], seen["process"])
    sys.stdout.write(json.dumps(info) + "\n")
`

const fakeJscEJS = `import json
from yt_dlp_ejs.yt import solver


class _Script:
    def __init__(self, code):
        self.code = code


class EJSBaseJCP:
    _lib_script = _Script(solver.lib())
    _core_script = _Script(solver.core())

    def _construct_stdin(self, player, preprocessed, requests, /):
        json_requests = [{
            'type': request.type.value,
            'challenges': request.input.challenges,
        } for request in requests]
        data = {
            'type': 'preprocessed',
            'preprocessed_player': player,
            'requests': json_requests,
        } if preprocessed else {
            'type': 'player',
            'player': player,
            'requests': json_requests,
            'output_preprocessed': True,
        }
        return f'''\
        {self._lib_script.code}
        Object.assign(globalThis, lib);
        {self._core_script.code}
        console.log(JSON.stringify(jsc({json.dumps(data)})));
        '''
`

const fakeJscNode = `import json
from .ejs import EJSBaseJCP


class _Value:
    def __init__(self, value):
        self.value = value


class _Input:
    def __init__(self, challenges):
        self.challenges = challenges


class Request:
    def __init__(self, kind, challenges):
        self.type = _Value(kind)
        self.input = _Input(challenges)


class NodeJCP(EJSBaseJCP):
    def _run_js_runtime(self, stdin, /):
        return json.dumps({"type": "result", "responses": [{"type": "result", "data": {"seen": "own-node", "count": 0, "process": "n/a"}}]})
`

func writeJscVenv(t *testing.T, dir string) string {
	t.Helper()
	return writeYtDlpVenvWith(t, dir, map[string]string{
		"__init__.py":                                fakeJscLibrary,
		"extractor/__init__.py":                      "",
		"extractor/youtube/__init__.py":              "",
		"extractor/youtube/jsc/__init__.py":          "",
		"extractor/youtube/jsc/_builtin/__init__.py": "",
		"extractor/youtube/jsc/_builtin/ejs.py":      fakeJscEJS,
		"extractor/youtube/jsc/_builtin/node.py":     fakeJscNode,
		"../yt_dlp_ejs/__init__.py":                  "",
		"../yt_dlp_ejs/yt/__init__.py":               "",
		"../yt_dlp_ejs/yt/solver.py":                 fakeJscSolver,
	})
}

func jscResolver(t *testing.T) *WarmResolver {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	bwrap, _ := countingBwrap(t, dir)
	r, err := NewWarmResolver(SandboxConfig{Bubblewrap: bwrap, YTDLP: writeJscVenv(t, dir), Node: node, FFmpeg: "ffmpeg", LookupTimeout: 20 * time.Second}, 6*time.Hour)
	if err != nil {
		t.Skipf("warm resolver unavailable: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// One node per worker solves every lookup's challenges, each in a fresh context (count=1, no process object); what
// it keeps between lookups is the player's preprocessed form (the second lookup is answered from it).
func TestTheWorkersSolverIsKeptAcrossLookupsWithAFreshContextEach(t *testing.T) {
	r := jscResolver(t)
	for _, want := range []struct{ query, title string }{
		{"first song", "seen=player count=1 process=undefined"},
		{"second song", "seen=preprocessed count=1 process=undefined"},
		{"third song", "seen=preprocessed count=1 process=undefined"},
	} {
		title, err := resolveTitle(t, r, want.query)
		if err != nil {
			t.Fatalf("%q: %v", want.query, err)
		}
		if title != want.title {
			t.Fatalf("%q solved as %q, want %q", want.query, title, want.title)
		}
	}
}

// A solver that died is not waited for: the lookup that finds it gone runs yt-dlp's own node, and the next lookup
// starts a new solver.
func TestAKilledSolverFallsBackAndIsStartedAgain(t *testing.T) {
	r := jscResolver(t)
	if title, err := resolveTitle(t, r, "first song"); err != nil || !strings.HasPrefix(title, "seen=player ") {
		t.Fatalf("first lookup = %q, %v", title, err)
	}
	if title, err := resolveTitle(t, r, "killsolver song"); err != nil || !strings.HasPrefix(title, "seen=own-node ") {
		t.Fatalf("the lookup that killed the solver = %q, %v, want yt-dlp's own node", title, err)
	}
	if title, err := resolveTitle(t, r, "after song"); err != nil || title != "seen=player count=1 process=undefined" {
		t.Fatalf("the lookup after = %q, %v, want a new solver (its cache empty)", title, err)
	}
}

// Fix round 4: with WarmUpLookup a new worker's first lookup is its own warm-up of a public video, run in the
// background; a member's lookup is neither held by it nor answered from it.
func TestANewWorkerWarmsItselfWithAPublicLookup(t *testing.T) {
	w := newFakeLookup()
	r, _, _, _ := testWarm(t, w)
	warmed := make(chan struct{})
	r.deps.warm = func(w lookupWorker) { r.warmUpWorker(w); close(warmed) }
	if _, err := r.Resolve(WithOwner(t.Context(), "community-a"), "https://www.youtube.com/watch?v=aqz-KE-bpKQ"); err != nil {
		t.Fatal(err)
	}
	<-warmed
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.args) != 2 {
		t.Fatalf("lookups = %d, want the warm-up and the member's", len(w.args))
	}
	warmUps := 0
	for _, args := range w.args {
		if args[len(args)-1] == videoURL(warmUpVideo) {
			warmUps++
		}
	}
	if warmUps != 1 {
		t.Fatalf("warm-up lookups = %d in %v", warmUps, w.args)
	}
}

// Fix round 5 (M4-1): a solver that stops answering is found by the worker's ping before the next lookup and
// replaced at once; the lookup is served by the new one.
func TestAHungSolverIsFoundByThePingAndReplaced(t *testing.T) {
	r := jscResolver(t)
	if title, err := resolveTitle(t, r, "first song"); err != nil || title != "seen=player count=1 process=undefined" {
		t.Fatalf("first lookup = %q, %v", title, err)
	}
	if title, err := resolveTitle(t, r, "stopsolver song"); err != nil || title != "stopped" {
		t.Fatalf("the lookup that stopped the solver = %q, %v", title, err)
	}
	began := time.Now()
	title, err := resolveTitle(t, r, "after song")
	if err != nil || title != "seen=player count=1 process=undefined" {
		t.Fatalf("the lookup after = %q, %v, want a new solver", title, err)
	}
	if waited := time.Since(began); waited > 4*time.Second {
		t.Fatalf("the lookup after a hung solver took %v", waited)
	}
}

// Fix round 5 (M4-1): a solver that hangs during a request is killed at the request's deadline; that lookup runs
// yt-dlp's own node, and the next gets a new solver.
func TestASolverThatHangsMidRequestIsKilledAtItsDeadline(t *testing.T) {
	r := jscResolver(t)
	if title, err := resolveTitle(t, r, "first song"); err != nil || !strings.HasPrefix(title, "seen=player ") {
		t.Fatalf("first lookup = %q, %v", title, err)
	}
	if title, err := resolveTitle(t, r, "hangsolver song"); err != nil || !strings.HasPrefix(title, "seen=own-node ") {
		t.Fatalf("the lookup whose solver hung = %q, %v, want yt-dlp's own node", title, err)
	}
	if title, err := resolveTitle(t, r, "after song"); err != nil || title != "seen=player count=1 process=undefined" {
		t.Fatalf("the lookup after = %q, %v, want a new solver", title, err)
	}
}

// Fix round 5 (M4-1): an endless Promise job runs inside the evaluation and its timeout (microtaskMode
// 'afterEvaluate'): that request fails alone, and the same solver, its cache intact, answers the next lookup.
func TestAnEndlessPromiseJobFailsItsOwnRequestOnly(t *testing.T) {
	r := jscResolver(t)
	if title, err := resolveTitle(t, r, "first song"); err != nil || title != "seen=player count=1 process=undefined" {
		t.Fatalf("first lookup = %q, %v", title, err)
	}
	if title, err := resolveTitle(t, r, "microloop song"); err != nil || !strings.HasPrefix(title, "seen=own-node ") {
		t.Fatalf("the endless Promise job = %q, %v, want its request refused", title, err)
	}
	if title, err := resolveTitle(t, r, "after song"); err != nil || title != "seen=preprocessed count=1 process=undefined" {
		t.Fatalf("the lookup after = %q, %v, want the same solver (its preprocessed player kept)", title, err)
	}
}

// Fix round 5 (M4-2): the solver runs only the bundled script; a request for another is refused, and that lookup
// runs yt-dlp's own node.
func TestASolverRequestForAnotherScriptIsRefused(t *testing.T) {
	r := jscResolver(t)
	if title, err := resolveTitle(t, r, "otherscript song"); err != nil || !strings.HasPrefix(title, "seen=own-node ") {
		t.Fatalf("another script = %q, %v, want yt-dlp's own node", title, err)
	}
	if title, err := resolveTitle(t, r, "first song"); err != nil || title != "seen=player count=1 process=undefined" {
		t.Fatalf("the bundled script after = %q, %v", title, err)
	}
}
