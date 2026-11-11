// Package dirs provides the on-device directory layout used by the engine.
//
// Inside the iOS app everything lives under an environment-provided data
// directory (Application Support). The guest runtime sees the same tree under
// a bind mount, so paths are stored relative to Root whenever possible.
package dirs

import (
	"errors"
	"os"
	"path/filepath"
)

// Layout is the directory structure of one engine installation.
type Layout struct {
	Root string
}

const (
	envDataDir = "POCKETBOX_DATA"
)

// Resolve finds the data directory: $POCKETBOX_DATA when set, otherwise
// <home>/.pocketbox. On iOS the app passes the Application Support container.
func Resolve() (Layout, error) {
	if d := os.Getenv(envDataDir); d != "" {
		return Layout{Root: filepath.Clean(d)}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return Layout{Root: filepath.Join(home, ".pocketbox")}, nil
}

func (l Layout) Content() string { return filepath.Join(l.Root, "content") }
func (l Layout) Images() string  { return filepath.Join(l.Root, "images") }
func (l Layout) Temp() string    { return filepath.Join(l.Root, "tmp") }
func (l Layout) Engine() string  { return filepath.Join(l.Root, "engine") }
func (l Layout) State() string   { return filepath.Join(l.Engine(), "state.json") }
func (l Layout) Logs() string    { return filepath.Join(l.Root, "logs") }
func (l Layout) Volumes() string { return filepath.Join(l.Root, "volumes") }
func (l Layout) Socket() string  { return filepath.Join(l.Engine(), "docker.sock") }
func (l Layout) RootFS() string  { return filepath.Join(l.Root, "rootfs") }
func (l Layout) Config() string  { return filepath.Join(l.Root, "config.json") }

// Create makes every directory and returns the first error.
func (l Layout) Create() error {
	for _, d := range []string{l.Root, l.Content(), l.Images(), l.Temp(), l.Engine(), l.Logs(), l.Volumes(), l.RootFS()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// Check verifies the layout is usable (root exists and is ours).
func (l Layout) Check() error {
	if l.Root == "" {
		return errors.New("dirs: empty root")
	}
	st, err := os.Stat(l.Root)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("dirs: root is not a directory")
	}
	return nil
}
