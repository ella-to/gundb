package cli

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"time"

	ucli "github.com/urfave/cli/v3"
)

// visualHTML is the page gundb visual serves. It loads GUN and D3 from a
// CDN and talks to --addr itself; this process only hands it the address.
//
//go:embed visual.html
var visualHTML []byte

// visualConfig is the placeholder in visualHTML replaced by the page's
// settings, as JSON.
const visualConfig = `{/*CONFIG*/}`

func (a *App) visualCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "visual",
		Usage: "open a live graph of a server's data in the browser",
		Description: `Serves a page that connects to --addr with GUN.js, draws every node and
value with D3, and keeps it live: new nodes grow out of the node that links
to them, changes flash, deletes fade away. A log at the bottom lists the
PUTs, SETs and GETs the page sees.

  gundb visual                                      # the server on ` + DefaultAddr + `
  gundb visual --addr https://relay.example.com/gun
  gundb visual --listen localhost:9000 --no-open    # open it yourself

The page lists every soul with a LEX get, so --addr must be a gundb server
(or a GUN.js relay with radisk). It asks again every --rescan to find nodes
nothing links to; changes to nodes it already has arrive as they happen.
GETs show up only when the server passes them on, which it does when it
cannot answer them from its own store.`,
		Flags: []ucli.Flag{
			addrFlag(),
			&ucli.StringFlag{
				Name:    "listen",
				Aliases: []string{"l"},
				Usage:   "`host:port` to serve the page on; port 0 picks a free one",
				Value:   "localhost:0",
				Sources: ucli.EnvVars("GUNDB_VISUAL_LISTEN"),
			},
			&ucli.DurationFlag{
				Name:  "rescan",
				Usage: "how often the page lists every soul again, to find new unlinked nodes; 0 only at start",
				Value: 5 * time.Second,
			},
			&ucli.BoolFlag{
				Name:  "no-open",
				Usage: "print the page's URL instead of opening the browser",
			},
		},
		OnUsageError: usageError,
		Action:       a.visual,
	}
}

func (a *App) visual(ctx context.Context, c *ucli.Command) error {
	if c.Args().Len() > 0 {
		return fail("give the server with --addr, e.g. gundb visual --addr "+DefaultAddr,
			"gundb visual takes no arguments, got %q", c.Args().Slice())
	}
	log, _ := a.logger(c)
	addr, err := browserAddr(c.String("addr"))
	if err != nil {
		return err
	}
	page, err := visualPage(addr, c.Duration("rescan"))
	if err != nil {
		return err
	}

	host := c.String("listen")
	ln, err := net.Listen("tcp", host)
	if err != nil {
		return explainListen(host, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(page)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	pageURL := "http://" + pageHost(ln.Addr().(*net.TCPAddr), host)
	log.Info("gundb visual is running, Ctrl-C to stop", "page", pageURL, "server", addr)
	if a.onListen != nil {
		a.onListen(pageURL)
	}
	if !c.Bool("no-open") {
		if err := openBrowser(pageURL); err != nil {
			log.Warn("cannot open the browser; open the page yourself", "page", pageURL, "reason", err.Error())
		}
	}

	select {
	case <-ctx.Done():
	case err := <-served:
		return fmt.Errorf("the page server stopped: %w", err)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	return nil
}

// browserAddr checks --addr and returns it as the URL GUN.js in a browser
// connects to: http(s), which GUN turns into ws(s) itself.
func browserAddr(raw string) (string, error) {
	if err := checkPeerURL("--addr", raw); err != nil {
		return "", err
	}
	u, _ := url.Parse(raw)
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	return u.String(), nil
}

// visualPage returns visualHTML with its settings filled in.
func visualPage(addr string, rescan time.Duration) ([]byte, error) {
	if rescan < 0 {
		return nil, fail("use a duration such as 5s, or 0 to list only at start", "--rescan must not be negative, got %s", rescan)
	}
	if rescan > 0 && rescan < 500*time.Millisecond {
		rescan = 500 * time.Millisecond // each rescan sends the whole graph
	}
	// json.Marshal escapes <, > and &, so the result is safe in a <script>.
	cfg, err := json.Marshal(map[string]any{
		"addr":   addr,
		"rescan": rescan.Milliseconds(),
	})
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(visualHTML, []byte(visualConfig)) {
		return nil, errors.New("gundb visual: the page has no settings placeholder")
	}
	return bytes.Replace(visualHTML, []byte(visualConfig), cfg, 1), nil
}

// pageHost is host:port for the page's URL: the port actually bound, and
// localhost when listening on every interface.
func pageHost(bound *net.TCPAddr, listen string) string {
	host, _, _ := net.SplitHostPort(listen)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return net.JoinHostPort(host, fmt.Sprint(bound.Port))
}

// openBrowser opens url in the default browser, without waiting for it.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
