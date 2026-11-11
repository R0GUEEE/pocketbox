package api

import (
	"os"

	"github.com/pocketbox/pocketbox/pkg/engine"
	"github.com/pocketbox/pocketbox/pkg/images"
)

// enginePullProgress is the pull callback shape the server streams.
type enginePullProgress = images.Progress

func splitRef(ref string) (string, string) {
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == ':' {
			return ref[:i], ref[i+1:]
		}
		if ref[i] == '/' {
			break
		}
	}
	return ref, "latest"
}

func readLog(e *engine.Engine, c *engine.Container) (string, error) {
	data, err := os.ReadFile(e.LogPath(c.ID))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}
