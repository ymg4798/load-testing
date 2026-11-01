package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ymg4798/lt/internal/convert"
	"github.com/ymg4798/lt/internal/engine"
	"github.com/ymg4798/lt/internal/scenario"
	"github.com/ymg4798/lt/internal/yamlx"
)

const convertUsage = `Usage: lt convert [flags] <input|->

Turn a HAR recording, curl command(s) or an OpenAPI spec into a scenario.
The format is detected from the file; use --from to force it.

Flags:
`

func cmdConvert(args []string) int {
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), convertUsage)
		fs.PrintDefaults()
	}
	from := fs.String("from", "", "input format: har, curl or openapi (default: detect)")
	out := fs.String("o", "", "output file (default: stdout)")
	name := fs.String("name", "", "scenario name (default: input file name)")
	rate := fs.Float64("rate", 1, "iterations per second")
	duration := fs.String("duration", "30s", "test duration")
	all := fs.Bool("all", false, "keep static assets (HAR) and deprecated operations (OpenAPI)")
	hosts := fs.String("host", "", "keep only these hosts, comma-separated (HAR, curl)")
	noSleep := fs.Bool("no-sleep", false, "don't turn pauses in a HAR into sleep steps")
	baseURL := fs.String("base-url", "", "value for vars.baseUrl (default: from the input)")
	pos, err := parse(fs, args)
	if err != nil {
		if isHelp(err) {
			return exitOK
		}
		return exitUsage
	}
	if len(pos) != 1 {
		return fail(exitUsage, "usage: lt convert [flags] <input|->")
	}
	if *rate <= 0 {
		return fail(exitUsage, "--rate must be > 0")
	}
	if d, err := yamlx.ParseDuration(*duration); err != nil || d <= 0 {
		return fail(exitUsage, "bad --duration %q", *duration)
	}
	var data []byte
	if pos[0] == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(pos[0])
	}
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	opt := convert.Options{
		Name: *name, Rate: *rate, Duration: *duration, All: *all,
		NoSleep: *noSleep, BaseURL: *baseURL,
	}
	for _, h := range strings.Split(*hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			opt.Hosts = append(opt.Hosts, h)
		}
	}
	y, err := convert.Convert(strings.ToLower(*from), pos[0], data, opt)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}

	// The output must load and compile like any hand-written scenario.
	dir := "."
	if *out != "" {
		dir = filepath.Dir(*out)
	}
	f, err := scenario.Parse(y, dir, "converted.yaml", nil)
	if err == nil {
		err = engine.Validate(f)
	}
	if err != nil {
		os.Stderr.Write(y)
		return fail(exitError, "generated scenario does not validate: %v", err)
	}

	if *out == "" {
		_, err = os.Stdout.Write(y)
	} else {
		err = os.WriteFile(*out, y, 0o644)
		if err == nil {
			fmt.Fprintf(os.Stderr, "wrote %s (%d step(s))\n", *out, len(f.Scenarios["main"].Flow))
		}
	}
	if err != nil {
		return fail(exitError, "%v", err)
	}
	return exitOK
}
