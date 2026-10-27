// A Go app in the browser: compiled to WebAssembly, storing its data in
// IndexedDB, and syncing through a Go relay. This program builds app/ to
// wasm and serves it next to the relay.
//
//	go run ./examples/browser
//
// then open http://localhost:8766 in two browsers. Stop the server, add
// notes, reload: the page still loads (sw.js) and the notes are still there
// (IndexedDB). Start the server again and the other browser catches up.
package main

import (
	_ "embed"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"ella.to/gundb"
)

//go:embed index.html
var page []byte

//go:embed sw.js
var worker []byte

func main() {
	dir, err := os.MkdirTemp("", "gundb-browser")
	check(err)
	defer os.RemoveAll(dir)

	// Build the browser app, and find the JS glue that loads it.
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "app.wasm"), "ella.to/gundb/examples/browser/app")
	build.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	build.Stderr = os.Stderr
	check(build.Run())
	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	check(err)
	glue := filepath.Join(strings.TrimSpace(string(goroot)), "lib", "wasm", "wasm_exec.js")

	relay := gundb.New()
	defer relay.Close()

	http.Handle("/gun", relay)
	http.HandleFunc("/app.wasm", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, filepath.Join(dir, "app.wasm")) })
	http.HandleFunc("/wasm_exec.js", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, glue) })
	http.HandleFunc("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		w.Write(worker)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write(page) })
	log.Print("open http://localhost:8766")
	log.Fatal(http.ListenAndServe(":8766", nil))
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
