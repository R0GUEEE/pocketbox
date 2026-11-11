// Package oci defines the slice of the OCI image specification the engine
// needs: image manifests, image indexes, the config descriptor and the
// image configuration itself.
package oci

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pocketbox/pocketbox/pkg/digest"
)

const (
	MediaTypeImageIndex           = "application/vnd.oci.image.index.v1+json"
	MediaTypeImageManifest        = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeImageConfig          = "application/vnd.oci.image.config.v1+json"
	MediaTypeLayer                = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeLayerGzip            = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaTypeLayerZstd            = "application/vnd.oci.image.layer.v1.tar+zstd"
	MediaTypeLayerNonDistGzip     = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	MediaTypeLayerNonDistUncomp   = "application/vnd.docker.image.rootfs.diff.tar"
	MediaTypeManifestNonDist      = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeIndexNonDist         = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeConfigNonDist        = "application/vnd.docker.container.image.v1+json"
	AnnotationRefName             = "org.opencontainers.image.ref.name"
	AnnotationDockerContentDigest = "com.docker.content.digest"
	AnnotationBaseDigest          = "org.opencontainers.image.base.digest"
	AnnotationBaseName            = "org.opencontainers.image.base.name"
)

type Descriptor struct {
	MediaType   string            `json:"mediaType,omitempty"`
	Digest      digest.Digest     `json:"digest"`
	Size        int64             `json:"size"`
	URLs        []string          `json:"urls,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *Platform         `json:"platform,omitempty"`
}

type Platform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
}

type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Subject       *Descriptor       `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

type Index struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Manifests     []Descriptor      `json:"manifests"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// Config is the image configuration (application/vnd.oci.image.config.v1+json).
type Config struct {
	Architecture string            `json:"architecture"`
	OS           string            `json:"os,omitempty"`
	Variant      string            `json:"variant,omitempty"`
	Config       ConfigSection     `json:"config,omitempty"`
	RootFS       RootFS            `json:"rootfs"`
	History      []History         `json:"history,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Created      string            `json:"created,omitempty"`
}

type RootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

type History struct {
	CreatedBy  string `json:"created_by,omitempty"`
	Comment    string `json:"comment,omitempty"`
	EmptyLayer bool   `json:"empty_layer,omitempty"`
	Author     string `json:"author,omitempty"`
	Created    string `json:"created,omitempty"`
}

type ConfigSection struct {
	User         string              `json:"User,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Volumes      map[string]struct{} `json:"Volumes,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
	Shell        []string            `json:"Shell,omitempty"`
	OnBuild      []string            `json:"OnBuild,omitempty"`
}

func ParseDescriptor(raw []byte) (Descriptor, error) {
	var d Descriptor
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, fmt.Errorf("oci: descriptor: %w", err)
	}
	return d, nil
}

// IsIndex reports whether a manifest body is an index/manifest list.
func IsIndex(mediaType string, raw []byte) bool {
	if mediaType == MediaTypeImageIndex || mediaType == MediaTypeIndexNonDist {
		return true
	}
	var probe struct {
		MediaType string          `json:"mediaType"`
		Manifests []Descriptor    `json:"manifests"`
		Layers    json.RawMessage `json:"layers"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.MediaType == MediaTypeImageIndex || probe.MediaType == MediaTypeIndexNonDist ||
		(len(probe.Manifests) > 0 && len(probe.Layers) == 0)
}

// SelectPlatform picks the manifest matching want from an index. It returns nil
// when no entry matches.
func SelectPlatform(idx *Index, want Platform) *Descriptor {
	var fallback *Descriptor
	for i := range idx.Manifests {
		m := &idx.Manifests[i]
		if m.Platform == nil {
			continue
		}
		if m.Platform.OS != want.OS || m.Platform.Architecture != want.Architecture {
			continue
		}
		if want.Variant != "" && m.Platform.Variant != "" && m.Platform.Variant != want.Variant {
			continue
		}
		if want.Variant != "" && m.Platform.Variant == want.Variant {
			return m
		}
		if fallback == nil {
			fallback = m
		}
	}
	return fallback
}

// LayerCompression maps a layer media type to "gzip", "zstd", "none" or "" for
// unknown.
func LayerCompression(mediaType string) string {
	switch mediaType {
	case MediaTypeLayerGzip, MediaTypeLayerNonDistGzip:
		return "gzip"
	case MediaTypeLayerZstd:
		return "zstd"
	case MediaTypeLayer, MediaTypeLayerNonDistUncomp:
		return "none"
	}
	if strings.HasSuffix(mediaType, "+gzip") {
		return "gzip"
	}
	if strings.HasSuffix(mediaType, "+zstd") {
		return "zstd"
	}
	if strings.HasSuffix(mediaType, ".tar") {
		return "none"
	}
	return ""
}
