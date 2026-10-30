package version

// To overide at build:
//
//	go build -ldflags="-X watui/internal/version.Version=0.X.X" ./cmd/watui
var Version = "0.4.0"
