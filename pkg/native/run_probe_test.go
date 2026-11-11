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

func TestRunBusyboxNative(t *testing.T) {
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
	e, err := engine.New(l, imgs)
	if err != nil {
		t.Fatal(err)
	}
	e.Runner = native.New(l.Logs())
	c, err := e.Create(engine.CreateOptions{Image: "busybox:1.36",
		Entrypoint: []string{"/bin/sh", "-c", "echo hello from $(hostname); id; ls / | tr '\\n' ' '"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(c.ID); err != nil {
		t.Fatal(err)
	}
	code, err := e.Wait(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(e.LogPath(c.ID))
	t.Logf("exit=%d log=%q", code, log)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
}
