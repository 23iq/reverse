package setup

import (
	"context"
	"net"
	"net/netip"
)

type Stage string

const (
	StageValidate    Stage = "validate"
	StageDNS         Stage = "dns"
	StagePorts       Stage = "ports"
	StagePackages    Stage = "packages"
	StageCertificate Stage = "certificate"
	StageConfig      Stage = "config"
	StageContainer   Stage = "container"
	StageCaddy       Stage = "caddy"
	StageComplete    Stage = "complete"
	StageRollback    Stage = "rollback"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusSuccess Status = "success"
	StatusWarning Status = "warning"
)

// Progress is safe to display in a TUI. Command never contains the setup
// password or its hash.
type Progress struct {
	Stage   Stage
	Status  Status
	Message string
	Command string
}

type ProgressFunc func(Progress)

type PackageManager string

const (
	PackageManagerAPT    PackageManager = "apt"
	PackageManagerDNF    PackageManager = "dnf"
	PackageManagerPacman PackageManager = "pacman"
)

type Options struct {
	Domain   string
	Password string
	Email    string

	DryRun    bool
	RootDir   string
	SourceDir string

	ServerImage   string
	ContainerName string

	PackageManager   PackageManager
	AllowDNSMismatch bool

	Runner           Runner
	Resolver         Resolver
	PublicIPSource   PublicIPSource
	PortChecker      PortChecker
	ReadinessChecker ReadinessChecker
	LookPath         func(string) (string, error)
	EffectiveUID     func() int
}

// Command is an executable invocation. Commands are always run directly,
// never through a shell.
type Command struct {
	Name string
	Args []string
	Dir  string
	Env  []string
}

type Runner interface {
	Run(context.Context, Command) (string, error)
}

type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type PublicIPSource interface {
	PublicIPs(context.Context) ([]netip.Addr, error)
}

type PortChecker interface {
	Check(context.Context, string) error
}

type ReadinessChecker interface {
	Wait(context.Context, string) error
}
