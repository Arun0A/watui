package version

// Version represents the current version of watui.
// To change the version manually, edit this value directly.
// It can also be overridden at build time via:
//
//	go build -ldflags="-X watui/internal/version.Version=0.3.1" ./cmd/watui
var Version = "0.3.1"
