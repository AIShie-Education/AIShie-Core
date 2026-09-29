// Package version carries build metadata stamped in by the linker:
//
//	go build -ldflags "-X .../internal/version.Version=v0.1.0 ..."
package version

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String is the one-line form printed by `aishie-core version`.
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
