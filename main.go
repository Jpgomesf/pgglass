// Command pgglass is an MCP server that gives AI agents read-only access to
// the PostgreSQL connections of its config file, over stdio.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
)

var version string

const usage = `Usage:
  pgglass [--config FILE]  serve the tools over stdio
  pgglass --version        print the version

Flags:
  --config FILE  config file (default ~/.config/pgglass/config.json)
  --version      print the version
`

type options struct {
	config  string
	version bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(stdout, usage)
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "pgglass: %v\n\n%s", err, usage)
		return 2
	case opts.version:
		fmt.Fprintln(stdout, "pgglass", buildVersion())
		return 0
	default:
		return serve(ctx, opts, stdin, stdout, stderr)
	}
}

func parseArgs(args []string) (options, error) {
	opts := options{config: defaultConfigPath()}
	fs := flag.NewFlagSet("pgglass", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.config, "config", opts.config, "")
	fs.BoolVar(&opts.version, "version", false, "")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return opts, nil
}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "pgglass", "config.json")
}

func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
