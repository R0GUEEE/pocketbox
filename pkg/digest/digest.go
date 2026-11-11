// Package digest implements the OCI content digests: algorithm identifier,
// hex value, parsing and verification against streamed content.
package digest

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
	"strings"
)

// Digest is "<algorithm>:<encoded>".
type Digest string

var (
	ErrInvalidFormat = errors.New("digest: invalid format")
	digestRegexp     = regexp.MustCompile(`^([a-z0-9]+(?:[.+_-][a-z0-9]+)*):([a-zA-Z0-9=_-]+)$`)
)

// Algorithm returns the part before the colon.
func (d Digest) Algorithm() string {
	i := strings.IndexByte(string(d), ':')
	if i < 0 {
		return ""
	}
	return string(d)[:i]
}

func (d Digest) Encoded() string {
	i := strings.IndexByte(string(d), ':')
	if i < 0 {
		return ""
	}
	return string(d)[i+1:]
}

func (d Digest) String() string { return string(d) }

// Parse validates the shape and the encoded length for known algorithms.
func Parse(s string) (Digest, error) {
	m := digestRegexp.FindStringSubmatch(s)
	if m == nil {
		return "", fmt.Errorf("%w: %q", ErrInvalidFormat, s)
	}
	alg, enc := m[1], m[2]
	want := 0
	switch alg {
	case "sha256":
		want = 64
	case "sha384":
		want = 96
	case "sha512":
		want = 128
	default:
		return "", fmt.Errorf("digest: unsupported algorithm %q", alg)
	}
	if len(enc) != want {
		return "", fmt.Errorf("%w: %q has %d characters, want %d", ErrInvalidFormat, s, len(enc), want)
	}
	if _, err := hex.DecodeString(enc); err != nil {
		return "", fmt.Errorf("%w: %q: %w", ErrInvalidFormat, s, err)
	}
	return Digest(s), nil
}

// New returns the digest of the given content for one of the supported
// algorithms.
func New(algorithm, encoded string) (Digest, error) {
	return Parse(algorithm + ":" + encoded)
}

// Hash returns a hash.Hash for the algorithm.
func (d Digest) Hash() (hash.Hash, error) {
	switch d.Algorithm() {
	case "sha256":
		return sha256.New(), nil
	case "sha384":
		return sha512.New384(), nil
	case "sha512":
		return sha512.New(), nil
	}
	return nil, fmt.Errorf("digest: unsupported algorithm %q", d.Algorithm())
}

// Verifier computes the digest of everything written to it.
type Verifier struct {
	d    Digest
	h    hash.Hash
	size int64
}

func NewVerifier(d Digest) (*Verifier, error) {
	h, err := d.Hash()
	if err != nil {
		return nil, err
	}
	return &Verifier{d: d, h: h}, nil
}

func (v *Verifier) Write(p []byte) (int, error) {
	n, err := v.h.Write(p)
	v.size += int64(n)
	return n, err
}

func (v *Verifier) Size() int64 { return v.size }

// Verified reports whether what was written matches the expected digest.
func (v *Verifier) Verified() error {
	got, _ := New(v.d.Algorithm(), hex.EncodeToString(v.h.Sum(nil)))
	if got != v.d {
		return fmt.Errorf("digest: expected %s, got %s", v.d, got)
	}
	return nil
}

// VerifyStream reads r to EOF and checks it against d.
func VerifyStream(d Digest, r io.Reader) error {
	v, err := NewVerifier(d)
	if err != nil {
		return err
	}
	if _, err := io.Copy(v, r); err != nil {
		return err
	}
	return v.Verified()
}

// FromReader is a convenience for computing a sha256 digest while consuming a
// stream. Callers that need the bytes should tee.
func FromReader(r io.Reader) (Digest, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	d, _ := New("sha256", hex.EncodeToString(h.Sum(nil)))
	return d, n, nil
}
