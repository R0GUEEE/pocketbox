package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pocketbox/pocketbox/pkg/digest"
	"github.com/pocketbox/pocketbox/pkg/dirs"
	"github.com/pocketbox/pocketbox/pkg/engine"
	"github.com/pocketbox/pocketbox/pkg/images"
)

func newTestServer(t *testing.T) (*server, *engine.Engine) {
	t.Helper()
	l := dirs.Layout{Root: t.TempDir()}
	if err := l.Create(); err != nil {
		t.Fatal(err)
	}
	imgs := images.New(l)
	e, err := engine.New(l, imgs)
	if err != nil {
		t.Fatal(err)
	}
	return &server{eng: e, logf: func(string, ...interface{}) {}}, e
}

// The docker CLI parses container list and inspect responses strictly enough
// that a missing key breaks `docker ps`; these tests pin the shape.
func TestContainerSummaryShape(t *testing.T) {
	c := &engine.Container{
		ID: strings.Repeat("a", 64), Name: "web", Image: "nginx:alpine",
		ImageID: "sha256:" + strings.Repeat("b", 64), State: engine.StateExited, ExitCode: 3,
		Ports:   []engine.PortBinding{{ContainerPort: "80", HostPort: "8080", Protocol: "tcp"}},
		Created: time.Now().Add(-2 * time.Minute),
	}
	raw, err := json.Marshal(containerSummary(c))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"Id", "Names", "Image", "ImageID", "Command", "Created", "State", "Status", "Ports", "Labels", "HostConfig", "NetworkSettings", "Mounts"} {
		if _, ok := m[key]; !ok {
			t.Errorf("container summary missing %q", key)
		}
	}
	names := m["Names"].([]interface{})
	if names[0] != "/web" {
		t.Errorf("Names[0] = %v", names[0])
	}
	if m["Status"] != "Exited (3)" {
		t.Errorf("Status = %v", m["Status"])
	}
	if m["Created"].(float64) <= 0 {
		t.Errorf("Created must be a unix timestamp")
	}
}

func TestInspectShape(t *testing.T) {
	s, e := newTestServer(t)
	_ = e
	img := &images.Image{Reference: "busybox:1.36", Digest: digest.Digest("sha256:" + strings.Repeat("c", 64))}
	if err := e.Images.Save(img); err != nil {
		t.Fatal(err)
	}
	c, err := e.Create(engine.CreateOptions{Image: "busybox:1.36", Cmd: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(s.inspect(c))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	state, ok := m["State"].(map[string]interface{})
	if !ok {
		t.Fatal("missing State")
	}
	for _, key := range []string{"Status", "Running", "Paused", "Restarting", "OOMKilled", "ExitCode", "Error", "Pid"} {
		if _, ok := state[key]; !ok {
			t.Errorf("state missing %q", key)
		}
	}
	cfg, ok := m["Config"].(map[string]interface{})
	if !ok {
		t.Fatal("missing Config")
	}
	for _, key := range []string{"Hostname", "Image", "Env", "Cmd", "Entrypoint", "WorkingDir", "User", "Labels", "Tty", "OpenStdin"} {
		if _, ok := cfg[key]; !ok {
			t.Errorf("config missing %q", key)
		}
	}
}

func TestSplitRef(t *testing.T) {
	cases := map[string][2]string{
		"alpine:3.20":        {"alpine", "3.20"},
		"ghcr.io/o/app:v1":   {"ghcr.io/o/app", "v1"},
		"localhost:5000/app": {"localhost:5000/app", "latest"},
		"busybox":            {"busybox", "latest"},
	}
	for in, want := range cases {
		repo, tag := splitRef(in)
		if repo != want[0] || tag != want[1] {
			t.Errorf("%s -> %s:%s, want %s:%s", in, repo, tag, want[0], want[1])
		}
	}
}
