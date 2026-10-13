// Command pgglass is an MCP server that gives AI agents read-only access to
// the PostgreSQL connections of its config file, over stdio or Streamable
// HTTP. Its doctor command checks that config and each connection's role.
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
  pgglass [--config FILE] [--http ADDR]  serve the tools over stdio, or over Streamable HTTP
  pgglass [--config FILE] doctor         check the config and every connection, then exit
  pgglass --version                      print the version

Flags:
  --config FILE  config file (default ~/.config/pgglass/config.json)
  --http ADDR    serve http://ADDR/mcp, such as :8411 for 127.0.0.1:8411; a host other
                 than loopback needs PGGLASS_HTTP_TOKEN, a token every request must carry
  --version      print the version
`

type options struct {
	config  string
	http    string
	version bool
	doctor  bool
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
	case opts.doctor:
		return doctor(ctx, opts.config, stdout, stderr)
	default:
		return serve(ctx, opts, stdin, stdout, stderr)
	}
}

func parseArgs(args []string) (options, error) {
	opts := options{config: defaultConfigPath()}
	fs := flag.NewFlagSet("pgglass", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.config, "config", opts.config, "")
	fs.StringVar(&opts.http, "http", "", "")
	fs.BoolVar(&opts.version, "version", false, "")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.Arg(0) == "doctor" {
		opts.doctor = true
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return opts, err
		}
	}
	switch {
	case fs.NArg() > 0:
		return opts, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case opts.doctor && opts.http != "":
		return opts, errors.New("doctor serves nothing: drop --http")
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
