package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pocketbox/pocketbox/pkg/images"
)

// ErrNotRunning is returned by Stop/Kill for a stopped container.
var ErrNotRunning = errors.New("engine: container is not running")

// running tracks the live process of a container (never persisted).
type running struct {
	proc    Process
	started time.Time
	cancel  chan struct{}
}

func (e *Engine) runState() *sync.Map { return e.procs }

// Start launches a created (or exited) container.
func (e *Engine) Start(ref string) error {
	if e.Runner == nil {
		return errors.New("engine: no runner configured")
	}
	e.mu.Lock()
	c, err := e.getLocked(ref)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	if c.State == StateRunning {
		e.mu.Unlock()
		return nil
	}
	img, err := e.Images.Get(c.Image)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	spec := &Spec{
		Container: c,
		Image:     img,
		RootFS:    c.RootFS,
		Args:      resolveArgs(c),
		Env:       e.containerEnvLocked(c),
		User:      c.User,
		Workdir:   c.WorkingDir,
		Hostname:  c.Hostname,
		Mounts:    c.Mounts,
	}
	e.mu.Unlock()

	proc, err := e.Runner.Start(spec)
	if err != nil {
		e.setExited(c.ID, 127, err.Error())
		return err
	}
	now := time.Now().UTC()
	e.mu.Lock()
	if cur, ok := e.containers[c.ID]; ok {
		cur.State = StateRunning
		cur.StartedAt = now
		cur.FinishedAt = time.Time{}
		cur.ExitCode = 0
		cur.Error = ""
		cur.Pid = proc.Pid()
	}
	_ = e.saveLocked()
	e.mu.Unlock()

	r := &running{proc: proc, started: now, cancel: make(chan struct{})}
	e.runState().Store(c.ID, r)
	go e.supervise(c.ID, r)
	return nil
}

func (e *Engine) supervise(id string, r *running) {
	// Tag every log line this container writes with its timestamp prefix so
	// docker logs -t can be served without buffering the whole file.
	code, err := r.proc.Wait()
	close(r.cancel)
	e.runState().Delete(id)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	e.setExited(id, code, msg)
	e.applyRestartPolicy(id)
}

func (e *Engine) setExited(id string, code int, msg string) {
	now := time.Now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.containers[id]
	if !ok {
		return
	}
	c.State = StateExited
	c.ExitCode = code
	c.Error = msg
	c.FinishedAt = now
	c.Pid = 0
	_ = e.saveLocked()
}

func (e *Engine) applyRestartPolicy(id string) {
	e.mu.RLock()
	c, ok := e.containers[id]
	var policy string
	var autoRemove bool
	if ok {
		policy = c.RestartPolicy
		autoRemove = c.AutoRemove
	}
	e.mu.RUnlock()
	if !ok {
		return
	}
	if autoRemove {
		_ = e.Remove(id, true)
		return
	}
	switch {
	case policy == "always", policy == "unless-stopped":
		_ = e.Start(id)
	}
}

// resolveArgs joins the entrypoint and command.
func resolveArgs(c *Container) []string {
	args := make([]string, 0, len(c.Entrypoint)+len(c.Cmd))
	args = append(args, c.Entrypoint...)
	args = append(args, c.Cmd...)
	return args
}

// containerEnvLocked extends the configured environment with the variables
// Docker defines for every container.
func (e *Engine) containerEnvLocked(c *Container) []string {
	env := append([]string{}, c.Env...)
	env = append(env,
		"HOSTNAME="+c.Hostname,
		"HOME="+homeFor(c.User),
		"PATH="+defaultPath,
	)
	return env
}

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func homeFor(user string) string {
	if user == "" || user == "root" || user == "0" || strings.HasPrefix(user, "0:") || strings.HasPrefix(user, "root:") {
		return "/root"
	}
	return "/"
}

// Stop sends SIGTERM, waits for timeout, then SIGKILL.
func (e *Engine) Stop(ref string, timeout time.Duration) error {
	c, err := e.Get(ref)
	if err != nil {
		return err
	}
	v, ok := e.runState().Load(c.ID)
	if !ok {
		if c.State == StateRunning {
			e.setExited(c.ID, 137, "runner lost")
		}
		return nil
	}
	r := v.(*running)
	_ = r.proc.Signal(15)
	select {
	case <-r.cancel:
		return nil
	case <-time.After(timeout):
	}
	_ = r.proc.Signal(9)
	select {
	case <-r.cancel:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("engine: container %s did not stop", c.Name)
	}
}

// Kill sends SIGKILL.
func (e *Engine) Kill(ref string) error {
	c, err := e.Get(ref)
	if err != nil {
		return err
	}
	v, ok := e.runState().Load(c.ID)
	if !ok {
		return ErrNotRunning
	}
	return v.(*running).proc.Signal(9)
}

// Signal delivers a signal number to the container.
func (e *Engine) Signal(ref string, sig int) error {
	c, err := e.Get(ref)
	if err != nil {
		return err
	}
	v, ok := e.runState().Load(c.ID)
	if !ok {
		return ErrNotRunning
	}
	return v.(*running).proc.Signal(sig)
}

// Running reports whether a container currently has a live process.
func (e *Engine) Running(id string) bool {
	_, ok := e.runState().Load(id)
	return ok
}

// LogPath is the file a container's stdio is appended to.
func (e *Engine) LogPath(id string) string {
	return filepath.Join(e.logDir, id+".log")
}

// LogsFile opens the container's log file.
func (e *Engine) LogsFile(id string) (*os.File, error) {
	return os.OpenFile(e.LogPath(id), os.O_RDONLY|os.O_CREATE, 0o644)
}

// TrimLogs removes a container's log file.
func (e *Engine) TrimLogs(id string) error {
	return os.Remove(e.LogPath(id))
}

// Wait blocks until the container is not running.
func (e *Engine) Wait(ref string) (int, error) {
	c, err := e.Get(ref)
	if err != nil {
		return 0, err
	}
	v, ok := e.runState().Load(c.ID)
	if !ok {
		return c.ExitCode, nil
	}
	<-v.(*running).cancel
	c2, err := e.Get(ref)
	if err != nil {
		return 0, err
	}
	return c2.ExitCode, nil
}

// Pull pulls an image and returns it.
func (e *Engine) Pull(ctx context.Context, ref string, progress func(images.Progress)) (*images.Image, error) {
	return e.Images.Pull(ctx, ref, progress)
}
