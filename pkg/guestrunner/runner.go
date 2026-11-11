// Package guestrunner starts containers from inside the Linux guest.
//
// The app's host process owns process creation: the guest asks it to start the
// container's program, the host notifies when it exits, and the guest keeps a
// placeholder process that reports the exit status to the engine. That is the
// only shape that works in a runtime where the guest cannot fork reliably.
package guestrunner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pocketbox/pocketbox/pkg/dirs"
	"github.com/pocketbox/pocketbox/pkg/engine"
)

// HostDir is where the guest leaves start requests for the host.
const HostDir = "host/run"

// Runner implements engine.Runner.
type Runner struct {
	layout dirs.Layout
	mu     sync.Mutex
}

func New(l dirs.Layout) *Runner { return &Runner{layout: l} }

// Request is what the host reads to start a container.
type Request struct {
	ID       string         `json:"id"`
	Args     []string       `json:"args"`
	Env      []string       `json:"env"`
	User     string         `json:"user"`
	Workdir  string         `json:"workdir"`
	Hostname string         `json:"hostname"`
	RootFS   string         `json:"rootfs"`
	LogPath  string         `json:"logPath"`
	Mounts   []engine.Mount `json:"mounts,omitempty"`
	Status   string         `json:"statusPath"`
}

type process struct {
	id     string
	runner *Runner
	done   chan struct{}
	code   int
	once   sync.Once
}

func (p *process) Wait() (int, error) {
	<-p.done
	return p.code, nil
}

func (p *process) Signal(sig int) error {
	// The host watches for a request file with this suffix.
	path := filepath.Join(p.runner.layout.Root, HostDir, "signal-"+p.id)
	return os.WriteFile(path, []byte(fmt.Sprintf("%d", sig)), 0o644)
}

func (p *process) Pid() int { return 0 }

// Start writes the request and waits for the host's status file.
func (r *Runner) Start(spec *engine.Spec) (engine.Process, error) {
	if len(spec.Args) == 0 {
		return nil, errors.New("guestrunner: container has no command")
	}
	id := spec.Container.ID
	dir := filepath.Join(r.layout.Root, HostDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	reqPath := filepath.Join(dir, id+".json")
	statusPath := filepath.Join(dir, id+".status")
	_ = os.Remove(statusPath)
	req := Request{
		ID: id, Args: spec.Args, Env: spec.Env, User: spec.User,
		Workdir: spec.Workdir, Hostname: spec.Hostname, RootFS: spec.RootFS,
		LogPath: filepath.Join(r.layout.Logs(), id+".log"), Mounts: spec.Mounts,
		Status: statusPath,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	tmp := reqPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, reqPath); err != nil {
		return nil, err
	}
	p := &process{id: id, runner: r, done: make(chan struct{})}
	go p.watch(statusPath)
	return p, nil
}

// watch polls for the host's exit status; the host removes the request file
// once it has started the program.
func (p *process) watch(statusPath string) {
	deadline := time.Now().Add(24 * time.Hour)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(statusPath); err == nil {
			code := 0
			fmt.Sscanf(string(raw), "%d", &code)
			_ = os.Remove(statusPath)
			p.once.Do(func() { p.code = code; close(p.done) })
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	p.once.Do(func() { p.code = 137; close(p.done) })
}

var _ engine.Runner = (*Runner)(nil)
