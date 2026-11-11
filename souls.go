package gundb

import (
	"context"
	"errors"
	"slices"
)

// ErrNoSoulStore is returned by DB.Souls when the store cannot list souls.
var ErrNoSoulStore = errors.New("gundb: store cannot list souls (see SoulStore)")

// Lex selects souls by GUN's LEX rules, like gun.get({"#": {"*": "chat/"}})
// in JS. A soul matches if it starts with Prefix and sorts between From and
// To, both inclusive, in JS string order. An empty From or To is no bound;
// the zero Lex matches every soul.
type Lex struct {
	Prefix   string
	From, To string
}

func (l Lex) lex() *lex {
	o := &lex{}
	if l.From != "" {
		o.Gt = &l.From
	}
	if l.To != "" {
		o.Lt = &l.To
	}
	if l.Prefix != "" || (o.Gt == nil && o.Lt == nil) {
		o.Prefix = &l.Prefix
	}
	return o
}

// Souls returns the souls of the nodes that match q, in JS string order.
// Peers are asked first (waiting up to Options.Wait), and the nodes they
// send are stored, so reading one with Get is then local. GUN.js relays
// answer from radisk; peers without storage that can list souls don't
// answer at all.
//
//	souls, err := db.Souls(ctx, gundb.Lex{Prefix: "chat/2024-"})
//	for _, soul := range souls {
//		msg, err := db.Get(soul).Once[Message](ctx)
//	}
//
// It needs a Store that implements SoulStore (the default one does) and
// returns ErrNoSoulStore otherwise.
func (db *DB) Souls(ctx context.Context, q Lex) ([]string, error) {
	ss, ok := db.store.(SoulStore)
	if !ok {
		return nil, ErrNoSoulStore
	}
	if db.ctx.Err() != nil {
		return nil, ErrClosed
	}
	l := q.lex()
	db.query(ctx, &getQuery{SoulLex: l})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return listSouls(ctx, ss, l)
}

// listSouls returns the stored souls l selects, in JS string order: only
// those in its range when the store can list a range (SoulRangeStore).
func listSouls(ctx context.Context, ss SoulStore, l *lex) ([]string, error) {
	var all []string
	var err error
	if rs, ok := ss.(SoulRangeStore); ok && l.Eq == nil && byteOrdered(l.Gt) && byteOrdered(l.Lt) {
		all, err = rs.SoulsBetween(ctx, deref(l.Prefix), deref(l.Gt), deref(l.Lt))
	} else {
		all, err = ss.ListSouls(ctx, deref(l.Prefix))
	}
	if err != nil {
		return nil, err
	}
	souls := slices.DeleteFunc(all, func(soul string) bool { return !l.matchSoul(soul) })
	slices.SortFunc(souls, jsCompare)
	return souls, nil
}

// byteOrdered reports whether comparing any string with bound in byte order
// gives what JS's UTF-16 order gives. The two differ only where a
// character from U+E000 up meets one from U+10000 up; a bound made of
// characters below U+D800 never meets either case at the place two
// strings first differ.
func byteOrdered(bound *string) bool {
	if bound == nil {
		return true
	}
	for _, r := range *bound {
		if r >= 0xD800 {
			return false
		}
	}
	return true
}
