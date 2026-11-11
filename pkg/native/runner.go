// Package native runs containers as host processes (development hosts, CI and
// the Linux self-test builds). It is not used inside the iOS app, where the
// guest runtime executes containers instead.
package native

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/pocketbox/pocketbox/pkg/engine"
)

// Runner implements engine.Runner with exec.Cmd.
type Runner struct {
	LogDir string
}

func New(logDir string) *Runner { return &Runner{LogDir: logDir} }

type process struct {
	cmd *exec.Cmd
}

func (p *process) Wait() (int, error) {
	err := p.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), fmt.Errorf("exited on signal %d", ws.Signal())
		}
		return ee.ExitCode(), nil
	}
	return -1, err
}

func (p *process) Signal(sig int) error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Signal(syscall.Signal(sig))
}

func (p *process) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Start launches the container command.
func (r *Runner) Start(spec *engine.Spec) (engine.Process, error) {
	if len(spec.Args) == 0 {
		return nil, fmt.Errorf("native: container has no command")
	}
	args := append([]string{}, spec.Args...)
	var cmd *exec.Cmd
	if strings.HasPrefix(args[0], "/") {
		cmd = exec.Command(args[0], args[1:]...)
	} else {
		// No PATH lookup inside the image rootfs: the host's PATH is the
		// closest thing, and an absolute path in the image wins.
		if p, err := lookInRootfs(spec.RootFS, args[0], spec.Env); err == nil {
			cmd = exec.Command(p, args[1:]...)
			args[0] = p
		} else {
			return nil, fmt.Errorf("native: %w", err)
		}
	}
	cmd.Env = spec.Env
	// Containers see the image as "/". Without chroot (iSH host builds) the
	// best available equivalent is starting in the image and running the
	// program directly, which keeps absolute paths inside the image for
	// anything the program opens relative to a path it just received.
	if spec.Workdir != "" && spec.Workdir != "/" {
		return nil, fmt.Errorf("native: working directories other than / need a chroot-capable host")
	}
	cmd.Dir = spec.RootFS
	cmd.SysProcAttr = procAttr()

	logPath := filepath.Join(r.LogDir, spec.Container.ID+".log")
	if err := os.MkdirAll(r.LogDir, 0o755); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	pw := &prefixWriter{w: logf, prefix: ""}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, err
	}
	go func() { _ = logf.Close() }()
	return &process{cmd: cmd}, nil
}

// prefixWriter keeps log lines intact while serialising concurrent writes.
type prefixWriter struct {
	mu     sync.Mutex
	w      io.WriteCloser
	prefix string
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.w.Write(b)
}

// lookInRootfs resolves a bare command name against the image's PATH.
func lookInRootfs(rootfs, name string, env []string) (string, error) {
	pathVar := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			pathVar = strings.TrimPrefix(kv, "PATH=")
		}
	}
	var tried []string
	for _, dir := range strings.Split(pathVar, ":") {
		candidate := filepath.Join(rootfs, dir, name)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return candidate, nil
		}
		tried = append(tried, candidate)
	}
	return "", fmt.Errorf("exec: %q: executable file not found in $PATH", name)
}

// LookPathInImage is exported for the CLI to pre-validate commands.
func LookPathInImage(rootfs string, env []string, name string) (string, error) {
	if strings.Contains(name, "/") {
		p := filepath.Join(rootfs, name)
		if st, err := os.Stat(p); err == nil && st.Mode()&0o111 != 0 {
			return p, nil
		}
		return "", fmt.Errorf("%s: not found in image", name)
	}
	return lookInRootfs(rootfs, name, env)
}

// PortsOf parses "8080:80/tcp" style specs.
func PortsOf(specs []string) ([]engine.PortBinding, error) {
	var out []engine.PortBinding
	for _, s := range specs {
		proto := "tcp"
		if i := strings.LastIndex(s, "/"); i >= 0 {
			proto = s[i+1:]
			s = s[:i]
		}
		host, cont, ok := strings.Cut(s, ":")
		if !ok {
			cont = s
			host = ""
		}
		for _, c := range strings.Split(cont, ",") {
			pb := engine.PortBinding{ContainerPort: c, Protocol: proto}
			if host != "" {
				pb.HostPort = host
			}
			if _, err := strconv.Atoi(strings.SplitN(c, "-", 2)[0]); err != nil {
				return nil, fmt.Errorf("invalid port %q", c)
			}
			out = append(out, pb)
		}
	}
	return out, nil
}

var _ engine.Runner = (*Runner)(nil)
var _ = context.Background
