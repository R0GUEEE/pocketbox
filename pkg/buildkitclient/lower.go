package buildkitclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/pocketbox/pocketbox/pkg/oci"
)

// Step is what the engine must do for one instruction.
type Step struct {
	Instr Instruction
	// Kind is "run", "copy", "layer", or "" for metadata-only instructions.
	Kind string
	// RunArgs is the argv of a RUN step.
	RunArgs    []string
	WorkingDir string
	Env        map[string]string
	User       string
	Shell      []string
	// Layer is filled for COPY/ADD; the caller hands it to the layer writer.
	Layer *Layer
	// Config changes to apply for metadata instructions.
	Config Change
}

// Change is a partial image-config mutation.
type Change struct {
	SetUser       *string
	SetWorkdir    *string
	SetEntrypoint *[]string
	SetCmd        *[]string
	AddEnv        map[string]string
	AddLabels     map[string]string
	AddPorts      []string
	AddVolumes    []string
	AddOnBuild    []string
	SetStopSignal *string
	ResetShell    bool
}

// Lower converts parsed instructions into steps. It validates the subset the
// engine supports and fails loudly on the rest.
func Lower(f *File, ctx *BuildContext) ([]Step, *oci.ConfigSection, error) {
	var steps []Step
	cfg := &oci.ConfigSection{}
	workdir := "/"
	env := map[string]string{}
	user := ""
	shell := []string{"/bin/sh", "-c"}
	entrypointSet, cmdSet := false, false
	var entrypoint, cmd []string

	for _, instr := range f.Instrs {
		step := Step{Instr: instr, WorkingDir: workdir, Env: cloneMap(env), User: user, Shell: shell}
		switch instr.Cmd {
		case "ARG":
			// Values already resolved during parsing.
		case "FROM":
			// The base image is resolved by the caller; everything before the
			// first FROM in later stages is out of scope for this engine.
		case "RUN":
			if hasFlag(instr.Flags, "mount") || hasFlag(instr.Flags, "network") {
				return nil, nil, fmt.Errorf("dockerfile:%d: RUN %s is not supported by the guest builder", instr.Line, firstFlag(instr.Flags))
			}
			step.Kind = "run"
			if exec, ok := ExecForm(instr.Args); ok {
				step.RunArgs = exec
			} else {
				if strings.TrimSpace(instr.Args) == "" {
					return nil, nil, fmt.Errorf("dockerfile:%d: RUN with no command", instr.Line)
				}
				step.RunArgs = append(append([]string{}, shell...), expandVars(instr.Args, instr.Env))
			}
			if wd := instr.Env["PWD"]; wd != "" {
				workdir = wd
			}
		case "COPY":
			args := instr.Args
			if hasFlag(instr.Flags, "from") {
				return nil, nil, fmt.Errorf("dockerfile:%d: COPY --from needs multi-stage support, which this engine does not implement", instr.Line)
			}
			src, dest, err := copyArgs(args)
			if err != nil {
				return nil, nil, fmt.Errorf("dockerfile:%d: %w", instr.Line, err)
			}
			if !strings.HasPrefix(dest, "/") {
				dest = path.Join(workdir, dest)
			}
			l := &Layer{}
			if err := ctx.CopyToLayer(l, src, dest, false); err != nil {
				return nil, nil, fmt.Errorf("dockerfile:%d: %w", instr.Line, err)
			}
			step.Kind = "copy"
			step.Layer = l
		case "ADD":
			if strings.HasPrefix(strings.TrimSpace(instr.Args), "http://") || strings.HasPrefix(strings.TrimSpace(instr.Args), "https://") {
				return nil, nil, fmt.Errorf("dockerfile:%d: ADD from a URL is not supported; use RUN with a fetch step", instr.Line)
			}
			src, dest, err := copyArgs(instr.Args)
			if err != nil {
				return nil, nil, fmt.Errorf("dockerfile:%d: %w", instr.Line, err)
			}
			if !strings.HasPrefix(dest, "/") {
				dest = path.Join(workdir, dest)
			}
			l := &Layer{}
			if err := ctx.CopyToLayer(l, src, dest, false); err != nil {
				return nil, nil, fmt.Errorf("dockerfile:%d: %w", instr.Line, err)
			}
			step.Kind = "copy"
			step.Layer = l
		case "WORKDIR":
			wd := expandVars(instr.Args, instr.Env)
			if !strings.HasPrefix(wd, "/") {
				wd = path.Join(workdir, wd)
			}
			workdir = path.Clean(wd)
			step.Kind = "layer"
			step.Layer = &Layer{}
			step.Layer.AddDir(workdir, 0o755)
			step.Config.SetWorkdir = strPtr(workdir)
			cfg.WorkingDir = workdir
		case "ENV":
			add := map[string]string{}
			for _, kv := range parseEnvStyle(instr.Args) {
				val := expandVars(kv[1], instr.Env)
				add[kv[0]] = val
				env[kv[0]] = val
			}
			step.Config.AddEnv = add
			cfg.Env = append(cfg.Env, flattenEnv(add)...)
		case "LABEL":
			add := map[string]string{}
			for k, v := range KVForm(instr.Args) {
				add[k] = expandVars(v, instr.Env)
			}
			step.Config.AddLabels = add
			if cfg.Labels == nil {
				cfg.Labels = map[string]string{}
			}
			for k, v := range add {
				cfg.Labels[k] = v
			}
		case "EXPOSE":
			for _, w := range splitWords(expandVars(instr.Args, instr.Env)) {
				spec, _, err := PortSpec(w)
				if err != nil {
					return nil, nil, fmt.Errorf("dockerfile:%d: %w", instr.Line, err)
				}
				step.Config.AddPorts = append(step.Config.AddPorts, spec)
				if cfg.ExposedPorts == nil {
					cfg.ExposedPorts = map[string]struct{}{}
				}
				cfg.ExposedPorts[spec] = struct{}{}
			}
		case "VOLUME":
			var vols []string
			if exec, ok := ExecForm(instr.Args); ok {
				vols = exec
			} else {
				vols = splitWords(instr.Args)
			}
			step.Config.AddVolumes = vols
			if cfg.Volumes == nil {
				cfg.Volumes = map[string]struct{}{}
			}
			for _, v := range vols {
				cfg.Volumes[v] = struct{}{}
			}
		case "USER":
			user = expandVars(instr.Args, instr.Env)
			step.Config.SetUser = strPtr(user)
			cfg.User = user
		case "ENTRYPOINT":
			if exec, ok := ExecForm(instr.Args); ok {
				entrypoint = exec
			} else {
				entrypoint = append(append([]string{}, shell...), expandVars(instr.Args, instr.Env))
			}
			entrypointSet = true
			step.Config.SetEntrypoint = &entrypoint
			cfg.Entrypoint = entrypoint
			if !cmdSet {
				step.Config.SetCmd = &[]string{}
				cfg.Cmd = []string{}
			}
		case "CMD":
			if exec, ok := ExecForm(instr.Args); ok {
				cmd = exec
			} else {
				cmd = append(append([]string{}, shell...), expandVars(instr.Args, instr.Env))
			}
			cmdSet = true
			step.Config.SetCmd = &cmd
			cfg.Cmd = cmd
		case "SHELL":
			exec, ok := ExecForm(instr.Args)
			if !ok {
				return nil, nil, fmt.Errorf("dockerfile:%d: SHELL requires JSON array form", instr.Line)
			}
			shell = exec
			step.Config.ResetShell = true
			cfg.Shell = shell
		case "STOPSIGNAL":
			sig := expandVars(instr.Args, instr.Env)
			step.Config.SetStopSignal = strPtr(sig)
			cfg.StopSignal = sig
		case "ONBUILD":
			step.Config.AddOnBuild = []string{instr.Args}
			cfg.OnBuild = append(cfg.OnBuild, instr.Args)
		case "HEALTHCHECK":
			// Parsed for completeness; the guest runtime does not supervise
			// health checks, so the instruction is recorded and ignored.
			step.Kind = ""
		case "MAINTAINER":
			step.Config.AddLabels = map[string]string{"maintainer": instr.Args}
		default:
			return nil, nil, fmt.Errorf("dockerfile:%d: unsupported instruction %s", instr.Line, instr.Cmd)
		}
		_ = entrypointSet
		steps = append(steps, step)
	}
	return steps, cfg, nil
}

func flattenEnv(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func strPtr(s string) *string { return &s }

func hasFlag(flags []string, name string) bool {
	for _, f := range flags {
		if f == "--"+name || strings.HasPrefix(f, "--"+name+"=") {
			return true
		}
	}
	return false
}

func firstFlag(flags []string) string {
	if len(flags) == 0 {
		return ""
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(flags[0], "--"), "=")
	return "--" + name
}

// copyArgs splits COPY/ADD arguments into sources and destination.
func copyArgs(args string) ([]string, string, error) {
	if exec, ok := ExecForm(args); ok {
		if len(exec) < 2 {
			return nil, "", errors.New("COPY needs at least one source and a destination")
		}
		return exec[:len(exec)-1], exec[len(exec)-1], nil
	}
	words := splitWords(args)
	if len(words) < 2 {
		return nil, "", errors.New("COPY needs at least one source and a destination")
	}
	return words[:len(words)-1], words[len(words)-1], nil
}

// ApplyChange mutates a config section with a step's metadata change.
func ApplyChange(cfg *oci.ConfigSection, c Change) {
	if c.SetUser != nil {
		cfg.User = *c.SetUser
	}
	if c.SetWorkdir != nil {
		cfg.WorkingDir = *c.SetWorkdir
	}
	if c.SetEntrypoint != nil {
		cfg.Entrypoint = *c.SetEntrypoint
	}
	if c.SetCmd != nil {
		cfg.Cmd = *c.SetCmd
	}
	if c.SetStopSignal != nil {
		cfg.StopSignal = *c.SetStopSignal
	}
	if c.ResetShell {
		cfg.Shell = nil
	}
	for k, v := range c.AddEnv {
		replaced := false
		for i, kv := range cfg.Env {
			if strings.HasPrefix(kv, k+"=") {
				cfg.Env[i] = k + "=" + v
				replaced = true
			}
		}
		if !replaced {
			cfg.Env = append(cfg.Env, k+"="+v)
		}
	}
	for k, v := range c.AddLabels {
		if cfg.Labels == nil {
			cfg.Labels = map[string]string{}
		}
		cfg.Labels[k] = v
	}
	for _, p := range c.AddPorts {
		if cfg.ExposedPorts == nil {
			cfg.ExposedPorts = map[string]struct{}{}
		}
		cfg.ExposedPorts[p] = struct{}{}
	}
	for _, v := range c.AddVolumes {
		if cfg.Volumes == nil {
			cfg.Volumes = map[string]struct{}{}
		}
		cfg.Volumes[v] = struct{}{}
	}
	cfg.OnBuild = append(cfg.OnBuild, c.AddOnBuild...)
}

// MarshalConfig renders an image config deterministically.
func MarshalConfig(cfg oci.Config) ([]byte, error) {
	return json.Marshal(cfg)
}
