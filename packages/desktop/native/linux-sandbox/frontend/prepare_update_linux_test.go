//go:build linux && cgo

package frontend

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestPrepareUpdateRoleRequiresExactReservedGrammar(t *testing.T) {
	nonce := strings.Repeat("a", 64)
	got, ok, err := prepareUpdateRole([]string{"--dilla-sandbox-prepare-update=" + nonce}, nil)
	if err != nil || !ok || got != nonce {
		t.Fatalf("valid role rejected: %q %v %v", got, ok, err)
	}
	for _, args := range [][]string{{"--dilla-sandbox-prepare-update=" + nonce, "x"}, {"--dilla-sandbox-prepare-update=" + nonce, "--update-recovery"}, {"--dilla-sandbox-prepare-update=ABC"}, {"--dilla-sandbox-prepare-update=" + nonce, "--dilla-sandbox-prepare-update=" + nonce}} {
		if _, _, err := prepareUpdateRole(args, nil); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if _, _, err := prepareUpdateRole([]string{"--dilla-sandbox-prepare-update=" + nonce}, []string{"DILLA_UPDATE_STARTUP_NONCE=" + nonce}); err == nil {
		t.Fatal("accepted conflicting environment")
	}
}

func TestCheckUpdateRoleCannotAuthorizeMaintenance(t *testing.T) {
	f := makeCapsule(t, 2)
	e, err := NewEmbeddedCapsule(f.raw, f.trust.PublicKeys)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	ensure := func() error { called = true; return nil }
	missing := Status{Format: 1, Status: "missing"}
	ready := Status{Format: 1, Status: "ready", Generation: 2, CompatibilityEpoch: 1, SandboxAPI: 1, MaintainerSHA512: e.constants.MaintainerSHA512, HelperSHA512: e.helperSHA512, resident: true}
	lower := ready
	lower.Generation = 1
	maintenance := ready
	maintenance.Status = "maintenance"
	for _, state := range []Status{missing, ready} {
		if err := prepareStatus(state, e, true, ensure); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range []Status{lower, maintenance, {Format: 1, Status: "unsafe"}} {
		if err := prepareStatus(state, e, true, ensure); err == nil {
			t.Fatal("accepted unsafe status")
		}
	}
	if called {
		t.Fatal("read-only check invoked authorization")
	}
	nonce := strings.Repeat("a", 64)
	if got, ok, err := checkUpdateRole([]string{"--dilla-sandbox-check-update=" + nonce}, nil); err != nil || !ok || got != nonce {
		t.Fatal("invalid check role")
	}
	if _, _, err := checkUpdateRole([]string{"--dilla-sandbox-check-update=" + nonce, "--dilla-sandbox-prepare-update=" + nonce}, nil); err == nil {
		t.Fatal("combined roles accepted")
	}
}

func TestPrepareNodeChild(t *testing.T) {
	if os.Getenv("DILLA_PREPARE_CHILD") != "1" {
		return
	}
	if err := prepareEndpoints(); err != nil {
		os.Exit(2)
	}
	signature, err := readPrepareSignature()
	if err != nil || len(signature) != 64 {
		os.Exit(3)
	}
	if _, err := os.NewFile(4, "report").Write([]byte("ok\n")); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

func TestNodeSpawnPreparationEndpoints(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	const script = `const {spawn}=require('node:child_process');const c=spawn(process.argv[1],['-test.run=^TestPrepareNodeChild$'],{env:{...process.env,DILLA_PREPARE_CHILD:'1'},stdio:['ignore','ignore','inherit','pipe','pipe']});let out='';c.stdio[3].on('error',()=>{});c.stdio[4].on('error',()=>{});c.stdio[4].on('data',b=>out+=b);c.stdio[3].end(Buffer.alloc(Number(process.argv[2])));c.on('close',code=>{process.exit((Number(process.argv[2])===64?code===0&&out==='ok\n':code!==0)?0:1)});`
	for _, size := range []string{"63", "64", "65"} {
		cmd := exec.CommandContext(t.Context(), node, "-e", script, os.Args[0], size)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("size %s: %v: %s", size, err, output)
		}
	}
}

func TestPreparationRejectsFileListenerAndTCP(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	const script = `const {spawn}=require('node:child_process'),net=require('node:net'),fs=require('node:fs');
const run=fd=>new Promise(resolve=>{const c=spawn(process.argv[1],['-test.run=^TestPrepareNodeChild$'],{env:{...process.env,DILLA_PREPARE_CHILD:'1'},stdio:['ignore','ignore','ignore',fd,'pipe']});c.stdio[4].on('error',()=>{});c.on('close',code=>resolve(code===2));});
(async()=>{let ok=await run(fs.openSync('/dev/null','r'));const server=net.createServer();await new Promise(r=>server.listen(0,'127.0.0.1',r));ok=ok&&await run(server._handle.fd);let accepted;server.once('connection',s=>accepted=s);const client=net.connect(server.address().port,'127.0.0.1');await new Promise(r=>client.once('connect',r));ok=ok&&await run(client._handle.fd);client.destroy();accepted.destroy();server.close();process.exit(ok?0:1)})().catch(()=>process.exit(1));`
	cmd := exec.CommandContext(t.Context(), node, "-e", script, os.Args[0])
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("invalid endpoint accepted: %v: %s", err, output)
	}
}
