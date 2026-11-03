// Package cli is the gundb command: a GUN relay server and a client for
// reading, writing and watching any GUN peer.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	ucli "github.com/urfave/cli/v3"
)

// DefaultAddr is where the server listens, and where the other commands
// connect, when --addr is not given.
const DefaultAddr = "http://localhost:8765/gun"

// defaultTimeout bounds get, put, set and user, and how long sub waits to
// connect.
const defaultTimeout = 2 * time.Second

// App is one run of the command, with its own input and output.
type App struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer

	// onListen, if set, is called with the server's address once it
	// listens. Tests use it to find a server started on port 0.
	onListen func(addr string)
}

// Main runs the command with args (os.Args) and returns the exit code.
// Errors are printed to stderr with a hint where one helps.
func Main(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &App{Stdin: stdin, Stdout: stdout, Stderr: stderr}
	if err := a.Command().Run(ctx, args); err != nil {
		printError(stderr, err)
		return 1
	}
	return 0
}

// Flags shared by several commands. Each command gets its own: a flag
// holds the value it parsed.

func addrFlag() ucli.Flag {
	return &ucli.StringFlag{
		Name:    "addr",
		Aliases: []string{"a"},
		Usage:   "`URL` of the gundb server (http, https, ws or wss)",
		Value:   DefaultAddr,
		Sources: ucli.EnvVars("GUNDB_ADDR"),
	}
}
func headerFlag() ucli.Flag {
	return &ucli.StringSliceFlag{
		Name:    "header",
		Aliases: []string{"H"},
		Usage:   "HTTP header for the connection, as `Name=Value`; repeat it for more",
	}
}
func timeoutFlag() ucli.Flag {
	return &ucli.DurationFlag{
		Name:    "timeout",
		Aliases: []string{"t"},
		Usage:   "how long to wait for the server, like 500ms or 10s; 0 waits forever",
		Value:   defaultTimeout,
		Sources: ucli.EnvVars("GUNDB_TIMEOUT"),
	}
}
func usernameFlag() ucli.Flag {
	return &ucli.StringFlag{
		Name:    "username",
		Aliases: []string{"u"},
		Usage:   "log in as this SEA user: ~/ then means your own space",
		Sources: ucli.EnvVars("GUNDB_USERNAME"),
	}
}
func passwordFlag() ucli.Flag {
	return &ucli.StringFlag{
		Name:    "password",
		Aliases: []string{"p"},
		Usage:   "password of --username",
		Sources: ucli.EnvVars("GUNDB_PASSWORD"),
	}
}
func verboseFlag() ucli.Flag {
	return &ucli.BoolFlag{
		Name:  "verbose",
		Usage: "log what happens on the wire, for debugging",
	}
}

// clientFlags are the flags of every command that connects to a server.
func clientFlags(more ...ucli.Flag) []ucli.Flag {
	return append([]ucli.Flag{addrFlag(), headerFlag(), timeoutFlag(), usernameFlag(), passwordFlag()}, more...)
}

// Command returns the root command.
func (a *App) Command() *ucli.Command {
	root := a.commands()
	// Commas belong in header values; --peers splits its own. urfave reads
	// this per command, so every command needs it.
	var noSplit func(c *ucli.Command)
	noSplit = func(c *ucli.Command) {
		c.DisableSliceFlagSeparator = true
		for _, sub := range c.Commands {
			noSplit(sub)
		}
	}
	noSplit(root)
	return root
}

func (a *App) commands() *ucli.Command {
	return &ucli.Command{
		Name:      "gundb",
		Usage:     "run a GUN relay, and read, write and watch its data",
		Reader:    a.Stdin,
		Writer:    a.Stdout,
		ErrWriter: a.Stderr,
		Flags:     []ucli.Flag{verboseFlag()},
		Description: `Paths name a place in the graph, one key per segment:
users/1/name is gun.get('users').get('1').get('name').

With --username and --password, ~/ is your own space (signed writes),
and ~@alias/... or ~<pub>/... is another user's.

Values are JSON when they parse as JSON (42, true, {"a":1}, "quoted"),
text otherwise; --string always stores text, and - reads stdin.`,
		HideVersion:           true,
		EnableShellCompletion: true,
		OnUsageError:          usageError,
		Commands: []*ucli.Command{
			a.serverCommand(),
			a.getCommand(),
			a.putCommand(),
			a.setCommand(),
			a.subCommand(),
			a.userCommand(),
		},
	}
}

// usageError keeps flag mistakes short: the mistake and where to look,
// rather than the whole help.
func usageError(_ context.Context, c *ucli.Command, err error, _ bool) error {
	return withHint(err, "see: %s --help", c.FullName())
}

// args checks that c got exactly the arguments named in want.
func args(c *ucli.Command, want ...string) ([]string, error) {
	got := c.Args().Slice()
	if len(got) == len(want) {
		return got, nil
	}
	usage := c.FullName() + " <" + strings.Join(want, "> <") + ">"
	err := fmt.Errorf("%s needs %d argument(s), got %d", c.FullName(), len(want), len(got))
	if len(got) > len(want) {
		err = fmt.Errorf("%s: too many arguments %q; quote a value with spaces: 'hello world'", c.FullName(), got[len(want):])
	}
	return nil, withHint(err, "usage: %s", usage)
}

// logger returns the logger for messages to the person running gundb, and
// the one for the gundb library, which is quiet unless --verbose.
func (a *App) logger(c *ucli.Command) (app, lib *slog.Logger) {
	level := slog.LevelInfo
	libLevel := slog.LevelWarn
	if c.Bool("verbose") {
		level, libLevel = slog.LevelDebug, slog.LevelDebug
	}
	return newLogger(a.Stderr, level), newLogger(a.Stderr, libLevel)
}

// operation returns ctx bounded by --timeout (none for 0).
func operation(ctx context.Context, c *ucli.Command) (context.Context, context.CancelFunc) {
	if t := c.Duration("timeout"); t > 0 {
		return context.WithTimeout(ctx, t)
	}
	return context.WithCancel(ctx)
}
