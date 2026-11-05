package cli

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestVisualServesPage checks that gundb visual serves its page with the
// server's address filled in, and nothing at other paths.
func TestVisualServesPage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	logs := &buffer{}
	a := &App{Stdin: strings.NewReader(""), Stdout: logs, Stderr: logs, onListen: func(s string) { ready <- s }}
	done := make(chan error, 1)
	go func() {
		done <- a.Command().Run(ctx, []string{"gundb", "visual", "--no-open",
			"--listen", "127.0.0.1:0", "--addr", "ws://example.test:9000/gun", "--rescan", "2s"})
	}()
	var page string
	select {
	case page = <-ready:
	case err := <-done:
		t.Fatalf("visual did not start: %v\n%s", err, logs)
	case <-time.After(5 * time.Second):
		t.Fatal("visual did not start in time")
	}

	res, err := http.Get(page)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("page: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	want := `window.GUNDB_VISUAL = {"addr":"http://example.test:9000/gun","rescan":2000};`
	if !strings.Contains(string(body), want) {
		t.Fatalf("page settings: want %s in the page", want)
	}
	if res, err := http.Get(page + "/nope"); err != nil || res.StatusCode != 404 {
		t.Fatalf("other paths should 404, got %v %v", res.StatusCode, err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("visual: %v\n%s", err, logs)
	}
}

func TestVisualBadFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"visual", "--no-open", "--addr", "localhost:8765"}, "bad --addr"},
		{[]string{"visual", "--no-open", "--rescan", "-1s"}, "--rescan must not be negative"},
		{[]string{"visual", "--no-open", "extra"}, "takes no arguments"},
	} {
		_, stderr, err := run(t, "", tc.args...)
		if err == nil || !strings.Contains(stderr, tc.want) {
			t.Errorf("gundb %s: want error %q, got %v\n%s", strings.Join(tc.args, " "), tc.want, err, stderr)
		}
	}
}
