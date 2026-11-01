package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ymg4798/load-testing/internal/convert"
	"github.com/ymg4798/load-testing/internal/engine"
	"github.com/ymg4798/load-testing/internal/record"
	"github.com/ymg4798/load-testing/internal/scenario"
	"github.com/ymg4798/load-testing/internal/yamlx"
)

const recordUsage = `Usage: lt record [flags]

Record HTTP traffic through a proxy and turn it into a scenario. Stop with
Ctrl+C (or --duration); the scenario is written to --out.

  Forward proxy (default): set http://localhost:8888 as the HTTP and HTTPS
  proxy of your browser or system (or HTTP_PROXY / HTTPS_PROXY, curl -x).
  Plain HTTP is recorded. HTTPS passes through unrecorded unless --mitm,
  which decrypts it with certificates from a local CA you trust once.

  Reverse proxy: --target http://localhost:8080 forwards every request to
  the target; point your client at the proxy instead of the server.

Examples:
  lt record --target http://localhost:8080 --out flow.yaml
  lt record --mitm --host app.example.com --out flow.yaml

Flags:
`

func cmdRecord(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), recordUsage)
		fs.PrintDefaults()
	}
	listen := fs.String("listen", ":8888", "proxy listen address")
	target := fs.String("target", "", "reverse-proxy mode: forward every request to this URL")
	mitm := fs.Bool("mitm", false, "intercept HTTPS with a local CA (see --ca-dir)")
	caDir := fs.String("ca-dir", "~/.lt/ca", "where the --mitm CA is kept ("+record.CACertFile+" is the one to trust)")
	hosts := fs.String("host", "", "record only these hosts (and their subdomains), comma-separated")
	out := fs.String("out", "recorded.yaml", "scenario file to write (- for stdout)")
	duration := fs.String("duration", "", "stop recording after this long (default: until Ctrl+C)")
	name := fs.String("name", "", "scenario name (default: from --out)")
	rate := fs.Float64("rate", 1, "iterations per second in the generated scenario")
	testDuration := fs.String("test-duration", "30s", "duration of the generated scenario")
	all := fs.Bool("all", false, "keep static assets (images, scripts, styles, fonts)")
	noSleep := fs.Bool("no-sleep", false, "don't turn pauses into sleep steps")
	baseURL := fs.String("base-url", "", "value for vars.baseUrl (default: the most used origin)")
	insecure := fs.Bool("insecure", false, "don't verify upstream TLS certificates")
	quiet := fs.Bool("q", false, "don't list requests as they are recorded")
	pos, err := parse(fs, args)
	if err != nil {
		if isHelp(err) {
			return exitOK
		}
		return exitUsage
	}
	if len(pos) != 0 {
		return fail(exitUsage, "lt record takes no arguments (got %q); see lt record -h", strings.Join(pos, " "))
	}
	var stopAfter yamlx.Duration
	if *duration != "" {
		d, err := yamlx.ParseDuration(*duration)
		if err != nil || d <= 0 {
			return fail(exitUsage, "bad --duration %q", *duration)
		}
		stopAfter = yamlx.Duration(d)
	}
	if d, err := yamlx.ParseDuration(*testDuration); err != nil || d <= 0 {
		return fail(exitUsage, "bad --test-duration %q", *testDuration)
	}
	if *rate <= 0 {
		return fail(exitUsage, "--rate must be > 0")
	}
	if *mitm && *target != "" {
		return fail(exitUsage, "--mitm is for the forward proxy; with --target use an https:// target instead")
	}
	var (
		caPath    string
		caCreated bool
	)
	opt := record.Options{Target: *target, MITM: *mitm, Insecure: *insecure, Log: os.Stderr}
	if *quiet {
		opt.Log = nil
	}
	for _, h := range strings.Split(*hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			opt.Hosts = append(opt.Hosts, h)
		}
	}
	if *mitm {
		dir := expandHome(*caDir)
		ca, created, err := record.LoadOrCreateCA(dir)
		if err != nil {
			return fail(exitError, "%v", err)
		}
		opt.CA = ca
		caPath, caCreated = ca.CertPath, created
	}
	rec, err := record.New(opt)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fail(exitError, "%v", err)
	}
	addr := proxyAddr(ln.Addr())
	if caPath != "" {
		printCAHelp(caPath, caCreated, addr)
	}
	if *target != "" {
		fmt.Fprintf(os.Stderr, "lt record: reverse proxy on http://%s -> %s\n", addr, *target)
		fmt.Fprintf(os.Stderr, "  send your requests to http://%s instead of %s\n", addr, *target)
	} else {
		fmt.Fprintf(os.Stderr, "lt record: proxy on http://%s\n", addr)
		fmt.Fprintf(os.Stderr, "  set it as the HTTP and HTTPS proxy, e.g.\n")
		fmt.Fprintf(os.Stderr, "    export HTTP_PROXY=http://%s HTTPS_PROXY=http://%s\n", addr, addr)
		fmt.Fprintf(os.Stderr, "    curl -x http://%s http://example.com/\n", addr)
		if !*mitm {
			fmt.Fprintf(os.Stderr, "  HTTPS passes through unrecorded; add --mitm to record it\n")
		}
	}
	if len(opt.Hosts) > 0 {
		fmt.Fprintf(os.Stderr, "  recording only %s\n", strings.Join(opt.Hosts, ", "))
	}
	if stopAfter > 0 {
		fmt.Fprintf(os.Stderr, "  recording for %s (Ctrl+C to stop early)\n\n", stopAfter.D())
	} else {
		fmt.Fprintf(os.Stderr, "  press Ctrl+C to stop and write %s\n\n", *out)
	}
	if err := rec.Serve(ctx, ln, stopAfter.D()); err != nil {
		return fail(exitError, "%v", err)
	}

	entries := rec.Entries()
	fmt.Fprintf(os.Stderr, "\nlt record: %d request(s) recorded", len(entries))
	if n := rec.Tunnels(); n > 0 {
		fmt.Fprintf(os.Stderr, ", %d HTTPS tunnel(s) passed through unrecorded", n)
	}
	fmt.Fprintln(os.Stderr)
	if len(entries) == 0 {
		return fail(exitError, "nothing recorded, no scenario written")
	}
	sname := *name
	if sname == "" {
		sname = "recorded"
		if *out != "-" {
			b := filepath.Base(*out)
			sname = strings.TrimSuffix(b, filepath.Ext(b))
		}
	}
	y, err := rec.Scenario(convert.Options{
		Name: sname, Rate: *rate, Duration: *testDuration, All: *all, NoSleep: *noSleep, BaseURL: *baseURL,
	})
	if err != nil {
		return fail(exitError, "%v", err)
	}
	dir := "."
	if *out != "-" {
		dir = filepath.Dir(*out)
	}
	f, err := scenario.Parse(y, dir, "recorded.yaml", nil)
	if err == nil {
		err = engine.Validate(f)
	}
	if err != nil {
		os.Stderr.Write(y)
		return fail(exitError, "generated scenario does not validate: %v", err)
	}
	if *out == "-" {
		_, err = os.Stdout.Write(y)
	} else if err = os.WriteFile(*out, y, 0o644); err == nil {
		fmt.Fprintf(os.Stderr, "wrote %s (%d step(s)); run it with: lt run %s\n", *out, len(f.Scenarios["main"].Flow), *out)
	}
	if err != nil {
		return fail(exitError, "%v", err)
	}
	return exitOK
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// proxyAddr shows a listen address as something a client can dial.
func proxyAddr(a net.Addr) string {
	host, port, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

func printCAHelp(certPath string, created bool, addr string) {
	if created {
		fmt.Fprintf(os.Stderr, "lt record: created a local CA: %s\n", certPath)
	} else {
		fmt.Fprintf(os.Stderr, "lt record: using the local CA %s\n", certPath)
	}
	fmt.Fprintf(os.Stderr, "  HTTPS is decrypted with certificates signed by this CA. Clients must trust it once:\n")
	switch runtime.GOOS {
	case "darwin":
		fmt.Fprintf(os.Stderr, "    macOS:   sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain %s\n", certPath)
	case "linux":
		fmt.Fprintf(os.Stderr, "    Linux:   sudo cp %s /usr/local/share/ca-certificates/lt-ca.crt && sudo update-ca-certificates\n", certPath)
	case "windows":
		fmt.Fprintf(os.Stderr, "    Windows: certutil -addstore -user Root %s\n", certPath)
	}
	fmt.Fprintf(os.Stderr, "    Firefox: Settings > Privacy & Security > Certificates > View Certificates > Authorities > Import\n")
	fmt.Fprintf(os.Stderr, "    curl:    curl --cacert %s -x http://%s https://...\n", certPath, addr)
	fmt.Fprintf(os.Stderr, "    Node:    NODE_EXTRA_CA_CERTS=%s\n", certPath)
	fmt.Fprintf(os.Stderr, "  Anyone with %s can impersonate any site to clients that trust it: keep it private,\n", filepath.Join(filepath.Dir(certPath), record.CAKeyFile))
	fmt.Fprintf(os.Stderr, "  and remove the CA from your trust store when you are done recording.\n\n")
}
