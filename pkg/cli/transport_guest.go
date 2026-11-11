//go:build pocketbox_guest

// Guest build: images are fetched through the runtime host, because the Linux
// VM's own network stack is not the phone's.

package cli

import (
	"net/http"

	"github.com/pocketbox/pocketbox/pkg/netg"
)

func hostTransport(netDir string) http.RoundTripper {
	return netg.NewTransport(netDir)
}
