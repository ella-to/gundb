package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	ucli "github.com/urfave/cli/v3"

	"ella.to/gundb"
)

// Links between nodes: put --link, set --link, unset and get --links.

func linkFlag() ucli.Flag {
	return &ucli.StringFlag{
		Name:  "link",
		Usage: "store a link to the node at this `path` (or with this soul) instead of a value",
	}
}

func linksFlag() ucli.Flag {
	return &ucli.BoolFlag{
		Name:  "links",
		Usage: `show the node as stored: its soul under "_", links as {"#": soul} instead of filled in`,
	}
}

// checkLink rejects the flags that make no sense with --link: a link is
// not a value, so it is not text, encrypted or signed.
func checkLink(c *ucli.Command) error {
	for _, f := range []string{"string", "enc", "sign", "to"} {
		if c.IsSet(f) {
			return fail("drop --"+f+": a link points at a node, it holds no value of its own", "--%s does not work with --link", f)
		}
	}
	return nil
}

// linkTarget returns a ref to the node at path, to link to, and its soul
// for messages. It checks with the server that a node is there, so a typo
// does not leave a link to nowhere; Put and Set then link to the soul the
// node has, which is not always its path (nodes added with set, or written
// by GUN.js, have random souls). path can also be a soul itself, as
// get --links prints it.
func (cl *client) linkTarget(ctx context.Context, path string) (*gundb.Ref, string, error) {
	keys, err := splitPath(path)
	if err != nil {
		return nil, "", err
	}
	ref, err := cl.ref(ctx, path)
	if err != nil {
		return nil, "", err
	}
	n, err := ref.Node(ctx)
	if errors.Is(err, gundb.ErrNotFound) && len(keys) > 1 {
		// A soul with slashes in it, like users/2 or ~<pub>/<id>.
		ref = cl.db.Get(strings.TrimSuffix(path, "/"))
		n, err = ref.Node(ctx)
	}
	switch {
	case errors.Is(err, gundb.ErrNotFound):
		return nil, "", fail("write it first, e.g. gundb put "+path+" '{\"name\":...}', or check the path with gundb get",
			"nothing at %s to link to", path)
	case err != nil && strings.Contains(err.Error(), "not a node"):
		parent := path[:max(strings.LastIndex(path, "/"), 0)]
		hint := "link to a node (an object), not to one of its fields"
		if parent != "" {
			hint += ", like --link " + parent
		}
		return nil, "", fail(hint, "%s is a value, not a node", path)
	case err != nil:
		return nil, "", explain(err, "read "+path, cl.timeout)
	}
	return ref, n.Soul, nil
}

// linkTo describes a link for messages: the path as given, and the soul
// too when it is not the same.
func linkTo(path, soul string) string {
	if strings.TrimSuffix(path, "/") == soul {
		return path
	}
	return fmt.Sprintf("%s (soul %s)", path, soul)
}

func (a *App) unsetCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "unset",
		Usage:     "remove a link from the list at a path (gun.unset)",
		ArgsUsage: "<path> --link <path>",
		Description: `Removes the link to a node from the list at path: the opposite of
set --link. The node itself is left alone.

  gundb set users/1/friends --link users/2     # add
  gundb unset users/1/friends --link users/2   # remove

A link stored with put is removed with null: gundb put users/1/best null`,
		Flags:        clientFlags(linkFlag()),
		OnUsageError: usageError,
		Action: func(ctx context.Context, c *ucli.Command) error {
			arg, err := args(c, "path")
			if err != nil {
				return err
			}
			target := unexpandHome(c.String("link"))
			if target == "" {
				return fail("name the node to remove: gundb unset "+arg[0]+" --link <path>", "unset needs --link")
			}
			path := unexpandHome(arg[0])
			ctx, cancel := operation(ctx, c)
			defer cancel()
			log, _ := a.logger(c)
			cl, err := a.connect(ctx, c, log, true)
			if err != nil {
				return err
			}
			defer cl.close()
			ref, err := cl.ref(ctx, path)
			if err != nil {
				return err
			}
			_, soul, err := cl.linkTarget(ctx, target)
			if err != nil {
				return err
			}
			// A set keys each link by its soul.
			if err := ref.Get(soul).PutAck(ctx, nil); err != nil {
				return explain(err, "write "+path, cl.timeout)
			}
			fmt.Fprintf(c.Root().Writer, "ok  %s no longer links to %s\n", path, linkTo(target, soul))
			return nil
		},
	}
}

// storedNode reads the node at ref as it is stored, for get --links: its
// soul under "_" (like GUN's wire format), links as {"#": soul}, deleted
// fields left out.
func (cl *client) storedNode(ctx context.Context, ref *gundb.Ref, path string) (map[string]any, error) {
	n, err := ref.Node(ctx)
	switch {
	case err != nil && strings.Contains(err.Error(), "not a node"):
		return nil, fail("drop --links to read it, or name its node", "%s is a value, not a node: --links shows nodes", path)
	case err != nil:
		return nil, explain(err, "read "+path, cl.timeout)
	}
	out := map[string]any{"_": map[string]any{"#": n.Soul}}
	for _, k := range slices.Sorted(maps.Keys(n.Fields)) {
		switch v := n.Fields[k].(type) {
		case gundb.Null:
		case gundb.Link:
			out[k] = map[string]any{"#": v.Soul}
		case gundb.String:
			out[k] = string(v)
		case gundb.Number:
			out[k] = float64(v)
		case gundb.Bool:
			out[k] = bool(v)
		}
	}
	return out, nil
}
