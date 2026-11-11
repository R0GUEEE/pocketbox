// Package layers applies container image layers to a root filesystem.
//
// The applier works on a real directory tree (the coalesced rootfs of an
// image). It follows the OCI "apply changeset" rules: whiteouts, opaque
// directories, hard links, device nodes, uid/gid, modes, and symlinks. Callers
// that need ordering guarantees (whiteouts before payload, descendants before
// parents) can list entries first and reorder them.
package layers

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const (
	whiteoutPrefix = ".wh."
	opaqueMarker   = ".wh..wh..opq"
)

// Opener returns a reader for one layer blob (compressed or not).
type Opener func() (io.ReadCloser, error)

// Options control one apply pass.
type Options struct {
	// Root is the target directory; it must exist.
	Root string
	// PreserveUIDs sets ownership from the archive. Inside the iOS app the
	// process is unprivileged, so this is usually false there and true on a
	// Linux host.
	PreserveUIDs bool
	// AllowDeviceNodes creates device and fifo nodes in Root. Inside a
	// sandbox this is always off; the guest runtime synthesises /dev.
	AllowDeviceNodes bool
	// OnEntry, when set, is called for each archive entry before it is
	// applied (used for progress and debugging).
	OnEntry func(name string)
	// SkipOwnershipErrors keeps going when chown is refused (EPERM), which is
	// what an unprivileged iOS process gets from every chown.
	SkipOwnershipErrors bool
}

// Applied describes one layer application.
type Applied struct {
	Entries int
	Bytes   int64
}

func secureJoin(root, name string) (string, error) {
	clean := path.Clean("/" + strings.TrimPrefix(name, "/"))
	rel := strings.TrimPrefix(clean, "/")
	if rel == "" || rel == "." {
		return root, nil
	}
	if strings.Contains(rel, "..") {
		return "", fmt.Errorf("layers: path escapes root: %q", name)
	}
	return filepath.Join(root, filepath.FromSlash(rel)), nil
}

// Apply extracts one layer archive stream into opts.Root.
func Apply(r io.Reader, opts Options) (Applied, error) {
	var out Applied
	if opts.Root == "" {
		return out, errors.New("layers: empty root")
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("layers: read tar: %w", err)
		}
		if opts.OnEntry != nil {
			opts.OnEntry(hdr.Name)
		}
		if err := applyEntry(tr, hdr, opts, &out); err != nil {
			return out, fmt.Errorf("layers: %s: %w", hdr.Name, err)
		}
	}
}

// removeTree deletes whatever sits at dst (file, link or directory).
func removeTree(dst string) error {
	fi, err := os.Lstat(dst)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.IsDir() {
		// Make sure the tree is writable/removable: layers often ship 0555 dirs.
		_ = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
		return os.RemoveAll(dst)
	}
	return os.Remove(dst)
}

func ensureDir(dst string, hdr *tar.Header, opts Options) error {
	if fi, err := os.Lstat(dst); err == nil && !fi.IsDir() {
		if err := removeTree(dst); err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	_ = os.Chmod(dst, os.FileMode(hdr.Mode).Perm()|uX(hdr.Mode))
	if err := chown(dst, hdr, opts); err != nil {
		return err
	}
	setTimes(dst, hdr)
	return nil
}

// uX keeps owner-exec on directories so we can descend while applying.
func uX(mode int64) os.FileMode {
	if mode&0o100 == 0 {
		return 0
	}
	return 0o700
}

func chown(dst string, hdr *tar.Header, opts Options) error {
	if !opts.PreserveUIDs {
		return nil
	}
	err := os.Lchown(dst, hdr.Uid, hdr.Gid)
	if err != nil && opts.SkipOwnershipErrors && errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}

func setTimes(dst string, hdr *tar.Header) {
	if hdr.ModTime.IsZero() {
		return
	}
	_ = os.Chtimes(dst, hdr.AccessTime, hdr.ModTime)
}

func applyEntry(tr *tar.Reader, hdr *tar.Header, opts Options, out *Applied) error {
	base := path.Base(hdr.Name)
	dir := path.Dir(hdr.Name)

	// Whiteout handling first: it removes what the payload would otherwise
	// resurrect.
	if strings.HasPrefix(base, whiteoutPrefix) {
		target, err := secureJoin(opts.Root, path.Join(dir, strings.TrimPrefix(base, whiteoutPrefix)))
		if err != nil {
			return err
		}
		if base == opaqueMarker {
			return makeOpaque(target, opts)
		}
		return removeTree(target)
	}

	dst, err := secureJoin(opts.Root, hdr.Name)
	if err != nil {
		return err
	}
	out.Entries++
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		return ensureDir(dst, hdr, opts)

	case tar.TypeReg:
		if fi, err := os.Lstat(dst); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(dst); err != nil {
				return err
			}
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode).Perm())
		if err != nil {
			return err
		}
		n, cerr := io.Copy(f, tr)
		if serr := f.Sync(); serr != nil && cerr == nil {
			cerr = serr
		}
		if cerr == nil {
			cerr = f.Close()
		} else {
			f.Close()
		}
		out.Bytes += n
		if cerr != nil {
			return cerr
		}
		if err := os.Chmod(dst, os.FileMode(hdr.Mode).Perm()); err != nil {
			return err
		}
		if err := chown(dst, hdr, opts); err != nil {
			return err
		}
		setTimes(dst, hdr)
		return nil

	case tar.TypeSymlink:
		if err := removeTree(dst); err != nil {
			return err
		}
		if err := os.Symlink(hdr.Linkname, dst); err != nil {
			return err
		}
		if err := chown(dst, hdr, opts); err != nil {
			return err
		}
		return nil

	case tar.TypeLink:
		src, err := secureJoin(opts.Root, hdr.Linkname)
		if err != nil {
			return err
		}
		if err := removeTree(dst); err != nil {
			return err
		}
		if err := os.Link(src, dst); err != nil {
			// Cross-device or source missing: fall back to a copy so the
			// image still applies.
			return copyFile(src, dst, hdr, opts)
		}
		return nil

	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		if !opts.AllowDeviceNodes {
			// The guest runtime provides /dev; a plain file keeps the path
			// occupied and the size metadata honest.
			_ = removeTree(dst)
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, os.FileMode(hdr.Mode).Perm())
			if err == nil {
				return f.Close()
			}
			return nil
		}
		if err := removeTree(dst); err != nil {
			return err
		}
		mode := uint32(hdr.Mode)
		switch hdr.Typeflag {
		case tar.TypeChar:
			mode |= syscall.S_IFCHR
		case tar.TypeBlock:
			mode |= syscall.S_IFBLK
		case tar.TypeFifo:
			mode |= syscall.S_IFIFO
		}
		dev := int(hdr.Devmajor<<8 | hdr.Devminor)
		if err := syscall.Mknod(dst, mode, dev); err != nil {
			return err
		}
		return chown(dst, hdr, opts)

	case tar.TypeXGlobalHeader, tar.TypeXHeader:
		return nil
	}
	// Unknown entry types (GNU sparse metadata, sockets) are ignored the way
	// containerd does.
	return nil
}

// makeOpaque removes the contents of a directory without removing it.
func makeOpaque(dst string, opts Options) error {
	entries, err := os.ReadDir(dst)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := removeTree(filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, hdr *tar.Header, opts Options) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode).Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// OpenStream returns a decompressed reader for a layer blob.
func OpenStream(r io.Reader, compression string) (io.Reader, func() error, error) {
	switch compression {
	case "gzip":
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return gz, gz.Close, nil
	case "none", "":
		return r, func() error { return nil }, nil
	case "zstd":
		return nil, nil, errors.New("layers: zstd layers are not supported by this build")
	}
	return nil, nil, fmt.Errorf("layers: unknown compression %q", compression)
}

// SortedNames lists a tar stream's entry names; used by tests and by callers
// that need to reorder the pass (children before parents).
func SortedNames(r io.Reader) ([]string, error) {
	tr := tar.NewReader(r)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		names = append(names, hdr.Name)
	}
	sort.SliceStable(names, func(i, j int) bool {
		return strings.Count(names[i], "/") > strings.Count(names[j], "/")
	})
	return names, nil
}
