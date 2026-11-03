package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// logHandler writes one short line per record, for people rather than
// log collectors:
//
//	14:03:07 INFO  gundb server is running  addr=http://localhost:8765/gun
type logHandler struct {
	w     io.Writer
	mu    *sync.Mutex
	level slog.Leveler
	attrs string // pre-formatted With attributes
}

func newLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(&logHandler{w: w, mu: &sync.Mutex{}, level: level})
}

func (h *logHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level.Level() }

func (h *logHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format(time.TimeOnly))
	fmt.Fprintf(&b, " %-5s ", levelName(r.Level))
	b.WriteString(strings.TrimPrefix(r.Message, "gundb: "))
	b.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, a)
		return true
	})
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var b strings.Builder
	for _, a := range attrs {
		writeAttr(&b, a)
	}
	cp := *h
	cp.attrs += b.String()
	return &cp
}

func (h *logHandler) WithGroup(string) slog.Handler { return h }

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN"
	case l >= slog.LevelInfo:
		return "INFO"
	}
	return "DEBUG"
}

func writeAttr(b *strings.Builder, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	v := a.Value.Resolve().String()
	if v == "" || strings.ContainsAny(v, " \t\"=") {
		v = fmt.Sprintf("%q", v)
	}
	fmt.Fprintf(b, "  %s=%s", a.Key, v)
}
