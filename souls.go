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
	all, err := ss.ListSouls(ctx, q.Prefix)
	if err != nil {
		return nil, err
	}
	souls := slices.DeleteFunc(all, func(soul string) bool { return !l.matchSoul(soul) })
	slices.SortFunc(souls, jsCompare)
	return souls, nil
}
