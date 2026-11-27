// Package server is the reasonrd server for a distribution: a binary links
// the engine kinds and adapters it needs (their init registers them) and
// calls Main, as reasonrd does:
//
//	import (
//		"github.com/tycheg/reasonr-engine/server"
//		_ "github.com/tycheg/reasonr-stateless/src"
//		_ "github.com/tycheg/reasonr-postgres/src"
//	)
//
//	func main() { os.Exit(server.Main(os.Args[1:])) }
//
// The commands themselves are internal (internal/server).
package server

import (
	"context"
	"io"

	"github.com/tycheg/reasonr-engine/internal/server"
)

// Main runs the subcommand from args, stopping on SIGINT and SIGTERM; it
// returns the exit code.
func Main(args []string) int { return server.Main(args) }

// Run runs the subcommand from args until ctx is cancelled: its output goes
// to stdout, messages and the log to stderr; it returns the exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return server.Run(ctx, args, stdout, stderr)
}
