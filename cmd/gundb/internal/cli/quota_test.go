package cli

import (
	"strings"
	"testing"
)

func TestServerQuota(t *testing.T) {
	a := startServer(t, "--max-nodes", "3", "--max-bytes", "1MB")
	must(t, "user", "create", "-u", "ali", "-p", "secret123", "-a", a) // her account: 1 node
	login := []string{"-u", "ali", "-p", "secret123", "-a", a}
	must(t, append([]string{"put", "~/notes/1", "one"}, login...)...) // ~/notes: 2
	must(t, append([]string{"put", "~/todo/1", "milk"}, login...)...) // ~/todo: 3
	_, stderr, err := run(t, "", append([]string{"put", "~/more/1", "x"}, login...)...)
	if err == nil {
		t.Fatal("a 4th node was accepted")
	}
	wantOut(t, stderr, "quota exceeded", "may keep 3 nodes", "gundb put <path> null")

	// Deleting frees the node, and the write then goes through.
	must(t, append([]string{"put", "~/todo/1", "null"}, login...)...)
	must(t, append([]string{"put", "~/more/1", "x"}, login...)...)

	// Data outside users' spaces is nobody's: not limited.
	for _, p := range []string{"a/1", "b/1", "c/1", "d/1"} {
		must(t, "put", p, "1", "-a", a)
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "0": 0, "2048": 2048, "512KB": 512 << 10, "10mb": 10 << 20, "1.5GB": 3 << 29, "100 B": 100} {
		if got, err := parseBytes(in); err != nil || got != want {
			t.Errorf("parseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"ten", "-1MB", "10TB"} {
		if _, err := parseBytes(in); err == nil || !strings.Contains(err.Error(), "bad --max-bytes") {
			t.Errorf("parseBytes(%q) = %v, want an error", in, err)
		}
	}
}
