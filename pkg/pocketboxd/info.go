// PocketBoxInfo returns the guest-visible description of the engine build. It
// is the one entry point the Swift side calls directly instead of routing
// through argv, so a broken guest boot can still show what it is running.
package pocketboxd

import "runtime"

// Info describes the embedded engine.
type Info struct {
	Version  string `json:"version"`
	Runtime  string `json:"runtime"`
	Platform string `json:"platform"`
	DataRoot string `json:"dataRoot"`
}

// Build is stamped by the release build.
var Build = "0.1.0"

// Describe returns the engine description as JSON.
func Describe(dataRoot string) Info {
	return Info{
		Version:  Build,
		Runtime:  "linux guest",
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		DataRoot: dataRoot,
	}
}
