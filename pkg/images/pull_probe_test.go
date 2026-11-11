package images

import (
	"context"
	"os"
	"testing"

	"github.com/pocketbox/pocketbox/pkg/dirs"
	"github.com/pocketbox/pocketbox/pkg/netg"
	"github.com/pocketbox/pocketbox/pkg/registry"
)

// TestPullBusybox hits Docker Hub when POCKETBOX_PROBE=1.
func TestPullBusybox(t *testing.T) {
	if os.Getenv("POCKETBOX_PROBE") != "1" {
		t.Skip("set POCKETBOX_PROBE=1 to hit Docker Hub")
	}
	l := dirs.Layout{Root: t.TempDir()}
	if err := l.Create(); err != nil {
		t.Fatal(err)
	}
	s := New(l)
	img, err := s.Pull(context.Background(), "busybox:1.36", func(p Progress) {
		t.Logf("%s layer %d/%d %s %d/%d", p.Event, p.Layer+1, p.Layers, p.Blob, p.Bytes, p.Total)
	})
	if err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(img.RootFS)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range ents {
		names = append(names, e.Name())
	}
	t.Logf("platform=%v size=%d rootfs=%v", img.Platform, img.Size, names)
}

func TestPullAlpine(t *testing.T) {
	if os.Getenv("POCKETBOX_PROBE") != "1" {
		t.Skip("set POCKETBOX_PROBE=1 to hit Docker Hub")
	}
	l := dirs.Layout{Root: t.TempDir()}
	if err := l.Create(); err != nil {
		t.Fatal(err)
	}
	s := New(l)
	img, err := s.Pull(context.Background(), "alpine:3.20", func(p Progress) {
		t.Logf("%s layer %d/%d %d/%d", p.Event, p.Layer+1, p.Layers, p.Bytes, p.Total)
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(img.RootFS + "/etc/alpine-release")
	t.Logf("alpine-release=%q size=%d", b, img.Size)
}

func TestPullMultiLayer(t *testing.T) {
	if os.Getenv("POCKETBOX_PROBE") != "1" {
		t.Skip("set POCKETBOX_PROBE=1 to hit Docker Hub")
	}
	l := dirs.Layout{Root: t.TempDir()}
	if err := l.Create(); err != nil {
		t.Fatal(err)
	}
	s := New(l)
	ref := os.Getenv("POCKETBOX_IMAGE")
	if ref == "" {
		ref = "python:3.12-alpine"
	}
	img, err := s.Pull(context.Background(), ref, func(p Progress) {
		t.Logf("%s layer %d/%d %d/%d", p.Event, p.Layer+1, p.Layers, p.Bytes, p.Total)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s layers=%d rootfs=%s", ref, len(img.Layers), img.RootFS)
}

// TestPullViaBridge performs the same pull through the file-based host
// bridge, with a netg.Serve loop standing in for the iOS host.
func TestPullViaBridge(t *testing.T) {
	if os.Getenv("POCKETBOX_PROBE") != "1" {
		t.Skip("set POCKETBOX_PROBE=1 to hit Docker Hub")
	}
	l := dirs.Layout{Root: t.TempDir()}
	if err := l.Create(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stop := make(chan struct{})
	defer close(stop)
	go netg.Serve(dir, stop)
	s := New(l)
	s.SetOptions(registry.Options{Transport: netg.NewTransport(dir)})
	img, err := s.Pull(context.Background(), "alpine:3.20", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(img.RootFS + "/etc/alpine-release")
	t.Logf("bridged alpine-release=%q", b)
}
