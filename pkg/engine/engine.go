// Package engine is the runtime state machine: containers, volumes, logs and
// the lifecycle operations the daemon and CLI expose.
package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"crypto/rand"
	"encoding/hex"

	"github.com/pocketbox/pocketbox/pkg/dirs"
	"github.com/pocketbox/pocketbox/pkg/images"
)

// Container states.
const (
	StateCreated = "created"
	StateRunning = "running"
	StateExited  = "exited"
	StateDead    = "dead"
)

// HealthCheck is a Docker healthcheck configuration.
type HealthCheck struct {
	Test     []string `json:"Test,omitempty"`
	Interval int64    `json:"Interval,omitempty"`
	Timeout  int64    `json:"Timeout,omitempty"`
	Retries  int      `json:"Retries,omitempty"`
	Start    int64    `json:"StartPeriod,omitempty"`
}

// Mount is a bind or volume mount.
type Mount struct {
	Type   string `json:"Type"` // bind | volume
	Source string `json:"Source"`
	Target string `json:"Target"`
	// GuestSource is Source expressed relative to the engine data root, so a
	// container's filesystem view is portable into the guest runtime.
	GuestSource string `json:"GuestSource,omitempty"`
	ReadOnly    bool   `json:"ReadOnly,omitempty"`
}

// PortBinding is one published port.
type PortBinding struct {
	ContainerPort string `json:"containerPort"`
	HostPort      string `json:"hostPort,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
}

// Container is the persisted record of one container.
type Container struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	ImageID     string            `json:"imageID"`
	Created     time.Time         `json:"created"`
	StartedAt   time.Time         `json:"startedAt,omitempty"`
	FinishedAt  time.Time         `json:"finishedAt,omitempty"`
	State       string            `json:"state"`
	ExitCode    int               `json:"exitCode"`
	Error       string            `json:"error,omitempty"`
	Pid         int               `json:"pid,omitempty"`
	RootFS      string            `json:"rootFS"`
	Entrypoint  []string          `json:"entrypoint,omitempty"`
	Cmd         []string          `json:"cmd,omitempty"`
	Env         []string          `json:"env,omitempty"`
	WorkingDir  string            `json:"workingDir,omitempty"`
	User        string            `json:"user,omitempty"`
	Hostname    string            `json:"hostname,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Mounts      []Mount           `json:"mounts,omitempty"`
	Ports       []PortBinding     `json:"ports,omitempty"`
	Privileged  bool              `json:"privileged,omitempty"`
	Network     string            `json:"network,omitempty"`
	Healthcheck *HealthCheck      `json:"healthcheck,omitempty"`
	// Restart policy: "no", "always", "on-failure[:n]", "unless-stopped".
	RestartPolicy string `json:"restartPolicy,omitempty"`
	// AutoRemove deletes the record when the process exits.
	AutoRemove bool `json:"autoRemove,omitempty"`
	// Guest marks containers executed by the guest runtime (ish), which reports
	// guest pids instead of host pids.
	Guest bool `json:"guest,omitempty"`
}

// Volume is a named volume.
type Volume struct {
	Name       string            `json:"name"`
	Created    time.Time         `json:"created"`
	Labels     map[string]string `json:"labels,omitempty"`
	Mountpoint string            `json:"mountpoint"`
}

// CreateOptions is the input to Create.
type CreateOptions struct {
	Name          string
	Image         string
	Env           []string
	Entrypoint    []string
	Cmd           []string
	WorkingDir    string
	User          string
	Hostname      string
	Labels        map[string]string
	Mounts        []Mount
	Ports         []PortBinding
	Privileged    bool
	Network       string
	Healthcheck   *HealthCheck
	RestartPolicy string
	AutoRemove    bool
	Tty           bool
	StdinOpen     bool
	// Init sets the container's init/PID 1 behaviour.
	Init bool
}

// Engine is the lifecycle owner. All methods are safe for concurrent use.
type Engine struct {
	Layout dirs.Layout
	Images *images.Store

	mu         sync.RWMutex
	containers map[string]*Container
	volumes    map[string]*Volume
	// Runner executes a container; the daemon installs one.
	Runner Runner
	// LogDir holds per-container log files.
	logDir    string
	statePath string
	saveMu    sync.Mutex
	// procs holds *running entries keyed by container id.
	procs *sync.Map
	saver *stateSaver
}

// Runner abstracts process execution so the engine can run on a host (native
// spawn) or inside the guest runtime.
type Runner interface {
	// Start launches the container process and returns a handle.
	Start(spec *Spec) (Process, error)
}

// Process is a running container.
type Process interface {
	Wait() (int, error)
	Signal(sig int) error
	Pid() int
}

// Spec is the fully resolved execution request handed to a Runner.
type Spec struct {
	Container *Container
	Image     *images.Image
	// RootFS is the host directory of the image rootfs.
	RootFS   string
	Args     []string
	Env      []string
	User     string
	Workdir  string
	Hostname string
	Mounts   []Mount
}

func New(l dirs.Layout, imgs *images.Store) (*Engine, error) {
	e := &Engine{
		Layout:     l,
		Images:     imgs,
		containers: map[string]*Container{},
		volumes:    map[string]*Volume{},
		logDir:     l.Logs(),
		statePath:  l.State(),
		procs:      &sync.Map{},
		saver:      nil,
	}
	e.saver = e.newSaver()
	if err := e.load(); err != nil {
		return nil, err
	}
	return e, nil
}

var (
	ErrNoSuchContainer = errors.New("engine: no such container")
	ErrConflict        = errors.New("engine: conflict")
)

func newID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))[:64]
	}
	return hex.EncodeToString(b[:])
}

// List returns containers sorted by creation time (oldest first), or filtered
// by All=false to running ones.
func (e *Engine) List(all bool) []*Container {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*Container, 0, len(e.containers))
	for _, c := range e.containers {
		if !all && c.State != StateRunning {
			continue
		}
		cp := *c
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Get finds a container by id, id prefix or name.
func (e *Engine) Get(ref string) (*Container, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.getLocked(ref)
}

func (e *Engine) getLocked(ref string) (*Container, error) {
	if c, ok := e.containers[ref]; ok {
		cp := *c
		return &cp, nil
	}
	for _, c := range e.containers {
		if c.Name == ref || strings.HasPrefix(c.ID, ref) {
			cp := *c
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNoSuchContainer, ref)
}

// Create records a new container in the created state.
func (e *Engine) Create(opts CreateOptions) (*Container, error) {
	if opts.Image == "" {
		return nil, errors.New("engine: image is required")
	}
	img, err := e.Images.Get(opts.Image)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	name := opts.Name
	if name == "" {
		name = fmt.Sprintf("%s_%s", sanitizeName(lastSegment(img.Reference)), randSuffix(6))
	}
	if _, err := e.getLocked(name); err == nil {
		return nil, fmt.Errorf("%w: name %q is already in use", ErrConflict, name)
	}
	for _, c := range e.containers {
		if c.Name == name {
			return nil, fmt.Errorf("%w: name %q is already in use", ErrConflict, name)
		}
	}

	env := mergeEnv(img.Config.Config.Env, opts.Env)
	entry := opts.Entrypoint
	if entry == nil {
		entry = img.Config.Config.Entrypoint
	}
	cmd := opts.Cmd
	if cmd == nil {
		cmd = img.Config.Config.Cmd
	}
	workdir := opts.WorkingDir
	if workdir == "" {
		workdir = img.Config.Config.WorkingDir
	}
	user := opts.User
	if user == "" {
		user = img.Config.Config.User
	}
	hostname := opts.Hostname
	if hostname == "" {
		hostname = name
	}
	net := opts.Network
	if net == "" {
		net = "bridge"
	}

	c := &Container{
		ID:            newID(),
		Name:          name,
		Image:         img.Reference,
		ImageID:       string(img.Digest),
		Created:       time.Now().UTC(),
		State:         StateCreated,
		RootFS:        img.RootFS,
		Entrypoint:    entry,
		Cmd:           cmd,
		Env:           env,
		WorkingDir:    workdir,
		User:          user,
		Hostname:      hostname,
		Labels:        opts.Labels,
		Mounts:        opts.Mounts,
		Ports:         opts.Ports,
		Privileged:    opts.Privileged,
		Network:       net,
		Healthcheck:   opts.Healthcheck,
		RestartPolicy: opts.RestartPolicy,
		AutoRemove:    opts.AutoRemove,
	}
	// Names volumes the container references so the mount is persistent.
	for i := range c.Mounts {
		if c.Mounts[i].Type == "volume" {
			if c.Mounts[i].GuestSource == "" {
				c.Mounts[i].GuestSource = "volumes/" + c.Mounts[i].Source
			}
			e.ensureVolumeLocked(c.Mounts[i].Source, nil)
		}
	}
	e.containers[c.ID] = c
	if err := e.saveLocked(); err != nil {
		delete(e.containers, c.ID)
		return nil, err
	}
	cp := *c
	return &cp, nil
}

func lastSegment(ref string) string {
	r := ref
	if i := strings.LastIndexByte(r, '/'); i >= 0 {
		r = r[i+1:]
	}
	if i := strings.IndexByte(r, ':'); i >= 0 {
		r = r[:i]
	}
	if i := strings.IndexByte(r, '@'); i >= 0 {
		r = r[:i]
	}
	return r
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimLeft(b.String(), "._-")
	if out == "" {
		out = "container"
	}
	return out
}

func randSuffix(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:n]
	}
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	out := make([]byte, n)
	for i := range b {
		out[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(out)
}

func mergeEnv(base, overrides []string) []string {
	idx := map[string]int{}
	var out []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		idx[k] = len(out)
		out = append(out, kv)
	}
	for _, kv := range overrides {
		k, _, _ := strings.Cut(kv, "=")
		if i, ok := idx[k]; ok {
			out[i] = kv
		} else {
			idx[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

// ensureVolumeLocked creates a volume if it does not exist (caller holds mu).
func (e *Engine) ensureVolumeLocked(name string, labels map[string]string) *Volume {
	if v, ok := e.volumes[name]; ok {
		return v
	}
	v := &Volume{Name: name, Created: time.Now().UTC(), Labels: labels,
		Mountpoint: filepath.Join(e.Layout.Volumes(), name)}
	_ = os.MkdirAll(v.Mountpoint, 0o755)
	e.volumes[name] = v
	return v
}

// Volumes lists volumes.
func (e *Engine) Volumes() []*Volume {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*Volume, 0, len(e.volumes))
	for _, v := range e.volumes {
		cp := *v
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Volume returns one volume by name.
func (e *Engine) Volume(name string) (*Volume, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	v, ok := e.volumes[name]
	if !ok {
		return nil, fmt.Errorf("engine: no such volume %q", name)
	}
	cp := *v
	return &cp, nil
}

// CreateVolume makes a named volume.
func (e *Engine) CreateVolume(name string, labels map[string]string) (*Volume, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if name == "" {
		name = newID()[:16]
	}
	if v, ok := e.volumes[name]; ok {
		cp := *v
		return &cp, nil
	}
	v := e.ensureVolumeLocked(name, labels)
	if err := e.saveLocked(); err != nil {
		return nil, err
	}
	cp := *v
	return &cp, nil
}

// RemoveVolume deletes a volume and its data.
func (e *Engine) RemoveVolume(name string, force bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.volumes[name]; !ok {
		return fmt.Errorf("engine: no such volume %q", name)
	}
	if !force {
		for _, c := range e.containers {
			for _, m := range c.Mounts {
				if m.Type == "volume" && m.Source == name {
					return fmt.Errorf("engine: volume %q is in use by %s", name, c.Name)
				}
			}
		}
	}
	_ = os.RemoveAll(e.volumes[name].Mountpoint)
	delete(e.volumes, name)
	return e.saveLocked()
}

// Remove deletes a container record and its writable overlay data.
func (e *Engine) Remove(ref string, force bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, err := e.getLocked(ref)
	if err != nil {
		return err
	}
	if c.State == StateRunning && !force {
		return fmt.Errorf("%w: container %s is running (stop it or use force)", ErrConflict, c.Name)
	}
	delete(e.containers, c.ID)
	return e.saveLocked()
}

// Rename changes a container's name.
func (e *Engine) Rename(ref, newName string) error {
	if newName == "" {
		return errors.New("engine: empty name")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c, err := e.getLocked(ref)
	if err != nil {
		return err
	}
	for _, other := range e.containers {
		if other.Name == newName {
			return fmt.Errorf("%w: name %q is already in use", ErrConflict, newName)
		}
	}
	e.containers[c.ID].Name = newName
	return e.saveLocked()
}
