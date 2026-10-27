//go:build !(js && wasm)

package idbstore

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestInWasm runs this package's js/wasm tests in Node, with IndexedDB
// provided by fake-indexeddb, so `go test ./...` covers the store without a
// browser. Set it up with:
//
//	(cd storage/idbstore/testdata && npm install)
func TestInWasm(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a wasm test binary")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	auto, err := filepath.Abs("testdata/node_modules/fake-indexeddb/auto/index.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(auto); err != nil {
		t.Skip("run `npm install` in storage/idbstore/testdata to enable the wasm tests")
	}
	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Skipf("go env GOROOT: %v", err)
	}
	wasmExec := filepath.Join(strings.TrimSpace(string(goroot)), "lib", "wasm")
	cmd := exec.Command("go", "test", "-count=1", "-v", ".")
	cmd.Env = append(os.Environ(),
		"GOOS=js", "GOARCH=wasm",
		"PATH="+wasmExec+string(os.PathListSeparator)+os.Getenv("PATH"),
		"NODE_OPTIONS=--require="+auto,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wasm tests failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
