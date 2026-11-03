package cli

import (
	"context"
	"fmt"
	"io"

	ucli "github.com/urfave/cli/v3"

	"ella.to/gundb"
)

func (a *App) userCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "user",
		Usage: "create a SEA user, or log in and show its keys",
		Description: `A user is a SEA key pair, stored on the server like gun.user() stores
it: the private keys are encrypted with the password, so the same user
can log in from GUN.js. Logged in (--username, --password), a command can
write to the user's own space, ~/..., and sign and encrypt values.

  gundb user create -u ali -p secret123
  gundb user login  -u ali -p secret123
  gundb put ~/profile/name Ali -u ali -p secret123

Set GUNDB_USERNAME and GUNDB_PASSWORD to stop repeating them.`,
		Commands: []*ucli.Command{
			{
				Name:         "create",
				Usage:        "create a user with a new key pair",
				Flags:        clientFlags(),
				OnUsageError: usageError,
				Action: func(ctx context.Context, c *ucli.Command) error {
					name, password, err := credentials(c)
					if err != nil {
						return err
					}
					return a.withServer(ctx, c, func(ctx context.Context, cl *client) error {
						u, err := cl.db.CreateUser(ctx, name, password)
						if err != nil {
							return explain(err, "create user "+name, cl.timeout)
						}
						fmt.Fprintf(c.Root().Writer, "created user %s\n", name)
						showUser(c.Root().Writer, u)
						fmt.Fprintf(c.Root().ErrWriter, "next:  gundb put ~/profile/name %s -u %s -p <password>\n", name, name)
						return nil
					})
				},
			},
			{
				Name:  "login",
				Usage: "check a username and password and show the user's keys",
				Flags: clientFlags(&ucli.BoolFlag{
					Name:  "keys",
					Usage: "print the whole key pair as JSON, private keys included",
				}),
				OnUsageError: usageError,
				Action: func(ctx context.Context, c *ucli.Command) error {
					name, password, err := credentials(c)
					if err != nil {
						return err
					}
					return a.withServer(ctx, c, func(ctx context.Context, cl *client) error {
						u, err := cl.db.Login(ctx, name, password)
						if err != nil {
							return explain(err, "log in as "+name, cl.timeout)
						}
						if c.Bool("keys") {
							fmt.Fprintln(c.Root().ErrWriter, "note:  priv and epriv are secret: anyone with them can sign and decrypt as "+name)
							fmt.Fprintln(c.Root().Writer, encode(u.Pair(), true))
							return nil
						}
						fmt.Fprintf(c.Root().Writer, "logged in as %s\n", name)
						showUser(c.Root().Writer, u)
						return nil
					})
				},
			},
		},
	}
}

// credentials returns --username and --password, which user commands need.
func credentials(c *ucli.Command) (name, password string, err error) {
	name, password = c.String("username"), c.String("password")
	if name == "" || password == "" {
		return "", "", fail("usage: "+c.FullName()+" --username <name> --password <password>",
			"%s needs --username and --password", c.FullName())
	}
	return name, password, nil
}

// withServer connects without logging in (the command does that itself)
// and runs fn within --timeout.
func (a *App) withServer(ctx context.Context, c *ucli.Command, fn func(ctx context.Context, cl *client) error) error {
	ctx, cancel := operation(ctx, c)
	defer cancel()
	log, _ := a.logger(c)
	cl, err := a.connect(ctx, c, log, false)
	if err != nil {
		return err
	}
	defer cl.close()
	return fn(ctx, cl)
}

func showUser(w io.Writer, u *gundb.User) {
	p := u.Pair()
	fmt.Fprintf(w, "  pub   %s\n  epub  %s\n", p.Pub, p.EPub)
}
