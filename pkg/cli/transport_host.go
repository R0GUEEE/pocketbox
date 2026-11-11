//go:build !pocketbox_guest

package cli

import "net/http"

// Host build: the registry client uses the default transport.
func hostTransport(string) http.RoundTripper { return nil }
