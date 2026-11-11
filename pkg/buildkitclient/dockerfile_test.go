package buildkitclient

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseBasics(t *testing.T) {
	src := `
# a comment
FROM alpine:3.20 AS build
ARG VERSION=1.2
ENV APP_HOME=/app PORT=8080
WORKDIR $APP_HOME
RUN mkdir -p src && \
    echo done # not a comment marker inside a command
COPY ["a b.txt", "/app/"]
EXPOSE 8080/udp 9000
CMD ["/app/run", "--port=8080"]
`
	f, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if f.Base != "alpine:3.20" {
		t.Fatalf("base %q", f.Base)
	}
	if len(f.Stages) != 1 || f.Stages[0] != "build" {
		t.Fatalf("stages %v", f.Stages)
	}
	var cmds []string
	for _, i := range f.Instrs {
		cmds = append(cmds, i.Cmd)
	}
	want := []string{"FROM", "ARG", "ENV", "WORKDIR", "RUN", "COPY", "EXPOSE", "CMD"}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("cmds %v", cmds)
	}
	run := f.Instrs[4]
	if !strings.Contains(run.Args, "echo done") || strings.Contains(run.Args, "\n") {
		t.Fatalf("continuation not joined: %q", run.Args)
	}
	if f.Instrs[3].Env["PWD"] != "/app" || f.Instrs[3].Env["APP_HOME"] != "/app" {
		t.Fatalf("workdir env %v", f.Instrs[3].Env)
	}
	exec, ok := ExecForm(f.Instrs[7].Args)
	if !ok || exec[0] != "/app/run" || exec[1] != "--port=8080" {
		t.Fatalf("exec form %v %v", exec, ok)
	}
	if p, _, err := PortSpec("8080/udp"); err != nil || p != "8080/udp" {
		t.Fatalf("port %q %v", p, err)
	}
	if _, _, err := PortSpec("nope"); err == nil {
		t.Fatal("expected invalid port")
	}
}

func TestExpandVars(t *testing.T) {
	env := map[string]string{"A": "one", "B": "two"}
	cases := map[string]string{
		"$A":        "one",
		"${A}-$B":   "one-two",
		"${X:-def}": "def",
		"literal $": "literal $",
		"${A}${B}":  "onetwo",
	}
	for in, want := range cases {
		if got := expandVars(in, env); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestSplitFlagsAndWords(t *testing.T) {
	args, flags := splitFlags(`--chown=1000:1000 --chmod=755 ["src", "/dst"]`)
	if len(flags) != 2 || flags[0] != "--chown=1000:1000" || !strings.HasPrefix(args, "[") {
		t.Fatalf("flags %v args %q", flags, args)
	}
	words, ok := splitWordsQ(`echo "a b" c\ d 'e f'`)
	if !ok || !reflect.DeepEqual(words, []string{"echo", "a b", "c d", "e f"}) {
		t.Fatalf("words %v %v", words, ok)
	}
	if _, ok := splitWordsQ(`"unterminated`); ok {
		t.Fatal("expected unterminated quote")
	}
}
