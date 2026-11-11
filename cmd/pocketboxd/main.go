// Command pocketboxd builds the engine as a c-archive so a host process can
// embed it. The iOS app ships this archive linked into the guest runtime's
// scratch build; on a host it is useful for tests that call into the engine
// without going through a process boundary.
package main

import "C"

import (
	"fmt"
	"os"

	"github.com/pocketbox/pocketbox/pkg/pocketboxd"
)

// pocketbox_version writes the engine version through the host's stdout.
//
//export pocketbox_version
func pocketbox_version() int {
	_, err := fmt.Fprintf(os.Stdout, "%s\n", pocketboxd.Build)
	if err != nil {
		return 1
	}
	return 0
}

// pocketbox_info prints the JSON description of this build.
//
//export pocketbox_info
func pocketbox_info(dataRoot *C.char) int {
	info := pocketboxd.Describe(C.GoString(dataRoot))
	_, err := fmt.Fprintf(os.Stdout, "%+v\n", info)
	if err != nil {
		return 1
	}
	return 0
}

func main() {}
