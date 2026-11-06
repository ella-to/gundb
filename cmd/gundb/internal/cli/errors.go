package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"ella.to/gundb"
)

// hintError is an error with advice on what to do about it, printed on
// its own line under the error.
type hintError struct {
	err  error
	hint string
}

func (e *hintError) Error() string { return e.err.Error() }
func (e *hintError) Unwrap() error { return e.err }

// withHint attaches a hint to err.
func withHint(err error, format string, args ...any) error {
	return &hintError{err: err, hint: fmt.Sprintf(format, args...)}
}

// fail returns an error with a message and a hint.
func fail(hint, format string, args ...any) error {
	return withHint(fmt.Errorf(format, args...), "%s", hint)
}

// explain turns an error from gundb into one a person can act on. what
// names the operation ("read users/1"), timeout is the --timeout in force.
func explain(err error, what string, timeout time.Duration) error {
	var h *hintError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &h):
		return err // already explained
	case errors.Is(err, gundb.ErrNotFound):
		return fail("write something there first with: gundb put <path> <value>", "nothing at %s", strings.TrimPrefix(what, "read "))
	case errors.Is(err, gundb.ErrForbidden):
		return fail("the server's rules (CanRead/CanWrite) refused it; are you sending the right --header?",
			"the server does not allow you to %s: %s", what, clean(err))
	case errors.Is(err, gundb.ErrUnverified):
		return fail("a user's space can only be written by that user: log in with --username and write to ~/...",
			"the server rejected the write: %s", clean(err))
	case errors.Is(err, gundb.ErrQuota):
		return fail("free some space by deleting what you no longer need (gundb put <path> null), or move to a bigger plan",
			"the server refused to %s: %s", what, clean(err))
	case errors.Is(err, gundb.ErrNoPeers):
		return fail("is the server still running?", "no server acknowledged the write to %s", strings.TrimPrefix(what, "write "))
	case errors.Is(err, context.DeadlineExceeded):
		return fail("wait longer with --timeout 10s, or --timeout 0 to wait forever",
			"timed out after %s trying to %s", timeout, what)
	case errors.Is(err, context.Canceled):
		return errors.New("interrupted")
	case errors.Is(err, gundb.ErrWrongLogin):
		return fail("check --username and --password; create a user with: gundb user create", "wrong username or password")
	case errors.Is(err, gundb.ErrUserExists):
		return fail("log in instead with: gundb user login", "that username is taken")
	}
	return fmt.Errorf("could not %s: %s", what, clean(err))
}

// clean drops the package prefixes from an error's text.
func clean(err error) string {
	s := err.Error()
	for _, p := range []string{"gundb: ", "sea: "} {
		s = strings.ReplaceAll(s, p, "")
	}
	return s
}

// printError writes err for a person: "error: ..." and any hints.
func printError(w io.Writer, err error) {
	fmt.Fprintf(w, "error: %s\n", err)
	for e := err; e != nil; e = errors.Unwrap(e) {
		if h, ok := e.(*hintError); ok && h.hint != "" {
			fmt.Fprintf(w, "hint:  %s\n", h.hint)
		}
	}
}
