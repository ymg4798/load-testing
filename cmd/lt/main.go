// Command lt is a load testing tool. See `lt help`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

var version = "dev"

const usage = `lt — load testing tool

Usage:
  lt run [flags] <scenario.yaml>        run a test
  lt validate <scenario.yaml>           check a scenario file without running it
  lt report <result.json> [-o out.html] turn a result file into an HTML report
  lt compare <base.json> <new.json>     compare two results
  lt history [test]                     stored runs and trends (lt run --save)
  lt convert <file.har|curl.txt|openapi.yaml>  turn recordings or API specs into a scenario
  lt record [--target URL | --listen :8888]   record traffic through a proxy into a scenario
  lt monitor [flags] <scenario.yaml>    run a scenario's flow on a schedule and alert on failure
  lt watch [flags] <watch.yaml>         always-on console: servers, databases, APM traces, alerts
  lt agent [--listen :7700]             serve as a remote load generator
  lt preflight                          check OS limits for high load
  lt version

Run "lt <command> -h" for a command's flags.
`

// Exit codes.
const (
	exitOK         = 0
	exitThresholds = 1 // test ran, thresholds failed (or compare found a regression)
	exitUsage      = 2 // bad flags or scenario file
	exitError      = 3 // test could not run
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitUsage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The first Ctrl+C / SIGTERM asks for a graceful stop; after that the
	// default handling is back, so a second one ends the process at once.
	go func() { <-ctx.Done(); stop() }()
	cmd, args := os.Args[1], os.Args[2:]
	var code int
	switch cmd {
	case "run":
		code = cmdRun(ctx, args)
	case "validate":
		code = cmdValidate(args)
	case "report":
		code = cmdReport(args)
	case "compare":
		code = cmdCompare(args)
	case "history":
		code = cmdHistory(args)
	case "record":
		code = cmdRecord(ctx, args)
	case "convert":
		code = cmdConvert(args)
	case "watch":
		code = cmdWatch(ctx, args)
	case "monitor":
		code = cmdMonitor(ctx, args)
	case "agent":
		code = cmdAgent(ctx, args)
	case "preflight":
		code = cmdPreflight(args)
	case "version", "--version", "-v":
		fmt.Println("lt", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "lt: unknown command %q\n\n%s", cmd, usage)
		code = exitUsage
	}
	os.Exit(code)
}

// parse parses flags that may appear before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// multiFlag collects a repeatable flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func fail(code int, format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "lt: "+format+"\n", a...)
	return code
}

func isHelp(err error) bool { return errors.Is(err, flag.ErrHelp) }
