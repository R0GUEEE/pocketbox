// Package images turns references into running-capable local images: it
// fetches manifests, configs and layers into the content store and applies
// layers to produce a coalesced root filesystem.
package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pocketbox/pocketbox/pkg/content"
	"github.com/pocketbox/pocketbox/pkg/digest"
	"github.com/pocketbox/pocketbox/pkg/dirs"
	"github.com/pocketbox/pocketbox/pkg/layers"
	"github.com/pocketbox/pocketbox/pkg/oci"
	"github.com/pocketbox/pocketbox/pkg/reference"
	"github.com/pocketbox/pocketbox/pkg/registry"
)

// Store owns the content store and the image rootfs cache.
type Store struct {
	Layout  dirs.Layout
	Content *content.Store

	mu    sync.Mutex
	creds map[string]registry.Credentials
	opts  registry.Options
	// Platform to request when a manifest list is served.
	Platform oci.Platform
	// Hooks for progress reporting.
	OnProgress func(Progress)
	// CredentialFor supplies registry credentials lazily by host.
	CredentialFor func(host string) (registry.Credentials, bool)
}

// Progress is a coarse download/apply event.
type Progress struct {
	Image   string
	Layer   int
	Layers  int
	Blob    digest.Digest
	Event   string // "manifest", "layer-start", "layer-done", "apply-start", "apply-done"
	Bytes   int64
	Total   int64
	Message string
}

// Image is a locally known image.
type Image struct {
	Reference    string            `json:"reference"`
	Name         string            `json:"name"`
	Digest       digest.Digest     `json:"digest"`
	Manifest     digest.Digest     `json:"manifestDigest"`
	ConfigDigest digest.Digest     `json:"configDigest"`
	Created      time.Time         `json:"created"`
	Platform     oci.Platform      `json:"platform"`
	Config       oci.Config        `json:"config"`
	Labels       map[string]string `json:"labels,omitempty"`
	RootFS       string            `json:"rootfs"`
	Size         int64             `json:"size"`
	// Layers are the layer digests in apply order.
	Layers []digest.Digest `json:"layers"`
	// ManifestLayers describe the layers for a manifest written by the builder.
	ManifestLayers []oci.Descriptor `json:"manifestLayers,omitempty"`
	// NewDiffIDs are the diff_ids the config lists for the layers this image
	// added itself (commits and builds), in order.
	NewDiffIDs []string `json:"newDiffIDs,omitempty"`
}

func New(l dirs.Layout) *Store {
	s := &Store{
		Layout:  l,
		Content: content.New(l.Content()),
		creds:   map[string]registry.Credentials{},
		Platform: oci.Platform{
			OS:           "linux",
			Architecture: runtime.GOARCH,
		},
	}
	if runtime.GOARCH == "arm64" {
		s.Platform.Variant = "v8"
	}
	return s
}

// SetCredentials registers credentials for a registry host.
func (s *Store) SetCredentials(host string, c registry.Credentials) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds[host] = c
}

// SetOptions controls transport behaviour (insecure registries, timeouts).
func (s *Store) SetOptions(o registry.Options) { s.opts = o }

func (s *Store) client(host string) *registry.Client {
	s.mu.Lock()
	c, ok := s.creds[host]
	getter := s.CredentialFor
	s.mu.Unlock()
	if !ok && getter != nil {
		if cred, found := getter(host); found {
			c = cred
		}
	}
	return registry.New(host, c, s.opts)
}

func (s *Store) progress(p Progress) {
	if s.OnProgress != nil {
		s.OnProgress(p)
	}
}

// ManifestBundle is a resolved image manifest with its config.
type ManifestBundle struct {
	Ref          reference.Reference
	Manifest     oci.Manifest
	ManifestDig  digest.Digest
	ManifestRaw  []byte
	MediaType    string
	ConfigRaw    []byte
	Config       oci.Config
	ConfigDigest digest.Digest
}

// Resolve fetches the manifest (resolving indexes for the engine platform) and
// the image config.
func (s *Store) Resolve(ctx context.Context, ref reference.Reference) (*ManifestBundle, error) {
	r := ref
	cl := s.client(r.APIHost())
	raw, mt, d, err := cl.GetManifest(ctx, r, registry.AcceptManifests)
	if err != nil {
		return nil, err
	}
	if oci.IsIndex(mt, raw) {
		var idx oci.Index
		if err := json.Unmarshal(raw, &idx); err != nil {
			return nil, fmt.Errorf("images: index: %w", err)
		}
		desc := oci.SelectPlatform(&idx, s.Platform)
		if desc == nil {
			return nil, fmt.Errorf("images: %s has no manifest for %s/%s", ref.String(), s.Platform.OS, s.Platform.Architecture)
		}
		sub := r
		sub.Digest = desc.Digest
		sub.Tag = ""
		raw, mt, d, err = cl.GetManifest(ctx, sub, registry.AcceptManifests)
		if err != nil {
			return nil, err
		}
	}
	var m oci.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("images: manifest: %w", err)
	}
	if m.Config.Digest == "" {
		return nil, errors.New("images: manifest has no config")
	}
	s.progress(Progress{Image: ref.String(), Event: "manifest", Blob: d})

	cfgRaw, err := s.fetchBlob(ctx, cl, r.Repository, m.Config.Digest, m.Config.Size, "")
	if err != nil {
		return nil, fmt.Errorf("images: config: %w", err)
	}
	var cfg oci.Config
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		return nil, fmt.Errorf("images: config json: %w", err)
	}
	return &ManifestBundle{
		Ref: r, Manifest: m, ManifestDig: d, ManifestRaw: raw, MediaType: mt,
		ConfigRaw: cfgRaw, Config: cfg, ConfigDigest: m.Config.Digest,
	}, nil
}

// fetchBlob reads a blob from the content store, downloading it first if needed.
// The returned bytes are only materialised when intoMemory is true; callers
// that want the file should use fetchBlobFile.
func (s *Store) fetchBlob(ctx context.Context, cl *registry.Client, repo string, d digest.Digest, size int64, _ string) ([]byte, error) {
	if s.Content.Has(d) {
		rc, info, err := s.Content.Get(d)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		if info.Size > 8<<20 {
			return nil, fmt.Errorf("images: blob %s too large to buffer", d)
		}
		return io.ReadAll(rc)
	}
	body, _, err := cl.Blob(ctx, repo, d)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	info, err := s.Content.Put(d, body, size)
	if err != nil {
		return nil, err
	}
	rc, _, err := s.Content.Get(info.Digest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Pull downloads every blob of an image and applies its layers to a fresh
// rootfs directory.
func (s *Store) Pull(ctx context.Context, refStr string, progress func(Progress)) (*Image, error) {
	ref, err := reference.Parse(refStr)
	if err != nil {
		return nil, err
	}
	if progress != nil {
		s.OnProgress = progress
	}
	b, err := s.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	cl := s.client(ref.APIHost())
	if len(b.Config.RootFS.DiffIDs) != len(b.Manifest.Layers) {
		return nil, fmt.Errorf("images: %d diff_ids but %d layers", len(b.Config.RootFS.DiffIDs), len(b.Manifest.Layers))
	}

	var total int64
	for _, l := range b.Manifest.Layers {
		total += l.Size
	}
	var got int64
	for i, l := range b.Manifest.Layers {
		if s.Content.Has(l.Digest) {
			st, _ := s.Content.Stat(l.Digest)
			got += st.Size
			continue
		}
		s.progress(Progress{Image: ref.String(), Layer: i, Layers: len(b.Manifest.Layers), Blob: l.Digest, Event: "layer-start", Bytes: got, Total: total})
		body, size, err := cl.Blob(ctx, ref.Repository, l.Digest)
		if err != nil {
			return nil, fmt.Errorf("images: layer %s: %w", l.Digest, err)
		}
		info, err := s.Content.Put(l.Digest, body, size)
		body.Close()
		if err != nil {
			return nil, fmt.Errorf("images: layer %s: %w", l.Digest, err)
		}
		got += info.Size
		s.progress(Progress{Image: ref.String(), Layer: i, Layers: len(b.Manifest.Layers), Blob: l.Digest, Event: "layer-done", Bytes: got, Total: total})
	}

	// Apply layers newest-last into a staging directory, then move it into
	// place. A failed pull never leaves a half-built rootfs at the image path.
	imgDigest := b.ManifestDig
	short := strings.ReplaceAll(string(imgDigest.Encoded()), "/", "_")
	final := filepath.Join(s.Layout.Images(), short[:16])
	staging := final + ".staging"
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return nil, err
	}
	applyOpts := layers.Options{Root: staging, PreserveUIDs: os.Geteuid() == 0, SkipOwnershipErrors: true}
	for i, l := range b.Manifest.Layers {
		s.progress(Progress{Image: ref.String(), Layer: i, Layers: len(b.Manifest.Layers), Event: "apply-start"})
		rc, _, err := s.Content.Get(l.Digest)
		if err != nil {
			os.RemoveAll(staging)
			return nil, err
		}
		dec, closeFn, err := layers.OpenStream(rc, oci.LayerCompression(l.MediaType))
		if err != nil {
			rc.Close()
			os.RemoveAll(staging)
			return nil, fmt.Errorf("images: layer %d: %w", i, err)
		}
		_, aerr := layers.Apply(dec, applyOpts)
		closeFn()
		rc.Close()
		if aerr != nil {
			os.RemoveAll(staging)
			return nil, fmt.Errorf("images: apply layer %d: %w", i, aerr)
		}
		s.progress(Progress{Image: ref.String(), Layer: i, Layers: len(b.Manifest.Layers), Event: "apply-done"})
	}
	_ = os.RemoveAll(final)
	if err := os.Rename(staging, final); err != nil {
		return nil, err
	}

	layerDigests := make([]digest.Digest, 0, len(b.Manifest.Layers))
	for _, l := range b.Manifest.Layers {
		layerDigests = append(layerDigests, l.Digest)
	}
	img := &Image{
		Reference:    ref.String(),
		Name:         ref.String(),
		Digest:       b.ManifestDig,
		Manifest:     b.ManifestDig,
		ConfigDigest: b.ConfigDigest,
		Created:      time.Now().UTC(),
		Platform:     s.Platform,
		Config:       b.Config,
		Labels:       b.Config.Config.Labels,
		RootFS:       final,
		Size:         got,
		Layers:       layerDigests,
	}
	if err := s.saveMeta(img); err != nil {
		return nil, err
	}
	return img, nil
}

func (s *Store) metaPath(d digest.Digest) string {
	return filepath.Join(s.Layout.Images(), strings.ReplaceAll(d.Encoded(), "/", "_")+".json")
}

// Save writes (or rewrites) an image record.
func (s *Store) Save(img *Image) error { return s.saveMeta(img) }

func (s *Store) saveMeta(img *Image) error {
	b, err := json.MarshalIndent(img, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.metaPath(img.Digest), b, 0o644)
}

// List returns every locally pulled image, newest first.
func (s *Store) List() ([]*Image, error) {
	entries, err := os.ReadDir(s.Layout.Images())
	if err != nil {
		return nil, err
	}
	var out []*Image
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.Layout.Images(), e.Name()))
		if err != nil {
			continue
		}
		var img Image
		if json.Unmarshal(raw, &img) == nil {
			out = append(out, &img)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Get finds a local image by reference or digest (full or unique prefix).
func (s *Store) Get(name string) (*Image, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	want := name
	if ref, err := reference.Parse(name); err == nil {
		want = ref.String()
	}
	var prefix *Image
	for _, img := range all {
		if img.Reference == want || string(img.Digest) == name {
			return img, nil
		}
		if strings.HasPrefix(string(img.Digest), name) {
			return img, nil
		}
		if strings.HasPrefix(string(img.Digest.Encoded()), name) && prefix == nil {
			prefix = img
		}
	}
	if prefix != nil {
		return prefix, nil
	}
	return nil, fmt.Errorf("images: no such image %q (try pulling it first)", name)
}

// Remove deletes an image's metadata and rootfs.
func (s *Store) Remove(img *Image) error {
	_ = os.Remove(s.metaPath(img.Digest))
	if img.RootFS != "" && strings.HasPrefix(img.RootFS, s.Layout.Images()) {
		return os.RemoveAll(img.RootFS)
	}
	return nil
}
