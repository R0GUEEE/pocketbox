package buildkitclient

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Instruction is one parsed Dockerfile line.
type Instruction struct {
	Cmd   string            // upper-case instruction
	Flags []string          // raw flags before the first argument (--foo=bar)
	Args  string            // everything after the flags, as written
	Env   map[string]string // resolved build-time variables for this line
	Line  int
	Raw   string
}

// File is a parsed Dockerfile.
type File struct {
	Base     string
	Stages   []string
	Instrs   []Instruction
	Platform string
	Env      map[string]string
	Args     map[string]string
}

type parser struct {
	file   *File
	env    map[string]string
	args   map[string]string
	line   int
	escape byte
}

// Parse reads a Dockerfile.
func Parse(input string) (*File, error) {
	f := &File{Env: map[string]string{}, Args: map[string]string{}}
	p := &parser{file: f, env: map[string]string{}, args: map[string]string{}, escape: '\\'}

	lines, err := logicalLines(input)
	if err != nil {
		return nil, err
	}
	// Inline parser directives must come first.
	for len(lines) > 0 {
		l := strings.TrimSpace(lines[0])
		if !strings.HasPrefix(l, "#") {
			break
		}
		body := strings.TrimSpace(strings.TrimPrefix(l, "#"))
		key, value, ok := strings.Cut(body, "=")
		if !ok {
			break
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "escape":
			if v := strings.TrimSpace(value); v == "`" || v == "\\" {
				p.escape = v[0]
			}
		case "syntax", "check":
		default:
			goto parsed
		}
		lines = lines[1:]
	}
parsed:
	_ = p.escape
	for _, line := range lines {
		p.line++
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		cmd, rest, ok := cutField(trimmed)
		if !ok {
			return nil, fmt.Errorf("dockerfile:%d: missing instruction", p.line)
		}
		cmd = strings.ToUpper(cmd)
		instr := Instruction{Cmd: cmd, Args: strings.TrimSpace(rest), Line: p.line, Raw: trimmed}
		args, flags := splitFlags(instr.Args)
		instr.Args = args
		instr.Flags = flags

		switch cmd {
		case "ARG":
			for _, kv := range splitWords(instr.Args) {
				name, val, has := strings.Cut(kv, "=")
				if has {
					val = expandVars(val, p.env)
				} else {
					val = p.env[name]
				}
				p.args[name] = val
				p.env[name] = val
			}
		case "ENV":
			for _, kv := range parseEnvStyle(instr.Args) {
				p.env[kv[0]] = kv[1]
			}
		case "FROM":
			fields := splitWords(instr.Args)
			if len(f.Stages) == 0 && len(fields) > 0 {
				f.Base = expandVars(fields[0], p.env)
			}
			if len(fields) >= 3 && strings.EqualFold(fields[1], "as") {
				f.Stages = append(f.Stages, fields[2])
			} else if len(fields) >= 1 {
				f.Stages = append(f.Stages, "")
			}
		case "WORKDIR":
			w := pathy(expandVars(instr.Args, p.env), p.env["PWD"])
			p.env["PWD"] = w
		}
		// Resolve build args in the recorded line as well, so the layering
		// stage does not need the whole variable history.
		instr.Env = cloneMap(p.env)
		f.Instrs = append(f.Instrs, instr)
	}
	f.Env = p.env
	f.Args = p.args
	return f, nil
}

func pathy(p, base string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	if base == "" {
		base = "/"
	}
	return strings.TrimSuffix(base, "/") + "/" + p
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// logicalLines joins backslash/powershell-style continuations and strips
// comments inside them the way BuildKit does.
func logicalLines(input string) ([]string, error) {
	sc := bufio.NewScanner(strings.NewReader(input))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	var out []string
	var cur strings.Builder
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimRight(line, " \t\r")
		if strings.HasSuffix(trimmed, "\\") {
			cur.WriteString(strings.TrimSuffix(trimmed, "\\"))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(line)
		out = append(out, cur.String())
		cur.Reset()
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out, nil
}

// splitFlags separates leading --flag[=value] tokens from the arguments.
func splitFlags(args string) (string, []string) {
	var flags []string
	s := strings.TrimSpace(args)
	for strings.HasPrefix(s, "--") {
		field, rest, _ := cutField(s)
		flags = append(flags, field)
		s = strings.TrimSpace(rest)
	}
	return s, flags
}

// cutField splits at the first unquoted space.
func cutField(s string) (string, string, bool) {
	var quote byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ' ' || c == '\t':
			return s[:i], s[i+1:], true
		}
	}
	if s == "" {
		return "", "", false
	}
	return s, "", true
}

// splitWords splits on unquoted whitespace, keeping quotes in the output only
// where they protect a space (callers that need exact values use splitWordsQ).
func splitWords(s string) []string {
	words, _ := splitWordsQ(s)
	return words
}

// splitWordsQ splits like a shell would for word splitting, honouring single
// and double quotes and backslash escapes.
func splitWordsQ(s string) ([]string, bool) {
	var out []string
	var cur strings.Builder
	var quote byte
	escaped := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		switch {
		case c == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ' ' || c == '\t':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out, quote == 0
}

// parseEnvStyle handles both "KEY value" and "KEY=value KEY2=value2" forms.
func parseEnvStyle(s string) [][2]string {
	words := splitWords(s)
	var out [][2]string
	if len(words) == 0 {
		return out
	}
	if !strings.Contains(words[0], "=") {
		k, v, _ := strings.Cut(s, " ")
		out = append(out, [2]string{k, strings.TrimSpace(v)})
		return out
	}
	for _, w := range words {
		k, v, ok := strings.Cut(w, "=")
		if !ok {
			continue
		}
		out = append(out, [2]string{k, v})
	}
	return out
}

// expandVars replaces $VAR and ${VAR} using env. Unknown names become empty.
func expandVars(s string, env map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				b.WriteByte(s[i])
				continue
			}
			name := s[i+2 : i+2+end]
			name, def, has := strings.Cut(name, ":-")
			if v, ok := env[name]; ok {
				b.WriteString(v)
			} else if has {
				b.WriteString(def)
			}
			i += end + 2
			continue
		}
		j := i + 1
		for j < len(s) && (s[j] == '_' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
			j++
		}
		if j == i+1 {
			b.WriteByte('$')
			continue
		}
		b.WriteString(env[s[i+1:j]])
		i = j - 1
	}
	return b.String()
}

// ExecForm reports whether an instruction argument is JSON array form, and
// returns it when it is.
func ExecForm(args string) ([]string, bool) {
	s := strings.TrimSpace(args)
	if !strings.HasPrefix(s, "[") {
		return nil, false
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, false
	}
	return out, true
}

// KVForm parses "key=value" arguments used by LABEL, VOLUME, EXPOSE.
func KVForm(args string) map[string]string {
	out := map[string]string{}
	for _, w := range splitWords(args) {
		k, v, ok := strings.Cut(w, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

// PortSpec parses "80", "80/tcp", "127.0.0.1:80/udp".
func PortSpec(arg string) (string, string, error) {
	proto := "tcp"
	if i := strings.LastIndexByte(arg, '/'); i >= 0 {
		proto = arg[i+1:]
		arg = arg[:i]
	}
	port := arg
	if i := strings.LastIndexByte(arg, ':'); i >= 0 {
		port = arg[i+1:]
	}
	n, err := strconv.Atoi(strings.SplitN(port, "-", 2)[0])
	if err != nil || n <= 0 || n > 65535 {
		return "", "", fmt.Errorf("invalid port %q", arg)
	}
	return port + "/" + proto, proto, nil
}
