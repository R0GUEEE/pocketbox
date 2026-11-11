// Package reference parses image references: registry/repository:tag@digest.
package reference

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pocketbox/pocketbox/pkg/digest"
)

const (
	DefaultRegistry = "docker.io"
	// DockerHub's API host differs from the name used in references.
	DockerHubAPI = "registry-1.docker.io"
	LegacyDomain = "index.docker.io"
	DefaultTag   = "latest"
)

// Reference is a parsed image name.
type Reference struct {
	// Registry is the registry host, without scheme. Docker Hub references
	// without an explicit host normalise to "docker.io".
	Registry string
	// Repository is the path below the registry, e.g. "library/alpine".
	Repository string
	Tag        string
	Digest     digest.Digest
}

var (
	domainRe = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*|\[[0-9a-fA-F:]+\])(?::[0-9]+)?$`)
	pathRe   = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	tagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// Parse parses s. Short names are normalised the way the Docker CLI does:
// "alpine" is "docker.io/library/alpine:latest".
func Parse(s string) (Reference, error) {
	var r Reference
	if s == "" {
		return r, fmt.Errorf("reference: empty")
	}
	rest := s
	if i := strings.Index(rest, "@"); i >= 0 {
		d, err := digest.Parse(rest[i+1:])
		if err != nil {
			return r, err
		}
		r.Digest = d
		rest = rest[:i]
	}
	// A tag separator is the last colon after the last slash.
	slash := strings.LastIndex(rest, "/")
	if i := strings.LastIndex(rest, ":"); i > slash {
		r.Tag = rest[i+1:]
		if !tagRe.MatchString(r.Tag) {
			return r, fmt.Errorf("reference: invalid tag %q", r.Tag)
		}
		rest = rest[:i]
	}
	if rest == "" {
		return r, fmt.Errorf("reference: missing repository")
	}

	first, remainder, hasSlash := strings.Cut(rest, "/")
	switch {
	case hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost"):
		if !domainRe.MatchString(first) {
			return r, fmt.Errorf("reference: invalid registry %q", first)
		}
		r.Registry = first
		r.Repository = remainder
	case hasSlash && strings.Count(rest, "/") > 0 && strings.ToLower(first) != first:
		// Uppercase path components are only valid in a domain.
		return r, fmt.Errorf("reference: invalid registry %q", first)
	default:
		r.Registry = DefaultRegistry
		r.Repository = rest
	}
	if r.Registry == LegacyDomain {
		r.Registry = DefaultRegistry
	}
	if strings.EqualFold(r.Registry, DefaultRegistry) && !strings.Contains(r.Repository, "/") {
		r.Repository = "library/" + r.Repository
	}
	if !pathRe.MatchString(strings.ToLower(r.Repository)) || r.Repository != strings.ToLower(r.Repository) {
		return r, fmt.Errorf("reference: invalid repository %q", r.Repository)
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = DefaultTag
	}
	return r, nil
}

// APIHost is the host the registry API is served from.
func (r Reference) APIHost() string {
	if strings.EqualFold(r.Registry, DefaultRegistry) {
		return DockerHubAPI
	}
	return r.Registry
}

// IsDockerHub reports whether this is a Docker Hub reference.
func (r Reference) IsDockerHub() bool {
	return strings.EqualFold(r.Registry, DefaultRegistry) || r.Registry == LegacyDomain
}

// Name is the canonical "registry/repository" form.
func (r Reference) Name() string { return r.Registry + "/" + r.Repository }

// String renders the reference back in short form: "alpine:3.19" for Docker
// Hub official images, otherwise registry/repository[:tag][@digest].
func (r Reference) String() string {
	var b strings.Builder
	if r.IsDockerHub() {
		if !strings.HasPrefix(r.Repository, "library/") {
			b.WriteString(r.Repository)
		} else {
			b.WriteString(strings.TrimPrefix(r.Repository, "library/"))
		}
	} else {
		b.WriteString(r.Name())
	}
	if r.Tag != "" {
		b.WriteString(":")
		b.WriteString(r.Tag)
	}
	if r.Digest != "" {
		b.WriteString("@")
		b.WriteString(string(r.Digest))
	}
	return b.String()
}

// ManifestRef is what goes into the GET request path.
func (r Reference) ManifestRef() string {
	if r.Digest != "" {
		return string(r.Digest)
	}
	return r.Tag
}

// Familiar strips the implicit Docker Hub prefix for display.
func (r Reference) Familiar() string { return r.String() }
