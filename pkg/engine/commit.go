package engine

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pocketbox/pocketbox/pkg/buildkitclient"
	"github.com/pocketbox/pocketbox/pkg/images"
	"github.com/pocketbox/pocketbox/pkg/oci"
)

// Commit turns a stopped container's filesystem into a new image tagged `tag`.
//
// The layer is the difference between the image the container was created from
// and its rootfs now. Diffing whole trees keeps the model honest across runners:
// whatever the container changed is what the new layer contains, and the size
// of the diff is bounded by the image, not by an append-only log.
func (e *Engine) Commit(id string, tag string) (*images.Image, error) {
	c, err := e.Get(id)
	if err != nil {
		return nil, err
	}
	if c.State == StateRunning {
		return nil, fmt.Errorf("commit: container %s is running", c.Name)
	}
	base, err := e.Images.Get(c.Image)
	if err != nil {
		return nil, fmt.Errorf("commit: base image: %w", err)
	}
	tarBytes, err := DiffTrees(base.RootFS, c.RootFS)
	if err != nil {
		return nil, err
	}
	diffID := buildkitclient.DiffID(tarBytes)
	gz, err := buildkitclient.Gzip(tarBytes)
	if err != nil {
		return nil, err
	}
	layerDigest, layerInfo, err := e.Images.Content.PutBytes(gz)
	if err != nil {
		return nil, err
	}
	if tag == "" {
		tag = fmt.Sprintf("%s:commit-%d", strings.TrimSuffix(c.Image, ":latest"), time.Now().Unix())
	}
	cfg := base.Config
	cfg.RootFS.DiffIDs = append(append([]string{}, cfg.RootFS.DiffIDs...), diffID)
	cfg.History = append(append([]oci.History{}, cfg.History...), oci.History{
		CreatedBy: "commit " + c.Name,
		Created:   time.Now().UTC().Format(time.RFC3339),
	})
	img, err := e.storeImageLayers(tag, &cfg, []oci.Descriptor{{
		MediaType: oci.MediaTypeLayerGzip, Digest: layerDigest, Size: layerInfo.Size,
	}}, c.RootFS)
	if err != nil {
		return nil, err
	}
	if err := copyTree(c.RootFS, base.RootFS); err != nil {
		return nil, fmt.Errorf("commit: update image rootfs: %w", err)
	}
	_ = os.RemoveAll(c.RootFS)
	return img, nil
}

// storeImageLayers is storeImage plus explicit layer descriptors, used by
// commits where only the newest layer is described.
func (e *Engine) storeImageLayers(tag string, cfg *oci.Config, newLayers []oci.Descriptor, rootfs string) (*images.Image, error) {
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
	for _, l := range newLayers {
		img.ManifestLayers = append(img.ManifestLayers, l)
		img.Layers = append(img.Layers, l.Digest)
		img.NewDiffIDs = append(img.NewDiffIDs, cfg.RootFS.DiffIDs[len(cfg.RootFS.DiffIDs)-1])
	}
	return img, e.Images.Save(img)
}

// DiffTrees writes a layer containing everything that differs between old and
// new, including whiteouts for removed paths.
func DiffTrees(oldRoot, newRoot string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	oldFiles := map[string]os.FileInfo{}
	if oldRoot != "" {
		err := filepath.WalkDir(oldRoot, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(oldRoot, p)
			if rel == "." {
				return nil
			}
			info, ierr := d.Info()
			if ierr == nil {
				oldFiles[filepath.ToSlash(rel)] = info
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	newSet := map[string]bool{}
	err := filepath.WalkDir(newRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(newRoot, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		newSet[rel] = true
		info, err := d.Info()
		if err != nil {
			return err
		}
		if old, ok := oldFiles[rel]; ok {
			if sameEntry(old, info) {
				return nil
			}
		}
		switch {
		case info.IsDir():
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Typeflag: tar.TypeDir,
				Mode: int64(info.Mode().Perm()), ModTime: epoch})
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeSymlink,
				Linkname: link, Mode: 0o777, ModTime: epoch})
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg,
				Mode: int64(info.Mode().Perm()), Size: int64(len(data)), ModTime: epoch}); err != nil {
				return err
			}
			_, err = tw.Write(data)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Whiteouts for paths that disappeared; deepest first.
	var removed []string
	for rel := range oldFiles {
		if !newSet[rel] {
			removed = append(removed, rel)
		}
	}
	for i := 1; i < len(removed); i++ {
		for j := i; j > 0 && strings.Count(removed[j], "/") > strings.Count(removed[j-1], "/"); j-- {
			removed[j], removed[j-1] = removed[j-1], removed[j]
		}
	}
	for _, rel := range removed {
		dir, base := filepath.Split(rel)
		name := filepath.ToSlash(filepath.Join(dir, ".wh."+base))
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0, ModTime: epoch}); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var epoch = time.Unix(0, 0).UTC()

func sameEntry(a, b os.FileInfo) bool {
	if a.Mode() != b.Mode() || a.Size() != b.Size() {
		return false
	}
	return a.ModTime().Equal(b.ModTime())
}
