package native_test

import (
	"context"
	"os"
	"testing"

	"github.com/pocketbox/pocketbox/pkg/dirs"
	"github.com/pocketbox/pocketbox/pkg/engine"
	"github.com/pocketbox/pocketbox/pkg/images"
	"github.com/pocketbox/pocketbox/pkg/native"
)

// TestDockerRunJoin mirrors `docker run busybox cat /dev/urandom | head -c 5`:
// the image has no ENTRYPOINT, so the command line is the command as written,
// and the shell pipeline has to work against the container's stdio.
func TestDockerRunJoin(t *testing.T) {
	if os.Getenv("POCKETBOX_PROBE") != "1" {
		t.Skip("set POCKETBOX_PROBE=1")
	}
	l := dirs.Layout{Root: t.TempDir()}
	if err := l.Create(); err != nil {
		t.Fatal(err)
	}
	imgs := images.New(l)
	if _, err := imgs.Pull(context.Background(), "busybox:1.36", nil); err != nil {
		t.Fatal(err)
	}
	e, _ := engine.New(l, imgs)
	e.Runner = native.New(l.Logs())
	for _, tc := range []struct{ name string; cmd []string }{
		{"pipeline", []string{"/bin/sh", "-c", "cat /dev/urandom | head -c 5 | wc -c"}},
		{"stdout", []string{"/bin/sh", "-c", "echo plain"}},
	} {
		c, err := e.Create(engine.CreateOptions{Image: "busybox:1.36", Entrypoint: tc.cmd})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Start(c.ID); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		code, _ := e.Wait(c.ID)
		data, _ := os.ReadFile(e.LogPath(c.ID))
		t.Logf("%s: exit=%d log=%q", tc.name, code, data)
		if len(data) == 0 {
			t.Errorf("%s produced no log", tc.name)
		}
	}
}
