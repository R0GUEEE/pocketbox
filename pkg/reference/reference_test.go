package reference

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in       string
		registry string
		repo     string
		tag      string
		api      string
	}{
		{"alpine", "docker.io", "library/alpine", "latest", "registry-1.docker.io"},
		{"alpine:3.19", "docker.io", "library/alpine", "3.19", "registry-1.docker.io"},
		{"library/alpine", "docker.io", "library/alpine", "latest", "registry-1.docker.io"},
		{"grafana/grafana:10.2", "docker.io", "grafana/grafana", "10.2", "registry-1.docker.io"},
		{"ghcr.io/owner/app:v1", "ghcr.io", "owner/app", "v1", "ghcr.io"},
		{"localhost:5000/app", "localhost:5000", "app", "latest", "localhost:5000"},
		{"localhost/app", "localhost", "app", "latest", "localhost"},
		{"index.docker.io/library/busybox", "docker.io", "library/busybox", "latest", "registry-1.docker.io"},
		{"registry.example.com:443/a/b/c:tag-1.2_x", "registry.example.com:443", "a/b/c", "tag-1.2_x", "registry.example.com:443"},
	}
	for _, c := range cases {
		r, err := Parse(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if r.Registry != c.registry || r.Repository != c.repo || r.Tag != c.tag || r.APIHost() != c.api {
			t.Errorf("%s: got %q %q %q api=%q", c.in, r.Registry, r.Repository, r.Tag, r.APIHost())
		}
	}
}

func TestParseDigest(t *testing.T) {
	r, err := Parse("alpine@sha256:927f30a4e7f8f8e2b1d6f0cbb16e0b9b9f4a4a8a2c4b5d6e7f8091a2b3c4d5e6")
	if err != nil {
		t.Fatal(err)
	}
	if r.Tag != "" || r.ManifestRef() == "" {
		t.Fatalf("tag=%q manifestRef=%q", r.Tag, r.ManifestRef())
	}
	if _, err := Parse("alpine:bad tag"); err == nil {
		t.Fatal("expected tag error")
	}
	if _, err := Parse("Alpine/Thing"); err == nil {
		t.Fatal("expected repository error")
	}
	if _, err := Parse(""); err == nil {
		t.Fatal("expected empty error")
	}
}

func TestString(t *testing.T) {
	for in, want := range map[string]string{
		"alpine":                 "alpine:latest",
		"alpine:3.19":            "alpine:3.19",
		"grafana/grafana:10.2":   "grafana/grafana:10.2",
		"ghcr.io/owner/app:v1":   "ghcr.io/owner/app:v1",
		"library/busybox:latest": "busybox:latest",
	} {
		r, err := Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.String(); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}
