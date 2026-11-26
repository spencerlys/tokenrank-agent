// Command tokenrank-agent runs TokenRank checks against your own AI API
// endpoint, using your own key, from your own machine.
//
// The agent fetches signed request batches from the TokenRank cloud, sends
// them straight to your endpoint and uploads the raw responses. Your key
// stays in your OS keychain and is only ever sent to your endpoint.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/spencerlys/tokenrank-agent/internal/agent"
	"github.com/spencerlys/tokenrank-agent/internal/audit"
	"github.com/spencerlys/tokenrank-agent/internal/config"
	"github.com/spencerlys/tokenrank-agent/internal/keychain"
	"github.com/spencerlys/tokenrank-agent/internal/sender"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// EnvAPIURL overrides the TokenRank API URL.
const EnvAPIURL = "TOKENRANK_API_URL"

const usage = `tokenrank-agent: check your own AI API endpoint with your own key.

Usage:
  tokenrank-agent register --base-url URL --model NAME [--api URL] [--key-stdin] [--config PATH]
  tokenrank-agent run [--once] [--api URL] [--config PATH] [--jitter-min D] [--jitter-max D] [--timeout D]
  tokenrank-agent audit --listen 127.0.0.1:8787
  tokenrank-agent version

The endpoint key is read from --key-stdin, from TOKENRANK_ENDPOINT_KEY, or
from a hidden prompt. It is stored in your OS keychain and is only ever sent
to your own endpoint.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func cli(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "register":
		return cmdRegister(ctx, args[1:], stdin, stdout, stderr)
	case "run":
		return cmdRun(ctx, args[1:], stdout, stderr)
	case "audit":
		return cmdAudit(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "tokenrank-agent %s (%s, %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func resolveConfigPath(p string) (string, error) {
	if p != "" {
		return p, nil
	}
	return config.DefaultPath()
}

func baseEnv(stderr io.Writer) agent.Env {
	return agent.Env{
		Store:   keychain.NewOS(),
		Getenv:  os.Getenv,
		Log:     stderr,
		Version: version,
	}
}

func cmdRegister(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("register", stderr)
	apiURL := fs.String("api", "", "TokenRank API URL (default $"+EnvAPIURL+" or "+config.DefaultAPIURL+")")
	baseURL := fs.String("base-url", "", "your endpoint's base URL, e.g. https://llm.example.com/v1")
	model := fs.String("model", "", "model name as your endpoint expects it")
	keyStdin := fs.Bool("key-stdin", false, "read the endpoint key from the first line of stdin")
	cfgPath := fs.String("config", "", "config file path (default: user config dir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *baseURL == "" || *model == "" {
		fmt.Fprintln(stderr, "register: --base-url and --model are required")
		return 2
	}
	path, err := resolveConfigPath(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "register:", err)
		return 1
	}
	key, err := readEndpointKey(*keyStdin, stdin, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "register:", err)
		return 1
	}
	cfg, err := agent.Register(ctx, baseEnv(stderr), agent.RegisterOptions{
		ConfigPath:  path,
		APIURL:      pickAPIURL(*apiURL),
		BaseURL:     *baseURL,
		Model:       *model,
		EndpointKey: key,
	})
	if err != nil {
		fmt.Fprintln(stderr, "register:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Registered install %s (protocol %s, every %d minutes).\nConfig: %s\nSecrets: OS keychain, service %q.\nNext: tokenrank-agent run\n",
		cfg.InstallID, cfg.Protocol, cfg.ScheduleMinutes, path, keychain.Service)
	return 0
}

func pickAPIURL(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := strings.TrimSpace(os.Getenv(EnvAPIURL)); v != "" {
		return v
	}
	return config.DefaultAPIURL
}

// readEndpointKey never accepts the key as a flag, so it cannot land in shell
// history or process listings.
func readEndpointKey(fromStdin bool, stdin io.Reader, stderr io.Writer) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(io.LimitReader(stdin, 8192)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read key from stdin: %w", err)
		}
		return strings.TrimSpace(line), nil
	}
	if v := strings.TrimSpace(os.Getenv(keychain.EnvEndpointKey)); v != "" {
		return v, nil
	}
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(stderr, "Endpoint API key (input hidden): ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", fmt.Errorf("read key: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", fmt.Errorf("no endpoint key: use --key-stdin, set %s, or run in a terminal", keychain.EnvEndpointKey)
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("run", stderr)
	once := fs.Bool("once", false, "run a single cycle and exit")
	apiURL := fs.String("api", "", "override the configured TokenRank API URL (or set $"+EnvAPIURL+")")
	cfgPath := fs.String("config", "", "config file path (default: user config dir)")
	jMin := fs.Duration("jitter-min", sender.DefaultJitterMin, "minimum random pause between requests")
	jMax := fs.Duration("jitter-max", sender.DefaultJitterMax, "maximum random pause between requests")
	timeout := fs.Duration("timeout", sender.DefaultTimeout, "timeout for each request to your endpoint")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *jMin < 0 || *jMax < *jMin {
		fmt.Fprintln(stderr, "run: need 0 <= --jitter-min <= --jitter-max")
		return 2
	}
	path, err := resolveConfigPath(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "run:", err)
		return 1
	}
	override := *apiURL
	if override == "" {
		override = strings.TrimSpace(os.Getenv(EnvAPIURL))
	}
	opts := agent.RunOptions{ConfigPath: path, APIURL: override, JitterMin: *jMin, JitterMax: *jMax, Timeout: *timeout}
	env := baseEnv(stderr)

	if *once {
		sum, err := agent.RunOnce(ctx, env, opts)
		if err != nil {
			fmt.Fprintln(stderr, "run:", err)
			return 1
		}
		if sum.NothingDue {
			fmt.Fprintln(stdout, "Nothing due.")
			return 0
		}
		fmt.Fprintf(stdout, "Batch %s: %d requests answered, %d with errors. Results uploaded.\n", sum.BatchID, sum.Sent, sum.Failed)
		return 0
	}
	fmt.Fprintf(stdout, "tokenrank-agent %s running; press Ctrl+C to stop.\n", version)
	if err := agent.Run(ctx, env, opts); err != nil {
		fmt.Fprintln(stderr, "run:", err)
		return 1
	}
	return 0
}

func cmdAudit(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("audit", stderr)
	listen := fs.String("listen", "127.0.0.1:8787", "loopback address for the local audit sidecar")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := audit.Serve(*listen, stdout); err != nil {
		fmt.Fprintln(stderr, "audit:", err)
		return 2
	}
	return 0
}
