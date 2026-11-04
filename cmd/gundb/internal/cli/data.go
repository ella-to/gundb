package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	ucli "github.com/urfave/cli/v3"

	"ella.to/gundb"
)

func (a *App) getCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "get",
		Usage:     "read the value at a path, as JSON",
		ArgsUsage: "<path>",
		Description: `Prints the value at path, with linked nodes filled in.

  gundb get users/1
  gundb get users/1/name
  gundb get ~/notes --dec -u ali -p secret123      # decrypt your own notes
  gundb get ~@ali/profile                          # ali's public profile
  gundb get inbox/bob --dec --from ali -u bob -p … # decrypt what ali sent you
  gundb get users/1 --links                        # links unfollowed, and the soul`,
		Flags:        clientFlags(decFlag(), verifyFlag(), fromFlag(), linksFlag()),
		OnUsageError: usageError,
		Action: func(ctx context.Context, c *ucli.Command) error {
			arg, err := args(c, "path")
			if err != nil {
				return err
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
			o, err := cl.newOpener(ctx, c)
			if err != nil {
				return err
			}
			var v any
			if c.Bool("links") {
				node, err := cl.storedNode(ctx, ref, path)
				if err != nil {
					return err
				}
				if v, err = o.walk(node, path); err != nil {
					return err
				}
			} else {
				raw, err := ref.Once[json.RawMessage](ctx)
				if err != nil {
					return explain(err, "read "+path, cl.timeout)
				}
				if v, err = o.open(raw, path); err != nil {
					return err
				}
			}
			fmt.Fprintln(c.Root().Writer, encode(v, true))
			o.note(c.Root().ErrWriter)
			return nil
		},
	}
}

// writeFlags are the flags of put and set.
func writeFlags() []ucli.Flag {
	return clientFlags(stringFlag(), signFlag(), encFlag(), toFlag(), linkFlag())
}

const writeExamples = `
Values are JSON when they parse as JSON (42, true, null, {"a":1}, "quoted"),
text otherwise; --string always stores text, and - reads the value from stdin.
--link <path> takes the place of the value: it stores a link to that node.`

func (a *App) putCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "put",
		Usage:     "write a value at a path",
		ArgsUsage: "<path> <value>",
		Description: `Writes value at path and waits until the server has it.

  gundb put users/1/name 'Ali'
  gundb put users/1 '{"name":"Ali","age":30}'
  gundb put users/1/age 30                  # a number; --string for "30"
  gundb put users/1/name null               # delete
  gundb put users/1 - < user.json
  gundb put ~/notes/1 'secret' --enc -u ali -p secret123
  gundb put inbox/bob 'hi Bob' --enc --to bob --sign -u ali -p secret123
  gundb put users/1/best --link users/2     # a link to another node
` + writeExamples,
		Flags:        writeFlags(),
		OnUsageError: usageError,
		Action: func(ctx context.Context, c *ucli.Command) error {
			return a.write(ctx, c, "put", true, func(ctx context.Context, cl *client, ref *gundb.Ref, path string, v any) (string, error) {
				return path, ref.PutAck(ctx, v)
			})
		},
	}
}

func (a *App) setCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "set",
		Usage:     "add a value to the list at a path (gun.set)",
		ArgsUsage: "<path> <value>",
		Description: `Adds value to the unordered list at path, under a new random key, and
prints the path of the new item.

  gundb set users/1/tags go
  gundb set todos '{"title":"buy milk","done":false}'
  gundb get todos                           # the whole list
  gundb set users/1/friends --link users/2  # a list of links; remove with unset
` + writeExamples,
		Flags:        writeFlags(),
		OnUsageError: usageError,
		Action: func(ctx context.Context, c *ucli.Command) error {
			return a.write(ctx, c, "add to", false, func(ctx context.Context, cl *client, ref *gundb.Ref, path string, v any) (string, error) {
				b := cl.db.Batch() // a Batch, for its CommitAck: Set does not wait
				item, err := b.Set(ctx, ref, v)
				if err != nil {
					return "", err
				}
				if err := b.CommitAck(ctx); err != nil {
					return "", err
				}
				if c.IsSet("link") {
					return path, nil // the item's key is the soul, already shown
				}
				return strings.TrimSuffix(path, "/") + "/" + item.Key(), nil
			})
		},
	}
}

// write runs put or set: parse, connect, seal, write, report.
// With put, a top-level path must get an object: a root node holds fields.
// With --link, the value is a link to the node it names.
func (a *App) write(ctx context.Context, c *ucli.Command, verb string, put bool,
	do func(ctx context.Context, cl *client, ref *gundb.Ref, path string, v any) (string, error)) error {
	target := unexpandHome(c.String("link"))
	if target != "" {
		return a.writeLink(ctx, c, verb, put, target, do)
	}
	arg, err := args(c, "path", "value")
	if err != nil {
		return err
	}
	path := unexpandHome(arg[0])
	v, err := parseValue(arg[1], c.Bool("string"), c.Root().Reader)
	if err != nil {
		return err
	}
	keys, err := splitPath(path)
	if err != nil {
		return err
	}
	if _, isObject := v.(map[string]any); put && len(keys) == 1 && (!isObject || c.Bool("enc") || c.Bool("sign")) {
		return fail(fmt.Sprintf("put a field: gundb put %s/name %s, or an object: gundb put %s '{\"name\":...}'", path, arg[1], path),
			"%s is a top-level node: it holds fields, not a single value", path)
	}
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
	shown := short(v)
	sealed, err := cl.seal(ctx, c, v)
	if err != nil {
		return err
	}
	switch {
	case c.Bool("enc") && c.Bool("sign"):
		shown = "(encrypted, signed)"
	case c.Bool("enc"):
		shown = "(encrypted)"
	case c.Bool("sign"):
		shown += " (signed)"
	}
	at, err := do(ctx, cl, ref, path, sealed)
	if err != nil {
		return explain(err, strings.Replace(verb, "put", "write", 1)+" "+path, cl.timeout)
	}
	fmt.Fprintf(c.Root().Writer, "ok  %s = %s\n", at, shown)
	return nil
}

// writeLink runs put --link and set --link: it puts or sets a ref to the
// node target names, which the library stores as a link.
func (a *App) writeLink(ctx context.Context, c *ucli.Command, verb string, put bool, target string,
	do func(ctx context.Context, cl *client, ref *gundb.Ref, path string, v any) (string, error)) error {
	if c.NArg() == 2 {
		return fail("drop the value, or drop --link", "--link takes the place of the value, but %q was given too", c.Args().Get(1))
	}
	arg, err := args(c, "path")
	if err != nil {
		return err
	}
	if err := checkLink(c); err != nil {
		return err
	}
	path := unexpandHome(arg[0])
	keys, err := splitPath(path)
	if err != nil {
		return err
	}
	if put && len(keys) == 1 {
		return fail(fmt.Sprintf("link from a field: gundb put %s/<field> --link %s, or add it to a list: gundb set %s --link %s", path, target, path, target),
			"%s is a top-level node: it holds fields, not a link", path)
	}
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
	to, soul, err := cl.linkTarget(ctx, target)
	if err != nil {
		return err
	}
	at, err := do(ctx, cl, ref, path, to)
	if err != nil {
		return explain(err, strings.Replace(verb, "put", "write", 1)+" "+path, cl.timeout)
	}
	how := "links to"
	if !put {
		how = "now has a link to"
	}
	fmt.Fprintf(c.Root().Writer, "ok  %s %s %s\n", at, how, linkTo(target, soul))
	return nil
}

func (a *App) subCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "sub",
		Usage:     "watch a path and print every change until Ctrl-C",
		ArgsUsage: "<path>",
		Description: `Prints the value at path now, and again each time it changes, one line
of JSON per change. With --map, prints each item of a list or node as it
changes instead of the whole value.

  gundb sub users
  gundb sub todos --map
  gundb sub ~/notes --map --dec -u ali -p secret123

--timeout only bounds connecting; sub runs until you stop it.`,
		Flags: clientFlags(decFlag(), verifyFlag(), fromFlag(), &ucli.BoolFlag{
			Name:    "map",
			Aliases: []string{"m"},
			Usage:   "print each item that changes (key and value), not the whole value",
		}),
		OnUsageError: usageError,
		Action: func(ctx context.Context, c *ucli.Command) error {
			arg, err := args(c, "path")
			if err != nil {
				return err
			}
			path := unexpandHome(arg[0])
			log, _ := a.logger(c)
			setup, cancel := operation(ctx, c)
			defer cancel()
			cl, err := a.connect(setup, c, log, true)
			if err != nil {
				return err
			}
			defer cl.close()
			ref, err := cl.ref(setup, path)
			if err != nil {
				return err
			}
			o, err := cl.newOpener(setup, c)
			if err != nil {
				return err
			}

			out := c.Root().Writer
			var mu sync.Mutex // On and Map().On call back from their own goroutines
			show := func(key string, raw json.RawMessage) {
				v, err := o.open(raw, strings.TrimSuffix(path+"/"+key, "/"))
				mu.Lock()
				defer mu.Unlock()
				stamp := time.Now().Format(time.TimeOnly)
				if err != nil {
					printError(c.Root().ErrWriter, err)
					return
				}
				if key != "" {
					stamp += "  " + key
				}
				fmt.Fprintf(out, "%s  %s\n", stamp, encode(v, false))
			}
			var off func()
			if c.Bool("map") {
				off = ref.Map().On(func(key string, raw json.RawMessage) { show(key, raw) })
			} else {
				off = ref.On(func(raw json.RawMessage) { show("", raw) })
			}
			log.Info("watching "+path+" (Ctrl-C to stop)", "addr", cl.addr)
			<-ctx.Done()
			off()
			return nil
		},
	}
}
