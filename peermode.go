package gundb

import (
	"fmt"
	neturl "net/url"
	"strings"
)

// PeerMode says which way data flows between this DB and a peer it dials
// (Options.Peers, Connect). Peers that connect to this DB are always
// PeerBoth: Options.CanRead and CanWrite decide what they may do.
type PeerMode uint8

const (
	// PeerBoth reads from the peer and writes to it: the default.
	PeerBoth PeerMode = iota
	// PeerRead only reads from the peer (see ReadFrom).
	PeerRead
	// PeerWrite only writes to the peer (see WriteTo).
	PeerWrite
)

func (m PeerMode) String() string {
	switch m {
	case PeerRead:
		return "read"
	case PeerWrite:
		return "write"
	}
	return "both"
}

// modeParam is the query parameter that sets a peer's mode in its URL:
// wss://b/gun?gundb_mode=read, ?gundb_mode=write or ?gundb_mode=both. Without it a peer
// is both ways. It is taken out before dialling, so the peer never sees it.
const modeParam = "gundb_mode"

// ReadFrom marks a peer URL, for Options.Peers or Connect, as one this DB
// only reads from: it asks the peer for data and takes what the peer
// sends, but never sends it anything. Its own writes, and those of its
// clients, are not sent there, and gets from that peer are answered "not
// found". Use it to follow a primary without ever changing it: a read
// replica, a staging server on production data, a dashboard.
//
// It adds ?gundb_mode=read to the URL; writing that yourself is the same.
//
//	Peers: []string{gundb.ReadFrom("wss://primary.example.com/gun")}
func ReadFrom(url string) string { return withMode(url, PeerRead) }

// WriteTo marks a peer URL, for Options.Peers or Connect, as one this DB
// only writes to: its writes, and those of its clients, are sent there
// (and queued while it is away), and the peer may read from this DB, but
// this DB never asks it for anything and drops whatever data it sends.
// Use it for a peer whose data must never come back in: a backup, an
// archive, an audit log, an upstream that ingests from many edges.
//
// It adds ?gundb_mode=write to the URL; writing that yourself is the same.
//
//	Peers: []string{gundb.WriteTo("wss://backup.example.com/gun")}
func WriteTo(url string) string { return withMode(url, PeerWrite) }

// withMode returns url with its gundb_mode parameter set to mode.
func withMode(url string, mode PeerMode) string {
	if base, _, err := ParsePeer(url); err == nil {
		url = base
	}
	base, frag, _ := strings.Cut(url, "#")
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	if frag != "" {
		frag = "#" + frag
	}
	return base + sep + modeParam + "=" + mode.String() + frag
}

// ParsePeer splits a peer as Options.Peers takes it into the URL to dial,
// without its gundb_mode parameter, and the mode that parameter sets: read,
// write or both, PeerBoth when it is absent. Any other value is an error,
// and so is giving it twice: a peer meant to be read only must not be
// written to because of a typo. The URL's other parameters are kept as
// they are.
//
//	ParsePeer("wss://b/gun?token=x&gundb_mode=read") // "wss://b/gun?token=x", PeerRead
func ParsePeer(s string) (url string, mode PeerMode, err error) {
	base, rest, ok := strings.Cut(s, "?")
	if !ok {
		return s, PeerBoth, nil
	}
	query, frag, hasFrag := strings.Cut(rest, "#")
	var keep []string
	found := false
	for _, kv := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(kv, "=")
		if k != modeParam {
			keep = append(keep, kv)
			continue
		}
		if found {
			return "", 0, fmt.Errorf("gundb: peer %q: %s= is given twice", s, modeParam)
		}
		found = true
		if v, err = neturl.QueryUnescape(v); err != nil {
			return "", 0, fmt.Errorf("gundb: peer %q: %w", s, err)
		}
		switch strings.ToLower(v) {
		case "read":
			mode = PeerRead
		case "write":
			mode = PeerWrite
		case "both":
			mode = PeerBoth
		default:
			return "", 0, fmt.Errorf("gundb: peer %q: %s=%s is not read, write or both", s, modeParam, v)
		}
	}
	if !found {
		return s, PeerBoth, nil
	}
	url = base
	if len(keep) > 0 {
		url += "?" + strings.Join(keep, "&")
	}
	if hasFrag {
		url += "#" + frag
	}
	return url, mode, nil
}

// allows reports whether a peer in mode m takes part when data must flow
// way: PeerRead (we read from it), PeerWrite (we write to it), or PeerBoth
// (either will do).
func (m PeerMode) allows(way PeerMode) bool { return way == PeerBoth || m == PeerBoth || m == way }

// flows reports whether a configured peer takes part in way.
func (o *outbound) flows(way PeerMode) bool { return o.mode.allows(way) }

// flows reports whether p takes part in way. Peers that connected to us
// take part in everything.
func (p *peer) flows(way PeerMode) bool { return p.origin == nil || p.origin.mode.allows(way) }

// reads reports whether this DB takes data from p.
func (p *peer) reads() bool { return p.flows(PeerRead) }

// writes reports whether this DB sends data to p.
func (p *peer) writes() bool { return p.flows(PeerWrite) }

// peersFor returns the connected peers that take part in way.
func (db *DB) peersFor(way PeerMode) []*peer {
	db.peersMu.Lock()
	defer db.peersMu.Unlock()
	out := make([]*peer, 0, len(db.peers))
	for p := range db.peers {
		if p.flows(way) {
			out = append(out, p)
		}
	}
	return out
}
