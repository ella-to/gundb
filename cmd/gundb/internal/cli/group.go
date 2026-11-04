package cli

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	ucli "github.com/urfave/cli/v3"

	"ella.to/gundb"
	"ella.to/gundb/sea"
	"ella.to/gundb/sea/senderkey"
)

// Encrypted group chat with sender keys (a ratchet), as in
// examples/senderkeys. In the graph, all signed by whoever owns the space:
//
//	~owner/groups/<owner>/<id>/members/<pub>      true for each member
//	~member/prekey                                the member's prekey (an epub)
//	~member/groups/<owner>/<id>/chain             the member's current chain ID
//	~member/groups/<owner>/<id>/keys/<chain>/<pub> that chain, sealed for <pub>
//	~member/groups/<owner>/<id>/messages/<id>     a senderkey.Message
//
// On the device only (device.go): prekeys, chains and history.

func (a *App) groupCommand() *ucli.Command {
	flags := func(more ...ucli.Flag) []ucli.Flag { return clientFlags(append([]ucli.Flag{dirFlag()}, more...)...) }
	return &ucli.Command{
		Name:  "group",
		Usage: "encrypted group chat: every message has its own key (a ratchet)",
		Description: `Group chat with sender keys, the scheme Signal and WhatsApp use for
groups. Every message has its own key. New members can't read what was
said before they joined, removed members can't read what comes next, and
neither your password alone nor a copy of this device reads what you
already read.

The keys stay on this device, in --dir, encrypted with your password.
Run init once per user and device, before anything else:

  gundb group init -u alice -p …            # each person, once
  gundb group create team -u alice -p …
  gundb group add team bob carol -u alice -p …
  gundb group send team 'hi all' -u alice -p …
  gundb group read alice/team -u bob -p …   # others name it owner/group
  gundb group sub alice/team -u carol -p …  # live, Ctrl-C to stop

Set GUNDB_USERNAME and GUNDB_PASSWORD to stop repeating -u and -p.`,
		Commands: []*ucli.Command{
			{
				Name:         "init",
				Usage:        "set up this device for group chats: run it once per user",
				Flags:        flags(),
				OnUsageError: usageError,
				Action: a.groupAction(0, func(ctx context.Context, g *groupRun) error {
					if err := g.dev.update(true, func(st *deviceState) error {
						if len(st.Prekeys) > 0 {
							return fail("this device is ready: create or join a group", "already set up in %s", g.dev.path)
						}
						p, err := sea.NewPair()
						if err != nil {
							return err
						}
						st.Prekeys[p.EPub] = p
						return g.me.Get("prekey").PutAck(ctx, p.EPub)
					}); err != nil {
						return explain(err, "publish your group key", g.cl.timeout)
					}
					fmt.Fprintf(g.out, "ok  %s can now create and join groups (keys in %s)\n", g.me.Alias(), g.dev.path)
					return nil
				}),
			},
			{
				Name:         "create",
				Usage:        "create a group that you own",
				ArgsUsage:    "<name>",
				Flags:        flags(),
				OnUsageError: usageError,
				Action: a.groupAction(1, func(ctx context.Context, g *groupRun) error {
					if !g.isOwner() {
						return fail("name it without an owner: gundb group create "+g.id, "you can only create your own groups")
					}
					members := g.members()
					if _, err := members.Once[map[string]any](ctx); err == nil {
						return fail("pick another name, or add people with: gundb group add "+g.id+" <user>", "you already have a group named %s", g.id)
					}
					if err := members.PutAck(ctx, map[string]any{g.me.Pub(): true}); err != nil {
						return explain(err, "create "+g.label, g.cl.timeout)
					}
					if err := g.update(ctx, nil); err != nil {
						return err
					}
					fmt.Fprintf(g.out, "ok  created %s; others name it %s\n", g.id, g.label)
					fmt.Fprintf(g.errw, "next:  gundb group add %s <user>...\n", g.id)
					return nil
				}),
			},
			{
				Name:         "add",
				Usage:        "add people to a group you own",
				ArgsUsage:    "<group> <user>...",
				Flags:        flags(),
				OnUsageError: usageError,
				Action:       a.groupAction(-1, func(ctx context.Context, g *groupRun) error { return g.setMembers(ctx, true) }),
			},
			{
				Name:         "remove",
				Usage:        "remove people from a group you own; they can't read what comes next",
				ArgsUsage:    "<group> <user>...",
				Flags:        flags(),
				OnUsageError: usageError,
				Action:       a.groupAction(-1, func(ctx context.Context, g *groupRun) error { return g.setMembers(ctx, false) }),
			},
			{
				Name:         "members",
				Usage:        "list the members of a group",
				ArgsUsage:    "<group>",
				Flags:        flags(),
				OnUsageError: usageError,
				Action: a.groupAction(1, func(ctx context.Context, g *groupRun) error {
					pubs, err := g.memberList(ctx)
					if err != nil {
						return err
					}
					var lines []string
					for _, pub := range pubs {
						line := g.name(ctx, pub)
						if pub == g.owner {
							line += "  (owner)"
						}
						if _, err := g.cl.db.User(pub).Get("prekey").Once[string](ctx); err != nil {
							line += "  (has not run gundb group init)"
						}
						lines = append(lines, line)
					}
					slices.Sort(lines)
					for _, l := range lines {
						fmt.Fprintln(g.out, l)
					}
					return nil
				}),
			},
			{
				Name:         "send",
				Usage:        "send a message to a group",
				ArgsUsage:    "<group> <text>",
				Description:  "Encrypts text with the next key of your chain and adds it to your feed.\n- reads the text from stdin.",
				Flags:        flags(),
				OnUsageError: usageError,
				Action: a.groupAction(2, func(ctx context.Context, g *groupRun) error {
					text := g.args[0]
					if text == "-" {
						b, err := io.ReadAll(g.c.Root().Reader)
						if err != nil {
							return err
						}
						text = strings.TrimSuffix(string(b), "\n")
					}
					var msg senderkey.Message
					id := fmt.Sprintf("%020d-%s", time.Now().UnixNano(), rand.Text()[:6])
					// The step of the chain is saved before the message is
					// sent: a lost message is better than a reused key.
					if err := g.update(ctx, func(gs *groupState) error {
						at := time.Now().UnixMilli()
						var err error
						if msg, err = gs.Sender.Encrypt(map[string]any{"text": text, "at": at}); err != nil {
							return err
						}
						gs.Seen[g.me.Pub()+"/"+id] = true
						gs.Log = append(gs.Log, logLine{ID: id, From: g.me.Pub(), At: at, Text: text})
						return nil
					}); err != nil {
						return err
					}
					if err := g.mine().Get("messages").Get(id).PutAck(ctx, msg); err != nil {
						return explain(err, "send to "+g.label, g.cl.timeout)
					}
					fmt.Fprintf(g.out, "ok  sent to %s\n", g.label)
					return nil
				}),
			},
			{
				Name:      "read",
				Usage:     "print a group's messages",
				ArgsUsage: "<group>",
				Description: `Fetches new messages and prints the conversation, oldest first. A message
can be decrypted only once, so what was said is kept on this device.`,
				Flags:        flags(&ucli.BoolFlag{Name: "new", Usage: "print only the messages that arrived since the last read"}),
				OnUsageError: usageError,
				Action: a.groupAction(1, func(ctx context.Context, g *groupRun) error {
					var lines []logLine
					if err := g.update(ctx, func(gs *groupState) error {
						fresh, err := g.pull(ctx, gs)
						lines = gs.Log
						if g.c.Bool("new") {
							lines = fresh
						}
						return err
					}); err != nil {
						return err
					}
					slices.SortStableFunc(lines, func(a, b logLine) int { return cmp.Compare(a.At, b.At) })
					for _, l := range lines {
						g.print(ctx, l)
					}
					return nil
				}),
			},
			{
				Name:      "sub",
				Usage:     "print a group's messages as they arrive, until Ctrl-C",
				ArgsUsage: "<group>",
				Description: `Prints the conversation so far, then each message as it arrives. It
also shares your chain with people as they are added, and starts a new
one when someone is removed.

--timeout only bounds connecting; sub runs until you stop it.`,
				Flags:        flags(),
				OnUsageError: usageError,
				Action:       a.groupAction(1, func(ctx context.Context, g *groupRun) error { return g.sub(ctx) }),
			},
			{
				Name:         "ls",
				Usage:        "list the groups on this device",
				Flags:        flags(),
				OnUsageError: usageError,
				Action: a.groupAction(0, func(ctx context.Context, g *groupRun) error {
					var labels []string
					err := g.dev.update(false, func(st *deviceState) error {
						for _, gs := range st.Groups {
							labels = append(labels, fmt.Sprintf("%s  (%d messages)", gs.Label, len(gs.Log)))
						}
						return nil
					})
					if err != nil {
						return g.explainDevice(err)
					}
					slices.Sort(labels)
					if len(labels) == 0 {
						fmt.Fprintln(g.errw, "note:  no groups on this device yet: gundb group create <name>, or read one you were added to")
					}
					for _, l := range labels {
						fmt.Fprintln(g.out, l)
					}
					return nil
				}),
			},
		},
	}
}

// groupRun is one run of a group command, logged in, with its group.
type groupRun struct {
	c         *ucli.Command
	cl        *client
	me        *gundb.User
	dev       *device
	prekeys   map[string]*sea.Pair // this device's, while update runs
	log       *slog.Logger
	out, errw io.Writer
	owner, id string   // the group: its owner's pub, and its name
	label     string   // owner/name, as people type it
	args      []string // the arguments after the group
}

// groupAction connects and logs in, finds the group named by the first
// argument (unless nargs is 0), and runs fn. nargs counts the group; -1
// means the group and one or more users.
func (a *App) groupAction(nargs int, fn func(ctx context.Context, g *groupRun) error) ucli.ActionFunc {
	return func(ctx context.Context, c *ucli.Command) error {
		got := c.Args().Slice()
		switch {
		case nargs == -1 && len(got) < 2:
			return withHint(fmt.Errorf("%s needs a group and at least one user", c.FullName()), "usage: %s <group> <user>...", c.FullName())
		case nargs == 1:
			if _, err := args(c, "group"); err != nil {
				return err
			}
		case nargs == 2:
			if _, err := args(c, "group", "text"); err != nil {
				return err
			}
		case nargs == 0 && len(got) > 0:
			return withHint(fmt.Errorf("%s takes no arguments", c.FullName()), "usage: %s -u <user> -p <password>", c.FullName())
		}
		if c.String("username") == "" {
			return fail("log in with --username and --password", "group chats need your keys, but you are not logged in")
		}
		run := ctx
		if c.Name != "sub" {
			var cancel context.CancelFunc
			run, cancel = operation(ctx, c)
			defer cancel()
		}
		setup, cancel := operation(ctx, c)
		defer cancel()
		log, _ := a.logger(c)
		cl, err := a.connect(setup, c, log, true)
		if err != nil {
			return err
		}
		defer cl.close()
		dev, err := deviceFor(c, cl, c.String("password"))
		if err != nil {
			return err
		}
		g := &groupRun{c: c, cl: cl, me: cl.user, dev: dev, log: log, out: c.Root().Writer, errw: c.Root().ErrWriter}
		if len(got) > 0 {
			if err := g.find(setup, got[0]); err != nil {
				return err
			}
			g.args = got[1:]
		}
		return fn(run, g)
	}
}

// find sets the group from "name" (your own) or "owner/name".
func (g *groupRun) find(ctx context.Context, arg string) error {
	owner, id, ok := strings.Cut(arg, "/")
	if !ok {
		owner, id = g.me.Alias(), arg
	}
	if id == "" || strings.Contains(id, "/") {
		return fail("name a group as name (yours) or owner/name, like alice/team", "bad group %q", arg)
	}
	k, err := g.cl.lookUp(ctx, owner)
	if err != nil {
		return err
	}
	g.owner, g.id = k.pub, id
	g.label = strings.TrimPrefix(strings.TrimPrefix(owner, "~"), "@") + "/" + id
	return nil
}

func (g *groupRun) isOwner() bool { return g.owner == g.me.Pub() }

// node is the group's node in the space of the member pub.
func (g *groupRun) node(pub string) *gundb.Ref {
	return g.cl.db.User(pub).Get("groups").Get(g.owner).Get(g.id)
}

// mine is the group's node in my space, for writing.
func (g *groupRun) mine() *gundb.Ref { return g.me.Get("groups").Get(g.owner).Get(g.id) }

// members is the member list, in the owner's space.
func (g *groupRun) members() *gundb.Ref {
	if g.isOwner() {
		return g.mine().Get("members")
	}
	return g.node(g.owner).Get("members")
}

func (g *groupRun) memberList(ctx context.Context) ([]string, error) {
	list, err := g.members().Once[map[string]bool](ctx)
	if errors.Is(err, gundb.ErrNotFound) {
		return nil, fail("check the name; the owner creates it with: gundb group create <name>", "there is no group %s", g.label)
	}
	if err != nil {
		return nil, explain(err, "read the members of "+g.label, g.cl.timeout)
	}
	var pubs []string
	for pub, in := range list {
		if in {
			pubs = append(pubs, pub)
		}
	}
	slices.Sort(pubs)
	return pubs, nil
}

// name returns the alias of the user pub, or a short pub.
func (g *groupRun) name(ctx context.Context, pub string) string {
	if pub == g.me.Pub() {
		return g.me.Alias()
	}
	if alias, err := g.cl.db.User(pub).Get("alias").Once[string](ctx); err == nil {
		return alias
	}
	return pub[:min(len(pub), 12)] + "…"
}

func (g *groupRun) explainDevice(err error) error {
	if errors.Is(err, errNoDevice) {
		return fail(fmt.Sprintf("run once on this device: gundb group init -u %s -p <password> (or point --dir at your keys)", g.me.Alias()),
			"this device has no group keys for %s", g.me.Alias())
	}
	return explain(err, "use the group "+g.label, g.cl.timeout)
}

// update loads the group's state, brings my chain up to date with the
// member list (sync), runs fn if not nil, and saves, all under the
// device's lock.
func (g *groupRun) update(ctx context.Context, fn func(*groupState) error) error {
	err := g.dev.update(false, func(st *deviceState) error {
		key := g.owner + "/" + g.id
		gs := st.Groups[key]
		if gs == nil {
			gs = &groupState{Label: g.label, Owner: g.owner, ID: g.id}
			st.Groups[key] = gs
		}
		if gs.SharedWith == nil {
			gs.SharedWith = map[string]string{}
		}
		if gs.Chains == nil {
			gs.Chains = map[string]*senderkey.Receiver{}
		}
		if gs.Seen == nil {
			gs.Seen = map[string]bool{}
		}
		g.prekeys = st.Prekeys
		if err := g.sync(ctx, gs); err != nil {
			return err
		}
		if fn != nil {
			return fn(gs)
		}
		return nil
	})
	if err != nil {
		return g.explainDevice(err)
	}
	return nil
}

// sync starts a new chain if I have none or someone it was shared with
// left, and shares it with every member who doesn't have it yet.
func (g *groupRun) sync(ctx context.Context, gs *groupState) error {
	pubs, err := g.memberList(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(pubs, g.me.Pub()) {
		return withHint(fmt.Errorf("%w of %s", errNotMember, g.label), "ask the owner to add you: gundb group add %s %s", g.id, g.me.Alias())
	}
	fresh := gs.Sender == nil
	for pub := range gs.SharedWith {
		fresh = fresh || !slices.Contains(pubs, pub)
	}
	if fresh {
		s, err := senderkey.NewSender()
		if err != nil {
			return err
		}
		gs.Sender, gs.SharedWith = s, map[string]string{}
		if err := g.mine().Get("chain").PutAck(ctx, s.Key.Chain); err != nil {
			return err
		}
	}
	var waiting []string
	for _, pub := range pubs {
		if pub == g.me.Pub() {
			continue
		}
		prekey, err := g.cl.db.User(pub).Get("prekey").Once[string](ctx)
		if errors.Is(err, gundb.ErrNotFound) {
			waiting = append(waiting, g.name(ctx, pub))
			continue
		}
		if err != nil {
			return err
		}
		if gs.SharedWith[pub] == prekey {
			continue
		}
		sealed, err := senderkey.Seal(gs.Sender.Share(), prekey)
		if err != nil {
			return err
		}
		if err := g.mine().Get("keys").Get(gs.Sender.Key.Chain).Get(pub).PutAck(ctx, sealed); err != nil {
			return err
		}
		gs.SharedWith[pub] = prekey
	}
	if len(waiting) > 0 {
		fmt.Fprintf(g.errw, "note:  %s can't read your messages until they run: gundb group init\n", strings.Join(waiting, ", "))
	}
	return nil
}

// pull decrypts the messages of the other members that this device has
// not handled yet, adds them to the log, and returns them.
func (g *groupRun) pull(ctx context.Context, gs *groupState) ([]logLine, error) {
	pubs, err := g.memberList(ctx)
	if err != nil {
		return nil, err
	}
	var fresh []logLine
	waiting := map[string]int{}
	for _, pub := range pubs {
		if pub == g.me.Pub() {
			continue
		}
		links, err := g.node(pub).Get("messages").Links(ctx)
		if errors.Is(err, gundb.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, id := range slices.Sorted(maps.Keys(links)) {
			seen := pub + "/" + id
			if gs.Seen[seen] {
				continue
			}
			msg, err := g.cl.db.Get(links[id]).Once[senderkey.Message](ctx)
			if err != nil {
				continue // not here yet
			}
			text, at, err := g.decrypt(ctx, gs, pub, msg)
			if errors.Is(err, errNoKeyYet) {
				waiting[g.name(ctx, pub)]++
				continue // the key may come later: try again next time
			}
			gs.Seen[seen] = true
			if err != nil {
				continue // from before I joined, or not for me
			}
			l := logLine{ID: id, From: pub, At: at, Text: text}
			gs.Log = append(gs.Log, l)
			fresh = append(fresh, l)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(waiting)) {
		fmt.Fprintf(g.errw, "note:  %d message(s) from %s wait for their key, which %s shares the next time they use the group\n", waiting[name], name, name)
	}
	return fresh, nil
}

var (
	errNoKeyYet  = errors.New("no key for this chain yet")
	errNotMember = errors.New("you are not a member")
)

func (g *groupRun) decrypt(ctx context.Context, gs *groupState, from string, msg senderkey.Message) (string, int64, error) {
	r := gs.Chains[msg.Chain]
	if r == nil {
		sealed, err := g.node(from).Get("keys").Get(msg.Chain).Get(g.me.Pub()).Once[senderkey.Sealed](ctx)
		if err != nil {
			return "", 0, errNoKeyYet
		}
		prekey := g.prekeys[sealed.PreKey]
		if prekey == nil {
			return "", 0, errors.New("sealed to a prekey this device doesn't have")
		}
		key, err := senderkey.Open(sealed, prekey)
		if err != nil {
			return "", 0, err
		}
		r = senderkey.NewReceiver(key)
		gs.Chains[msg.Chain] = r
	}
	raw, err := r.Decrypt(msg)
	if err != nil {
		return "", 0, err
	}
	var body struct {
		Text string `json:"text"`
		At   int64  `json:"at"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", 0, err
	}
	return body.Text, body.At, nil
}

func (g *groupRun) print(ctx context.Context, l logLine) {
	at := time.UnixMilli(l.At).Format("2006-01-02 15:04")
	fmt.Fprintf(g.out, "%s  %s: %s\n", at, g.name(ctx, l.From), l.Text)
}

// setMembers adds (in) or removes people, then brings my chain up to
// date: shared with the new ones, or a new chain without the removed.
func (g *groupRun) setMembers(ctx context.Context, in bool) error {
	verb := map[bool]string{true: "add", false: "remove"}[in]
	if !g.isOwner() {
		return fail("ask the owner", "only the owner of %s can %s members", g.label, verb)
	}
	change := map[string]any{}
	var names []string
	for _, who := range g.args {
		k, err := g.cl.lookUp(ctx, who)
		if err != nil {
			return err
		}
		switch {
		case k.pub == g.me.Pub():
			return fail("you own it", "you can't %s yourself", verb)
		case in:
			if _, err := g.cl.db.User(k.pub).Get("prekey").Once[string](ctx); err != nil {
				return fail(fmt.Sprintf("ask them to run once: gundb group init -u %s -p <password>", who),
					"%s has not set up group chats yet", who)
			}
			change[k.pub] = true
		default:
			current, err := g.memberList(ctx)
			if err != nil {
				return err
			}
			if !slices.Contains(current, k.pub) {
				return fail("see who is in it with: gundb group members "+g.id, "%s is not a member of %s", who, g.label)
			}
			change[k.pub] = nil
		}
		names = append(names, who)
	}
	if err := g.members().PutAck(ctx, change); err != nil {
		return explain(err, verb+" members", g.cl.timeout)
	}
	if err := g.update(ctx, nil); err != nil {
		return err
	}
	if in {
		fmt.Fprintf(g.out, "ok  added %s to %s; they read it as %s\n", strings.Join(names, ", "), g.id, g.label)
	} else {
		fmt.Fprintf(g.out, "ok  removed %s from %s; your new chain is not shared with them\n", strings.Join(names, ", "), g.id)
	}
	return nil
}

// sub prints the conversation, then watches every member's group node and
// the member list, and on each change syncs and pulls.
func (g *groupRun) sub(ctx context.Context) error {
	wake := make(chan struct{}, 1)
	poke := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	var mu sync.Mutex
	watching := map[string]func(){}
	defer func() {
		for _, off := range watching {
			off()
		}
	}()
	watch := func(pubs []string) {
		mu.Lock()
		defer mu.Unlock()
		for _, pub := range pubs {
			if watching[pub] == nil {
				watching[pub] = g.node(pub).On(func(json.RawMessage) { poke() })
			}
		}
	}
	offMembers := g.members().On(func(json.RawMessage) { poke() })
	defer offMembers()

	printed, first := 0, true
	step := func() error {
		run, cancel := operation(ctx, g.c)
		defer cancel()
		var lines []logLine
		err := g.update(run, func(gs *groupState) error {
			if _, err := g.pull(run, gs); err != nil {
				return err
			}
			lines = gs.Log[printed:]
			printed = len(gs.Log)
			return nil
		})
		if err != nil {
			return err
		}
		if pubs, err := g.memberList(run); err == nil {
			watch(pubs)
		}
		if first { // the conversation so far: oldest first
			first = false
			slices.SortStableFunc(lines, func(a, b logLine) int { return cmp.Compare(a.At, b.At) })
		}
		for _, l := range lines {
			g.print(run, l)
		}
		return nil
	}
	if err := step(); err != nil {
		return err
	}
	g.log.Info("watching "+g.label+" (Ctrl-C to stop)", "addr", g.cl.addr)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
			if err := step(); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if errors.Is(err, errNotMember) {
					return err
				}
				printError(g.errw, err)
			}
		}
	}
}
