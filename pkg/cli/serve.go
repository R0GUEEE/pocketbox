package cli

import (
	"context"
	"io"

	"github.com/pocketbox/pocketbox/pkg/api"
	"github.com/pocketbox/pocketbox/pkg/engine"
)

// Serve runs the Docker-compatible API on listen until ctx ends.
func Serve(ctx context.Context, e *engine.Engine, listen string, log io.Writer) error {
	return api.Serve(ctx, e, listen, log)
}
