package cli

import (
	"github.com/pocketbox/pocketbox/pkg/dirs"
	"github.com/pocketbox/pocketbox/pkg/engine"
	"github.com/pocketbox/pocketbox/pkg/guestrunner"
	"github.com/pocketbox/pocketbox/pkg/native"
	"github.com/pocketbox/pocketbox/pkg/registry"
)

// containerRunner chooses where containers execute. The app runs them inside
// the Linux guest: the host process is told to start one, and the guest gets a
// fresh program image for it. Host builds use plain processes.
func ContainerRunner(l dirs.Layout) engine.Runner {
	if inGuest() {
		return guestrunner.New(l)
	}
	return native.New(l.Logs())
}

// imagesOptions returns the registry transport options for the guest build.
func imagesOptions(netDir string) registry.Options {
	return registry.Options{Transport: hostTransport(netDir)}
}
