package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// stateFile is the on-disk form of the engine state.
type stateFile struct {
	Version    int                   `json:"version"`
	Containers map[string]*Container `json:"containers"`
	Volumes    map[string]*Volume    `json:"volumes"`
}

const stateVersion = 1

func (e *Engine) load() error {
	raw, err := os.ReadFile(e.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var st stateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("engine: state file %s is corrupt: %w", e.statePath, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if st.Containers != nil {
		for id, c := range st.Containers {
			if c.State == StateRunning {
				// Nothing survives a restart yet: the runner is gone.
				c.State = StateExited
				c.Error = "engine restarted"
				if c.FinishedAt.IsZero() {
					c.FinishedAt = time.Now().UTC()
				}
			}
			e.containers[id] = c
		}
	}
	if st.Volumes != nil {
		for name, v := range st.Volumes {
			e.volumes[name] = v
			_ = os.MkdirAll(v.Mountpoint, 0o755)
		}
	}
	return nil
}

// Save persists the state. Callers usually want saveLocked.
func (e *Engine) Save() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.saveLocked()
}

func (e *Engine) saveLocked() error {
	e.saveMu.Lock()
	defer e.saveMu.Unlock()
	st := stateFile{Version: stateVersion, Containers: e.containers, Volumes: e.volumes}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(e.statePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), e.statePath)
}

// stateSaver coalesces frequent updates (status polling from a runner) into
// at most one write per second.
type stateSaver struct {
	e     *Engine
	mu    sync.Mutex
	timer *time.Timer
}

func (e *Engine) newSaver() *stateSaver { return &stateSaver{e: e} }

func (s *stateSaver) touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		return
	}
	s.timer = time.AfterFunc(time.Second, func() {
		s.mu.Lock()
		s.timer = nil
		s.mu.Unlock()
		_ = s.e.Save()
	})
}
