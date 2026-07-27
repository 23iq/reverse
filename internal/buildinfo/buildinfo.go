package buildinfo

// Version is replaced at build time with:
//
//	go build -ldflags "-X github.com/23iq/reverse/internal/buildinfo.Version=v1alpha"
var Version = "v1alpha"
