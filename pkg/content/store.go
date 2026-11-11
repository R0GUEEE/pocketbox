// Package content is the local content-addressable blob store used for image
// manifests, configs and layers.
package content

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pocketbox/pocketbox/pkg/digest"
)

// Store keeps blobs at <root>/<algorithm>/<first2>/<hex>.
type Store struct {
	Root string
	mu   sync.Mutex
}

func New(root string) *Store { return &Store{Root: root} }

func (s *Store) path(d digest.Digest) (string, error) {
	if _, err := digest.Parse(string(d)); err != nil {
		return "", err
	}
	enc := d.Encoded()
	return filepath.Join(s.Root, d.Algorithm(), enc[:2], enc), nil
}

// Path returns the file location of a blob (which may not exist).
func (s *Store) Path(d digest.Digest) (string, error) { return s.path(d) }

// Info describes a stored blob.
type Info struct {
	Digest digest.Digest
	Size   int64
	Path   string
}

// Get opens a blob for reading.
func (s *Store) Get(d digest.Digest) (io.ReadCloser, Info, error) {
	p, err := s.path(d)
	if err != nil {
		return nil, Info{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, Info{}, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, Info{}, err
	}
	return f, Info{Digest: d, Size: st.Size(), Path: p}, nil
}

// Stat returns information about a blob.
func (s *Store) Stat(d digest.Digest) (Info, error) {
	p, err := s.path(d)
	if err != nil {
		return Info{}, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return Info{}, err
	}
	return Info{Digest: d, Size: st.Size(), Path: p}, nil
}

// Has reports whether the blob is present.
func (s *Store) Has(d digest.Digest) bool {
	_, err := s.Stat(d)
	return err == nil
}

// ErrDigest reports a content mismatch.
var ErrDigest = errors.New("content: digest mismatch")

// Put stores a blob, verifying digest d as it streams. It returns the size.
// The data is written to a temporary file and renamed into place.
func (s *Store) Put(d digest.Digest, r io.Reader, size int64) (Info, error) {
	p, err := s.path(d)
	if err != nil {
		return Info{}, err
	}
	if st, err := os.Stat(p); err == nil && (size < 0 || st.Size() == size) {
		return Info{Digest: d, Size: st.Size(), Path: p}, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return Info{}, err
	}
	s.mu.Lock()
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	s.mu.Unlock()
	if err != nil {
		return Info{}, err
	}
	defer os.Remove(tmp.Name())

	v, err := digest.NewVerifier(d)
	if err != nil {
		tmp.Close()
		return Info{}, err
	}
	n, err := io.Copy(io.MultiWriter(tmp, v), r)
	if err != nil {
		tmp.Close()
		return Info{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Info{}, err
	}
	if err := tmp.Close(); err != nil {
		return Info{}, err
	}
	if err := v.Verified(); err != nil {
		return Info{}, ErrDigest
	}
	if size >= 0 && n != size {
		return Info{}, fmt.Errorf("content: %s: read %d bytes, wanted %d", d, n, size)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return Info{}, err
	}
	return Info{Digest: d, Size: n, Path: p}, nil
}

// PutBytes stores an in-memory blob and returns its digest and size.
func (s *Store) PutBytes(b []byte) (digest.Digest, Info, error) {
	sum := sha256.Sum256(b)
	d, _ := digest.New("sha256", hex.EncodeToString(sum[:]))
	info, err := s.Put(d, strings.NewReader(string(b)), int64(len(b)))
	return d, info, err
}

// Remove deletes a blob.
func (s *Store) Remove(d digest.Digest) error {
	p, err := s.path(d)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// List walks the store and reports every blob.
func (s *Store) List() ([]Info, error) {
	var out []Info
	err := filepath.WalkDir(s.Root, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		if strings.HasPrefix(e.Name(), ".tmp-") {
			return nil
		}
		rel, _ := filepath.Rel(s.Root, p)
		alg := filepath.Dir(filepath.Dir(rel))
		d, derr := digest.New(alg, e.Name())
		if derr != nil {
			return nil
		}
		st, serr := e.Info()
		if serr != nil {
			return nil
		}
		out = append(out, Info{Digest: d, Size: st.Size(), Path: p})
		return nil
	})
	return out, err
}
