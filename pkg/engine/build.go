package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pocketbox/pocketbox/pkg/buildkitclient"
	"github.com/pocketbox/pocketbox/pkg/images"
	"github.com/pocketbox/pocketbox/pkg/layers"
	"github.com/pocketbox/pocketbox/pkg/oci"
)

// BuildOptions configures one Dockerfile build.
type BuildOptions struct {
	// Tag is the reference stored for the result, e.g. "myapp:latest".
	Tag string
	// Dockerfile is the Dockerfile text.
	Dockerfile string
	// ContextDir is an extracted build context on disk.
	ContextDir string
	// NoCache rebuilds every instruction.
	NoCache bool
	// Progress reports per-step status.
	Progress func(step int, total int, msg string)
}

// Build executes a Dockerfile and stores the result as a local image.
//
// Metadata instructions (ENV, WORKDIR, COPY, EXPOSE, …) are turned into layers
// here; RUN steps execute as real containers against the layer set built so
// far, and each successful run is committed as the next layer.
func (e *Engine) Build(ctx context.Context, opts BuildOptions) (*images.Image, error) {
	if e.Runner == nil {
		return nil, fmt.Errorf("build: no runner configured")
	}
	if strings.TrimSpace(opts.Tag) == "" {
		return nil, fmt.Errorf("build: a tag is required")
	}
	f, err := buildkitclient.Parse(opts.Dockerfile)
	if err != nil {
		return nil, err
	}
	if f.Base == "" {
		return nil, fmt.Errorf("build: no FROM instruction")
	}
	if f.Base == "scratch" {
		return nil, fmt.Errorf("build: FROM scratch is not supported yet; start from a base image")
	}
	ctxDir := opts.ContextDir
	if ctxDir == "" {
		ctxDir = e.Layout.Temp()
	}
	bctx := &buildkitclient.BuildContext{Dir: ctxDir}
	if raw, err := os.ReadFile(filepath.Join(ctxDir, ".dockerignore")); err == nil {
		bctx.Ignore = strings.Split(string(raw), "\n")
	}
	steps, section, err := buildkitclient.Lower(f, bctx)
	if err != nil {
		return nil, err
	}

	base, err := e.Images.Get(f.Base)
	if err != nil {
		e.notify(opts.Progress, 0, len(steps), "pulling base "+f.Base)
		base, err = e.Images.Pull(ctx, f.Base, nil)
		if err != nil {
			return nil, fmt.Errorf("build: base image %s: %w", f.Base, err)
		}
	}

	// Current image the next step builds on.
	current := base
	history := append([]oci.History{}, base.Config.History...)
	work := filepath.Join(e.Layout.Temp(), "build-"+fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := copyTree(base.RootFS, work); err != nil {
		return nil, fmt.Errorf("build: staging rootfs: %w", err)
	}
	defer os.RemoveAll(work)

	var layersOut []oci.Descriptor
	var diffIDs []string
	stepNo := 0
	for _, step := range steps {
		stepNo++
		if step.Instr.Cmd == "FROM" || step.Instr.Cmd == "ARG" {
			continue
		}
		e.notify(opts.Progress, stepNo, len(steps), strings.ToLower(step.Instr.Cmd)+" "+truncate(step.Instr.Args, 60))
		switch step.Kind {
		case "copy", "layer":
			tarBytes, err := step.Layer.Tar()
			if err != nil {
				return nil, err
			}
			diffID, desc, err := e.storeLayer(tarBytes)
			if err != nil {
				return nil, err
			}
			diffIDs = append(diffIDs, diffID)
			layersOut = append(layersOut, desc)
			history = append(history, oci.History{
				CreatedBy: "/bin/sh -c #(nop) " + step.Instr.Raw,
				Created:   time.Now().UTC().Format(time.RFC3339)})
			if _, err := layers.Apply(bytes.NewReader(tarBytes), layers.Options{Root: work}); err != nil {
				return nil, fmt.Errorf("build: apply %s: %w", step.Instr.Cmd, err)
			}
		case "run":
			img, err := e.runStep(ctx, current, work, step)
			if err != nil {
				return nil, err
			}
			current = img
			layersOut = append(layersOut, img.ManifestLayers...)
			diffIDs = append(diffIDs, img.NewDiffIDs...)
			history = append(history, oci.History{
				CreatedBy: "RUN " + step.Instr.Args,
				Created:   time.Now().UTC().Format(time.RFC3339)})
		}
		buildkitclient.ApplyChange(section, step.Config)
	}
	_ = layersOut

	cfg := oci.Config{
		Architecture: base.Config.Architecture,
		OS:           "linux",
		Config:       *section,
		RootFS: oci.RootFS{
			Type:    "layers",
			DiffIDs: append(append([]string{}, base.Config.RootFS.DiffIDs...), diffIDs...),
		},
		Created: time.Now().UTC().Format(time.RFC3339),
		History: history,
	}
	return e.storeImage(opts.Tag, &cfg, current.RootFS)
}

// runStep runs one RUN instruction on top of `on` and commits the result.
func (e *Engine) runStep(ctx context.Context, on *images.Image, work string, step buildkitclient.Step) (*images.Image, error) {
	c, err := e.Create(CreateOptions{
		Image:      on.Reference,
		Env:        envSlice(step.Env),
		Entrypoint: step.RunArgs,
		WorkingDir: step.WorkingDir,
		User:       step.User,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = e.Remove(c.ID, true) }()
	if err := e.Start(c.ID); err != nil {
		return nil, fmt.Errorf("build: RUN %q: %w", step.Instr.Args, err)
	}
	code, err := e.Wait(c.ID)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("build: RUN %q exited with code %d", step.Instr.Args, code)
	}
	img, err := e.Commit(c.ID, "")
	if err != nil {
		return nil, err
	}
	// The build already tracks the working tree; the committed rootfs copy is
	// only used until the build finishes.
	img.RootFS = work
	return img, nil
}

func (e *Engine) notify(p func(step, total int, msg string), step, total int, msg string) {
	if p != nil {
		p(step, total, msg)
	}
}

// storeLayer writes an uncompressed tar layer into the content store.
func (e *Engine) storeLayer(tarBytes []byte) (string, oci.Descriptor, error) {
	diffID := buildkitclient.DiffID(tarBytes)
	gz, err := buildkitclient.Gzip(tarBytes)
	if err != nil {
		return "", oci.Descriptor{}, err
	}
	d, info, err := e.Images.Content.PutBytes(gz)
	if err != nil {
		return "", oci.Descriptor{}, err
	}
	return diffID, oci.Descriptor{MediaType: oci.MediaTypeLayerGzip, Digest: d, Size: info.Size}, nil
}

// storeImage writes config + manifest for the built image and registers it.
func (e *Engine) storeImage(tag string, cfg *oci.Config, rootfs string) (*images.Image, error) {
	cfgRaw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	cfgDigest, cfgInfo, err := e.Images.Content.PutBytes(cfgRaw)
	if err != nil {
		return nil, err
	}
	img := &images.Image{
		Reference:    tag,
		Name:         tag,
		ConfigDigest: cfgDigest,
		Created:      time.Now().UTC(),
		Platform:     e.Images.Platform,
		Config:       *cfg,
		Labels:       cfg.Config.Labels,
		RootFS:       rootfs,
		Size:         cfgInfo.Size,
	}
	if err := e.Images.Save(img); err != nil {
		return nil, err
	}
	return img, nil
}

func envSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, kv := range sortedPairs(m) {
		out = append(out, kv)
	}
	return out
}

func sortedPairs(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + "=" + m[k]
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// copyTree copies a directory tree, following symlinks inside the tree.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
		return nil
	})
}
