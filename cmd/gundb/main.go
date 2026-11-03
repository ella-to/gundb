// Command gundb runs a GUN relay and talks to one from the command line.
//
//	gundb server                          # a relay on http://localhost:8765/gun
//	gundb put users/1/name 'Ali'          # write
//	gundb get users/1                     # read
//	gundb set users/1/tags go             # add to a list
//	gundb sub users                       # watch for changes, Ctrl-C to stop
//	gundb user create -u ali -p secret123 # a SEA user with its own key pair
//
// Run gundb --help, or gundb <command> --help, for everything else.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"ella.to/gundb/cmd/gundb/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
