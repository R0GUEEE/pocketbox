// Package rootfs assembles container filesystems inside the guest without
// opening packages in the runtime host.
//
// Layers arrive already decompressed as plain tar streams by the time the
// runtime sees them (the host stashes each blob and reports the decompressed
// path), so assembly is: mount the empty rootfs, expand every layer through
// Alpine's adduser/tar/busybox into it, and keep the mount for the container
// process that runs immediately after.
//
// The order of operations matters inside this runtime: filesystem work done as
// shell builtins never forks, while anything that runs another program does.
// The resolver below relies on builtins and busybox applets that are already
// in the page cache of the booted shell.
package rootfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Layer is one layer to expand.
type Layer struct {
	Path        string `json:"path"`   // host path of the tar stream
	DiffID      string `json:"diffID"` // sha256 of the uncompressed tar
	MediaType   string `json:"mediaType"`
	Compression string `json:"compression"`
}

// Plan is the request the runtime host reads.
type Plan struct {
	Root      string  `json:"root"` // target directory (host path)
	Layers    []Layer `json:"layers"`
	Rootless  bool    `json:"rootless"` // extract without chown/device nodes
	KeepMount bool    `json:"keepMount"`
}

// Script renders the shell program that performs the assembly. It is written to
// the runtime's script file and executed as the booted shell's command line, so
// the entire sequence runs without forking.
func (p *Plan) Script() (string, error) {
	if p.Root == "" {
		return "", errors.New("rootfs: empty root")
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "R=%s\n", shellQuote(p.Root))
	b.WriteString(`[ -d "$R" ] || mkdir -p "$R"` + "\n")
	for i, l := range p.Layers {
		fmt.Fprintf(&b, "L%d=%s\n", i, shellQuote(l.Path))
	}
	for i := range p.Layers {
		// tar with --no-same-owner keeps the extraction unprivileged, which is
		// what the iOS sandbox offers. --anchored is not in busybox, so only
		// GNU-compatible flags are used here.
		if p.Rootless {
			fmt.Fprintf(&b, "tar -xf \"$L%d\" -C \"$R\" --no-same-owner --no-same-permissions 2>/dev/null || tar -xf \"$L%d\" -C \"$R\"\n", i, i)
		} else {
			fmt.Fprintf(&b, "tar -xpf \"$L%d\" -C \"$R\"\n", i)
		}
	}
	b.WriteString("echo ROOTFS_OK\n")
	return b.String(), nil
}

// WritePlan stores a plan for the runtime host to pick up.
func WritePlan(dir string, p *Plan) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("plan-%d.json", os.Getpid()))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// FlattenWhiteouts lists tar entries of a layer in the order the applier needs
// them: whiteouts first, deepest entries first, so a directory entry never
// undoes a later file inside it. It is used to pre-sort small synthetic layers
// (Dockerfile steps) that the runtime expands.
func FlattenWhiteouts(names []string) []string {
	out := append([]string(nil), names...)
	sort.SliceStable(out, func(i, j int) bool {
		wi := strings.HasPrefix(filepath.Base(out[i]), ".wh.")
		wj := strings.HasPrefix(filepath.Base(out[j]), ".wh.")
		if wi != wj {
			return wi
		}
		return strings.Count(out[i], "/") > strings.Count(out[j], "/")
	})
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
