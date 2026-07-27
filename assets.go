// Package reverse contains build assets that make the installed reverse
// binary self-contained for VPS setup.
package reverse

import "embed"

// ServerBuildContext is the source context used to build the hardened server
// image when setup is run outside a source checkout.
//
// Keep the allowlist explicit: setup must never capture files from the
// directory where the installed binary is invoked.
//
//go:embed Dockerfile assets.go go.mod go.sum cmd internal
var ServerBuildContext embed.FS
