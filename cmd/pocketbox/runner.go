package main

import (
	"github.com/pocketbox/pocketbox/pkg/cli"
	"github.com/pocketbox/pocketbox/pkg/dirs"
	"github.com/pocketbox/pocketbox/pkg/engine"
)

// runtimeRunner selects how containers execute on this build.
//
// On a Linux host that is a plain process; inside the app's guest everything is
// one program image per command, so the host spawns the container through
// /pb/host (see the iOS runtime), and the guest side of that hand-off arrives
// as this runner's "start request" file.
func runtimeRunner(l dirs.Layout) engine.Runner {
	return cli.ContainerRunner(l)
}
