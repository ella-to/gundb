package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	ucli "github.com/urfave/cli/v3"

	"ella.to/gundb"
	"ella.to/gundb/sea"
	"ella.to/gundb/sea/pathmap"
)

// Obfuscated scopes, --obf: a part of a user's space whose names the server
// never sees. ~/vault/bank is stored as ~<pub>/<random root>/<token>, the
// token sealed with the scope's key (package sea/pathmap). The key reaches
// readers in grants: in ~<pub>/pathmap, one field per reader and scope,
// named and encrypted with the secret the owner shares with that reader.
//
// With --obf the path is the real one and the client translates; without
// it, the path is used as typed.

func obfFlag() ucli.Flag {
	return &ucli.BoolFlag{
		Name:    "obf",
		Aliases: []string{"obfuscate"},
		Usage:   "the path is in an obfuscated scope (~/<scope>/...): the server stores tokens, not its names; the first write to one of yours creates it",
	}
}

func rotateFlag() ucli.Flag {
	return &ucli.BoolFlag{
		Name:  "rotate",
		Usage: "with --obf, give the scope a new key and root, and keep only the readers named with --to",
	}
}

// scope is an obfuscated scope as the logged-in user reads it.
type scope struct {
	owner userKeys
	label string
	mine  bool
	key   *pathmap.Key
	grant pathmap.Grant
}

// path returns how the scope is named in messages: ~/label or ~@alias/label.
func (s *scope) path() string {
	if s.mine {
		return "~/" + s.label
	}
	return "~" + strings.TrimPrefix(s.owner.alias, "~") + "/" + s.label // as the path named it
}

// root returns a ref to the scope's root, signed by the user if it is
// theirs, sealed with its key.
func (cl *client) scopeRoot(s *scope) *gundb.Ref {
	if s.mine {
		return cl.user.Soul(s.grant.Root).Sealed(s.key)
	}
	return cl.db.Get(s.grant.Root).Sealed(s.key)
}

// obfRef returns the ref --obf makes of path: the scope's root, sealed,
// then the rest of the path. With create, a write to one of the user's
// own scopes that does not exist yet creates it.
func (cl *client) obfRef(ctx context.Context, path string, create bool) (*gundb.Ref, *scope, error) {
	keys, err := splitPath(path)
	if err != nil {
		return nil, nil, err
	}
	if !strings.HasPrefix(keys[0], "~") {
		return nil, nil, fail("put it in your space: ~/<scope>/..., or drop --obf",
			"--obf works in a user's space, and %s is not in one", path)
	}
	if len(keys) < 2 {
		return nil, nil, fail("name the scope: ~/<scope>/..., like ~/vault/bank", "--obf needs a scope after %s", keys[0])
	}
	s, err := cl.findScope(ctx, keys[0], keys[1], create)
	if err != nil {
		return nil, nil, err
	}
	ref := cl.scopeRoot(s)
	for _, k := range keys[2:] {
		ref = ref.Get(k)
	}
	return ref, s, nil
}

// findScope finds the scope label in the space of space (~, ~@alias or
// ~<pub>) through the user's grant, and creates it if create is set and
// the space is the user's.
func (cl *client) findScope(ctx context.Context, space, label string, create bool) (*scope, error) {
	if cl.user == nil {
		return nil, fail("log in with --username and --password", "--obf needs your keys to find the scope, but you are not logged in")
	}
	owner, err := cl.owner(ctx, space)
	if err != nil {
		return nil, err
	}
	if s := cl.scopes[owner.pub+"/"+label]; s != nil {
		return s, nil
	}
	s := &scope{owner: owner, label: label, mine: owner.pub == cl.user.Pub()}
	g, found, err := cl.readGrant(ctx, owner, label)
	if err != nil {
		return nil, err
	}
	switch {
	case found:
		s.grant = g
	case create && s.mine:
		if s.grant, err = cl.newScope(ctx, label); err != nil {
			return nil, err
		}
	case s.mine:
		return nil, fail("write to it with --obf first, e.g. gundb put '~/"+label+"/name' value --obf", "you have no obfuscated scope %s", s.path())
	default:
		return nil, fail("ask "+strings.TrimPrefix(owner.alias, "~@")+" to share it: gundb put '~/"+label+"' --obf --to <you>",
			"you have no access to an obfuscated %s", s.path())
	}
	if s.key, err = pathmap.KeyFrom(s.grant.Key); err != nil {
		return nil, err
	}
	if cl.scopes == nil {
		cl.scopes = map[string]*scope{}
	}
	cl.scopes[owner.pub+"/"+label] = s
	return s, nil
}

// owner returns the keys of the user whose space space names.
func (cl *client) owner(ctx context.Context, space string) (userKeys, error) {
	if space == "~" {
		p := cl.user.Pair()
		return userKeys{alias: "~@" + cl.user.Alias(), pub: p.Pub, epub: p.EPub}, nil
	}
	return cl.lookUp(ctx, space)
}

// secret returns the secret the user shares with u.
func (cl *client) secret(u userKeys) (string, error) {
	s, err := sea.Secret(u.epub, cl.user.Pair())
	if err != nil {
		return "", fmt.Errorf("could not derive a key shared with %s: %w", u.alias, err)
	}
	return s, nil
}

// readGrant reads the user's grant for label in owner's grants.
func (cl *client) readGrant(ctx context.Context, owner userKeys, label string) (pathmap.Grant, bool, error) {
	secret, err := cl.secret(owner)
	if err != nil {
		return pathmap.Grant{}, false, err
	}
	field := pathmap.GrantField(secret, label)
	sealed, err := cl.db.Get(pathmap.GrantsNode(owner.pub)).Get(field).Once[string](ctx)
	if errors.Is(err, gundb.ErrNotFound) {
		return pathmap.Grant{}, false, nil
	}
	if err != nil {
		return pathmap.Grant{}, false, explain(err, "read your grant for "+label, cl.timeout)
	}
	g, err := pathmap.OpenGrant(sealed, secret)
	if err != nil {
		return pathmap.Grant{}, false, nil // not one for us: as good as none
	}
	return g, true, nil
}

// writeGrant writes g for reader into the user's grants (a null g.Key
// deletes it).
func (cl *client) writeGrant(ctx context.Context, reader userKeys, g *pathmap.Grant, label string) error {
	secret, err := cl.secret(reader)
	if err != nil {
		return err
	}
	var value any // nil deletes
	if g != nil {
		if value, err = pathmap.SealGrant(*g, secret); err != nil {
			return err
		}
	}
	ref := cl.user.Soul(pathmap.GrantsNode(cl.user.Pub())).Get(pathmap.GrantField(secret, label))
	if err := ref.PutAck(ctx, value); err != nil {
		return explain(err, "write the grant for "+reader.alias, cl.timeout)
	}
	return nil
}

func (cl *client) me() userKeys {
	p := cl.user.Pair()
	return userKeys{alias: "~@" + cl.user.Alias(), pub: p.Pub, epub: p.EPub}
}

// newScope creates the user's scope label: a key, a root, and the user's
// own grant.
func (cl *client) newScope(ctx context.Context, label string) (pathmap.Grant, error) {
	key, err := pathmap.NewKey()
	if err != nil {
		return pathmap.Grant{}, err
	}
	root, err := pathmap.NewRoot(cl.user.Pub())
	if err != nil {
		return pathmap.Grant{}, err
	}
	g := pathmap.Grant{Key: key.Bytes(), Root: root, Label: label, Epoch: 1}
	return g, cl.writeGrant(ctx, cl.me(), &g, label)
}

// guardScope refuses a write without --obf to one of the user's own
// scopes: it would store the scope's names in clear, next to the tokens.
func (cl *client) guardScope(ctx context.Context, path string) error {
	if cl.user == nil {
		return nil
	}
	keys, err := splitPath(path)
	if err != nil || len(keys) < 2 {
		return nil
	}
	space, err := cl.spaceOf(ctx, path)
	if err != nil || space != cl.user.Pub() {
		return nil
	}
	if _, found, err := cl.readGrant(ctx, cl.me(), keys[1]); err != nil || !found {
		return err
	}
	return fail("run it again with --obf", "~/%s is obfuscated: without --obf its names would be stored in clear", keys[1])
}

// grant gives the users in --to (comma-separated) a grant for the scope
// of path, which must be the user's own.
func (cl *client) grant(ctx context.Context, path, to string) (string, error) {
	_, s, err := cl.obfRef(ctx, path, true)
	if err != nil {
		return "", err
	}
	if !s.mine {
		return "", fail("only the owner shares a scope", "%s is not yours to share", s.path())
	}
	var names []string
	for who := range strings.SplitSeq(to, ",") {
		if who = strings.TrimSpace(who); who == "" {
			continue
		}
		u, err := cl.lookUp(ctx, who)
		if err != nil {
			return "", err
		}
		g := s.grant
		g.Readers = nil
		if err := cl.writeGrant(ctx, u, &g, s.label); err != nil {
			return "", err
		}
		if !slices.Contains(s.grant.Readers, u.pub) {
			s.grant.Readers = append(s.grant.Readers, u.pub)
		}
		names = append(names, strings.TrimPrefix(who, "~@"))
	}
	if err := cl.writeGrant(ctx, cl.me(), &s.grant, s.label); err != nil { // the owner's list of readers
		return "", err
	}
	return strings.Join(names, ", "), nil
}

// listScopes returns the user's own scopes: label -> root and readers.
func (cl *client) listScopes(ctx context.Context) (map[string]any, error) {
	if cl.user == nil {
		return nil, fail("log in with --username and --password", "--obf lists your scopes, but you are not logged in")
	}
	n, err := cl.db.Get(pathmap.GrantsNode(cl.user.Pub())).Node(ctx)
	if errors.Is(err, gundb.ErrNotFound) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, explain(err, "read your scopes", cl.timeout)
	}
	secret, err := cl.secret(cl.me())
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, v := range n.Fields {
		s, ok := v.(gundb.String)
		if !ok {
			continue
		}
		g, err := pathmap.OpenGrant(string(s), secret)
		if err != nil {
			continue // a grant for someone else
		}
		readers := make([]any, 0, len(g.Readers))
		for _, pub := range g.Readers {
			readers = append(readers, pub)
		}
		out[g.Label] = map[string]any{"soul": g.Root, "readers": readers}
	}
	return out, nil
}

// mapNode returns, for get --obf --links, the node at ref as it is
// stored: each real name with the field it is stored under, and its value
// or the soul it links to.
func mapNode(ctx context.Context, ref *gundb.Ref, s *scope, path string) (map[string]any, error) {
	n, err := ref.Node(ctx)
	switch {
	case err != nil && strings.Contains(err.Error(), "not a node"):
		return nil, fail("drop --links to read it, or name its node", "%s is a value, not a node: --links shows nodes", path)
	case err != nil:
		return nil, err
	}
	out := map[string]any{"_": map[string]any{"#": n.Soul}}
	for _, k := range slices.Sorted(maps.Keys(n.Fields)) {
		entry := map[string]any{"field": s.key.Seal(n.Soul, k)}
		switch v := n.Fields[k].(type) {
		case gundb.Null:
			continue
		case gundb.Link:
			entry["#"] = v.Soul
		case gundb.String:
			entry["value"] = string(v)
		case gundb.Number:
			entry["value"] = float64(v)
		case gundb.Bool:
			entry["value"] = bool(v)
		}
		out[k] = entry
	}
	return out, nil
}

// rotate gives the scope of path a new key and root: it copies the data
// there, encrypted values encrypted again with the new key, deletes it
// from the old root, and writes grants for the user and the users in to
// only. Everyone else's grant is deleted.
func (cl *client) rotate(ctx context.Context, path, to string) (string, error) {
	keys, err := splitPath(path)
	if err != nil {
		return "", err
	}
	if len(keys) != 2 {
		return "", fail("rotate the whole scope: gundb put '~/"+keys[min(1, len(keys)-1)]+"' --obf --rotate", "--rotate takes a scope, like ~/vault")
	}
	_, s, err := cl.obfRef(ctx, path, false)
	if err != nil {
		return "", err
	}
	if !s.mine {
		return "", fail("only the owner rotates a scope", "%s is not yours", s.path())
	}
	old := cl.scopeRoot(s)
	var tree map[string]any
	raw, err := old.Once[json.RawMessage](ctx)
	switch {
	case errors.Is(err, gundb.ErrNotFound):
	case err != nil:
		return "", explain(err, "read "+s.path(), cl.timeout)
	default:
		v, err := decode(raw)
		if err != nil {
			return "", err
		}
		tree, _ = v.(map[string]any)
	}

	key, err := pathmap.NewKey()
	if err != nil {
		return "", err
	}
	root, err := pathmap.NewRoot(cl.user.Pub())
	if err != nil {
		return "", err
	}
	if err := reencrypt(tree, s.key.ValueKey(), key.ValueKey()); err != nil {
		return "", err
	}
	if len(tree) > 0 {
		if err := cl.user.Soul(root).Sealed(key).PutAck(ctx, tree); err != nil {
			return "", explain(err, "write the new "+s.path(), cl.timeout)
		}
	}
	nodes, err := cl.deleteTree(ctx, s.grant.Root)
	if err != nil {
		return "", err
	}

	// Grants: the owner's and those named in to; the others are deleted.
	keep := map[string]userKeys{}
	for who := range strings.SplitSeq(to, ",") {
		if who = strings.TrimSpace(who); who != "" {
			u, err := cl.lookUp(ctx, who)
			if err != nil {
				return "", err
			}
			keep[u.pub] = u
		}
	}
	g := pathmap.Grant{Key: key.Bytes(), Root: root, Label: s.label, Epoch: s.grant.Epoch + 1}
	var dropped []string
	for _, pub := range s.grant.Readers {
		if _, ok := keep[pub]; ok {
			continue
		}
		u, err := cl.lookUp(ctx, pub)
		if err != nil {
			return "", err
		}
		if err := cl.writeGrant(ctx, u, nil, s.label); err != nil {
			return "", err
		}
		dropped = append(dropped, cl.aliasOf(ctx, pub))
	}
	for pub, u := range keep {
		reader := g
		if err := cl.writeGrant(ctx, u, &reader, s.label); err != nil {
			return "", err
		}
		g.Readers = append(g.Readers, pub)
	}
	if err := cl.writeGrant(ctx, cl.me(), &g, s.label); err != nil {
		return "", err
	}
	delete(cl.scopes, cl.user.Pub()+"/"+s.label)

	msg := fmt.Sprintf("new key and root, %d node(s) cleared from the old one", nodes)
	if len(keep) > 0 {
		msg += fmt.Sprintf("; %d reader(s) kept", len(keep))
	}
	if len(dropped) > 0 {
		msg += "; dropped " + strings.Join(dropped, ", ")
	}
	return msg, nil
}

// reencrypt replaces every value in tree encrypted with from by the same
// value encrypted with to.
func reencrypt(tree map[string]any, from, to string) error {
	for k, v := range tree {
		switch v := v.(type) {
		case map[string]any:
			if err := reencrypt(v, from, to); err != nil {
				return err
			}
		case string:
			if seaKind(v) != "encrypted" {
				continue
			}
			plain, err := sea.Decrypt(v, from)
			if err != nil {
				continue // not encrypted with the scope's key: left as it is
			}
			p, err := decode(plain)
			if err != nil {
				return err
			}
			if tree[k], err = sea.Encrypt(p, to); err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteTree deletes every field of the node soul and of the nodes it
// links to in the user's space, in one write, and returns how many nodes
// it cleared.
func (cl *client) deleteTree(ctx context.Context, soul string) (int, error) {
	space := "~" + cl.user.Pub() + "/"
	b := cl.db.Batch()
	seen := map[string]bool{}
	var walk func(soul string) error
	walk = func(soul string) error {
		if seen[soul] || !strings.HasPrefix(soul, space) {
			return nil
		}
		seen[soul] = true
		n, err := cl.db.Get(soul).Node(ctx)
		if errors.Is(err, gundb.ErrNotFound) {
			return nil
		}
		if err != nil {
			return explain(err, "read "+soul, cl.timeout)
		}
		nulls := map[string]any{}
		for k, v := range n.Fields {
			if _, deleted := v.(gundb.Null); deleted {
				continue
			}
			nulls[k] = nil
			if l, ok := v.(gundb.Link); ok {
				if err := walk(l.Soul); err != nil {
					return err
				}
			}
		}
		if len(nulls) > 0 {
			return b.Put(ctx, cl.user.Soul(soul), nulls)
		}
		return nil
	}
	if err := walk(soul); err != nil {
		return 0, err
	}
	cleared := b.Len()
	if cleared == 0 {
		return 0, nil
	}
	if err := b.CommitAck(ctx); err != nil {
		return 0, explain(err, "clear the old scope", cl.timeout)
	}
	return cleared, nil
}

// aliasOf returns the alias of the user with key pub, for messages, or
// the start of the key if it is not found.
func (cl *client) aliasOf(ctx context.Context, pub string) string {
	if alias, err := cl.db.User(pub).Get("alias").Once[string](ctx); err == nil && alias != "" {
		return alias
	}
	if len(pub) > 12 {
		return pub[:12] + "..."
	}
	return pub
}
