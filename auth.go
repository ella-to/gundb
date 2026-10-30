package gundb

import (
	"errors"
	"fmt"
	"strings"

	"ella.to/gundb/transport"
)

// ErrForbidden is returned by PutAck when a peer's rules (Options.CanWrite)
// reject the write.
var ErrForbidden = errors.New("gundb: forbidden")

// AttachAs is Attach for a connection whose user you identified yourself,
// for transports other than the built-in WebSocket handler. Options.CanRead
// and Options.CanWrite are checked against user; customID is reported in
// PeerInfo, like the one Options.Authenticate returns.
func (db *DB) AttachAs(conn transport.Conn, user, customID string) {
	db.run(conn, nil, user, customID)
}

// trusted reports whether p's requests skip the rules: peers we dialled
// ourselves (Options.Peers) are trusted, only peers that connect to us are
// checked.
func (p *peer) trusted() bool { return p.origin != nil }

// allowWrite returns ErrForbidden, with the first denied field, if p may
// not write all of g.
func (db *DB) allowWrite(p *peer, g graph) error {
	if db.canWrite == nil || p.trusted() {
		return nil
	}
	for soul, n := range g {
		for field := range n.Fields {
			if !db.canWrite(p.user, soul, field) {
				return fmt.Errorf("%w: %q may not write %s.%s", ErrForbidden, p.user, soul, field)
			}
		}
	}
	return nil
}

// allowRead reports whether p may read soul.
func (db *DB) allowRead(p *peer, soul string) bool {
	return db.canRead == nil || p.trusted() || db.canRead(p.user, soul)
}

// sendTo sends m, already encoded as raw, to q without the nodes q may not
// read. An ack is still sent if nothing is left of its data, so whoever
// waits for it is not left hanging.
//
// When one message goes to many peers, pass the same non-nil memo for all
// of them: what a user may read is then worked out, and encoded, once per
// user rather than once per connection.
func (db *DB) sendTo(q *peer, m *message, raw []byte, memo map[string]readable) {
	if db.canRead == nil || q.trusted() || len(m.Put) == 0 {
		q.send(raw)
		return
	}
	r, ok := memo[q.user]
	if !ok {
		r = db.readableBy(q.user, m, raw)
		if memo != nil {
			memo[q.user] = r
		}
	}
	if !r.none {
		q.send(r.raw)
	}
}

// readable is a message as one user may see it.
type readable struct {
	raw  []byte
	none bool // nothing to send
}

func (db *DB) readableBy(user string, m *message, raw []byte) readable {
	keep := graph{}
	for soul, n := range m.Put {
		if db.canRead(user, soul) {
			keep[soul] = n
		}
	}
	switch {
	case len(keep) == len(m.Put):
		return readable{raw: raw}
	case len(keep) == 0 && m.Ack == "":
		return readable{none: true} // a put with nothing the user may see
	}
	cp := *m
	cp.Put, cp.Hash = keep, nil
	if len(keep) == 0 {
		cp.Put = nil
	}
	return readable{raw: db.encode(&cp)}
}

// remoteError turns an error text received from a peer back into an error
// that matches ErrForbidden or ErrUnverified.
func remoteError(text string) error {
	for _, known := range []error{ErrForbidden, ErrUnverified} {
		if rest, ok := strings.CutPrefix(text, known.Error()); ok {
			return fmt.Errorf("%w%s", known, rest)
		}
	}
	return errors.New(text)
}
