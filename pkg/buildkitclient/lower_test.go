package buildkitclient

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLowerAndCopy(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app/static"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app/index.html"), []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app/static/a.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app/secret.env"), []byte("X=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("*.env\napp/static/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := &BuildContext{Dir: dir, Ignore: []string{"*.env", "app/static/"}}

	f, err := Parse(`
FROM alpine:3.20
ENV APP_HOME=/srv/www
WORKDIR $APP_HOME
COPY app/ .
RUN echo ready
EXPOSE 80
ENTRYPOINT ["httpd", "-f"]
`)
	if err != nil {
		t.Fatal(err)
	}
	if f.Base != "alpine:3.20" {
		t.Fatalf("base %q", f.Base)
	}
	steps, cfg, err := Lower(f, ctx)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var copyLayer *Layer
	for _, s := range steps {
		kinds = append(kinds, s.Instr.Cmd+":"+s.Kind)
		if s.Instr.Cmd == "COPY" {
			copyLayer = s.Layer
		}
	}
	if kinds[1] != "ENV:" || kinds[2] != "WORKDIR:layer" || kinds[3] != "COPY:copy" {
		t.Fatalf("kinds %v", kinds)
	}
	var got []string
	for _, e := range copyLayer.Entries {
		got = append(got, filepath.ToSlash(e.Path))
	}
	if !contains(got, "srv/www/index.html") {
		t.Fatalf("copy layer missing index.html: %v", got)
	}
	for _, p := range got {
		if filepath.Base(p) == "secret.env" || filepath.Base(p) == "a.css" {
			t.Fatalf("ignored file copied: %v", got)
		}
	}
	if cfg.WorkingDir != "/srv/www" || len(cfg.ExposedPorts) != 1 || cfg.Entrypoint[0] != "httpd" {
		t.Fatalf("config %+v", cfg)
	}
	tarBytes, err := copyLayer.Tar()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Gzip(tarBytes); err != nil {
		t.Fatal(err)
	}
	if DiffID(tarBytes) == "" {
		t.Fatal(err)
	}
}

func TestLowerRejects(t *testing.T) {
	ctx := &BuildContext{Dir: t.TempDir()}
	for _, src := range []string{
		"FROM alpine\nRUN --mount=type=cache,target=/root/.cache go build",
		"FROM alpine\nCOPY --from=builder /out /out",
		"FROM alpine\nADD https://example.com/x /x",
		"FROM alpine\nBOGUS thing",
	} {
		f, err := Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if _, _, err := Lower(f, ctx); err == nil {
			t.Errorf("expected an error for %q", src)
		}
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
