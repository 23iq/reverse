package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

const Help = `reverse - expose a local HTTP service through your own VPS

USAGE
  reverse -p <port> [--host <address>] [--background]
  reverse --status
  reverse --stop
  reverse --config
  reverse --setup
  reverse --help

COMMANDS
  -p, --port <port>   Forward a local HTTP port through the configured server
  -s, --status        Show the background tunnel status
  -x, --stop          Stop the background tunnel
  --config, -cf       Create or replace the local client configuration
  --setup             Install and configure the server on this VPS

OPTIONS
  --host <address>    Local target address (default: value from config)
  -d, --background    Keep the tunnel running after this terminal closes
  --dry-run           Show setup actions without changing the VPS
  -h, --help          Show this help
  -v, --version       Print the version

EXAMPLES
  reverse --config
  reverse -p 3000
  reverse -p 3000 -d
  reverse --status
  reverse --stop
  reverse --host 127.0.0.1 -p 8080

The client configuration is stored with user-only permissions. Press q or
Ctrl+C in the tunnel dashboard to stop forwarding.
`

type Action int

const (
	ActionHelp Action = iota
	ActionVersion
	ActionConfigure
	ActionSetup
	ActionTunnel
	ActionStatus
	ActionStop
	ActionServer
	ActionDaemonWorker
)

type Options struct {
	Action       Action
	Port         int
	Host         string
	Background   bool
	DryRun       bool
	ServerConfig string
	SetupRoot    string
}

func Parse(args []string) (Options, error) {
	var opts Options
	set := flag.NewFlagSet("reverse", flag.ContinueOnError)
	set.SetOutput(io.Discard)

	var (
		portLong      int
		portShort     int
		configureLong bool
		configureCF   bool
		setup         bool
		statusLong    bool
		statusShort   bool
		stopLong      bool
		stopShort     bool
		server        bool
		daemonWorker  bool
		background    bool
		backgroundD   bool
		helpLong      bool
		helpShort     bool
		versionLong   bool
		versionShort  bool
	)
	set.IntVar(&portLong, "port", 0, "")
	set.IntVar(&portShort, "p", 0, "")
	set.StringVar(&opts.Host, "host", "", "")
	set.BoolVar(&configureLong, "config", false, "")
	set.BoolVar(&configureCF, "cf", false, "")
	set.BoolVar(&setup, "setup", false, "")
	set.BoolVar(&statusLong, "status", false, "")
	set.BoolVar(&statusShort, "s", false, "")
	set.BoolVar(&stopLong, "stop", false, "")
	set.BoolVar(&stopShort, "x", false, "")
	set.BoolVar(&background, "background", false, "")
	set.BoolVar(&backgroundD, "d", false, "")
	set.BoolVar(&opts.DryRun, "dry-run", false, "")
	set.BoolVar(&server, "server", false, "")
	set.BoolVar(&daemonWorker, "daemon-worker", false, "")
	set.StringVar(&opts.ServerConfig, "server-config", "", "")
	set.StringVar(&opts.SetupRoot, "setup-root", "", "")
	set.BoolVar(&helpLong, "help", false, "")
	set.BoolVar(&helpShort, "h", false, "")
	set.BoolVar(&versionLong, "version", false, "")
	set.BoolVar(&versionShort, "v", false, "")

	if err := set.Parse(args); err != nil {
		return Options{}, cleanFlagError(err)
	}
	if set.NArg() != 0 {
		if set.NArg() != 1 {
			return Options{}, fmt.Errorf("unexpected argument %q", set.Arg(0))
		}
		switch set.Arg(0) {
		case "help":
			helpLong = true
		case "status":
			statusLong = true
		case "stop":
			stopLong = true
		default:
			return Options{}, fmt.Errorf("unexpected argument %q", set.Arg(0))
		}
	}
	var portLongSet, portShortSet bool
	set.Visit(func(value *flag.Flag) {
		switch value.Name {
		case "port":
			portLongSet = true
		case "p":
			portShortSet = true
		}
	})
	portSpecified := portLongSet || portShortSet
	if portLongSet && portShortSet && portLong != portShort {
		return Options{}, errors.New("-p and --port specify different ports")
	}
	if portLongSet {
		opts.Port = portLong
	} else {
		opts.Port = portShort
	}

	actions := 0
	setAction := func(active bool, action Action) {
		if active {
			actions++
			opts.Action = action
		}
	}
	setAction(helpLong || helpShort || len(args) == 0, ActionHelp)
	setAction(versionLong || versionShort, ActionVersion)
	setAction(configureLong || configureCF, ActionConfigure)
	setAction(setup, ActionSetup)
	setAction(statusLong || statusShort, ActionStatus)
	setAction(stopLong || stopShort, ActionStop)
	if daemonWorker {
		setAction(true, ActionDaemonWorker)
	} else {
		setAction(portSpecified, ActionTunnel)
	}
	setAction(server, ActionServer)
	opts.Background = background || backgroundD

	if actions == 0 {
		opts.Action = ActionHelp
	}
	if actions > 1 {
		return Options{}, errors.New("choose exactly one command")
	}
	if portSpecified && (opts.Port < 1 || opts.Port > 65535) {
		return Options{}, errors.New("port must be between 1 and 65535")
	}
	if daemonWorker && !portSpecified {
		return Options{}, errors.New("--daemon-worker requires -p or --port")
	}
	if opts.Host != "" && opts.Action != ActionTunnel && opts.Action != ActionDaemonWorker {
		return Options{}, errors.New("--host can only be used with -p or --port")
	}
	if opts.Background && opts.Action != ActionTunnel {
		return Options{}, errors.New("--background can only be used with -p or --port")
	}
	if opts.DryRun && opts.Action != ActionSetup {
		return Options{}, errors.New("--dry-run can only be used with --setup")
	}
	if opts.SetupRoot != "" && opts.Action != ActionSetup {
		return Options{}, errors.New("--setup-root can only be used with --setup")
	}
	if opts.SetupRoot != "" && !opts.DryRun {
		return Options{}, errors.New("--setup-root requires --setup --dry-run")
	}
	if opts.ServerConfig != "" && opts.Action != ActionServer {
		return Options{}, errors.New("--server-config can only be used with --server")
	}
	return opts, nil
}

func cleanFlagError(err error) error {
	message := strings.TrimSpace(err.Error())
	message = strings.TrimPrefix(message, "flag provided but not defined: ")
	if strings.HasPrefix(message, "-") {
		return fmt.Errorf("unknown option %s", message)
	}
	return errors.New(message)
}
