// Command pocketbox is the container engine: a Dockerfile/OCI image store, a
// container lifecycle, and a Docker-compatible API.
//
// It runs in two places. On a Linux host it spawns containers as processes. In
// the iOS app it runs inside the embedded Linux guest, where the runtime host
// turns each command into one program image of the guest VM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/pocketbox/pocketbox/pkg/buildkitclient"
	"github.com/pocketbox/pocketbox/pkg/cli"
	"github.com/pocketbox/pocketbox/pkg/dirs"
	"github.com/pocketbox/pocketbox/pkg/engine"
	"github.com/pocketbox/pocketbox/pkg/images"
	"github.com/pocketbox/pocketbox/pkg/native"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var code int
	var err error
	switch cmd {
	case "version", "--version", "-v":
		fmt.Printf("pocketbox %s (guest runtime %s)\n", version, cli.RuntimeName())
		return
	case "help", "--help", "-h":
		usage()
		return
	case "ps":
		code, err = cmdPs(args)
	case "images":
		code, err = cmdImages(args)
	case "pull":
		code, err = cmdPull(args)
	case "run":
		code, err = cmdRun(args)
	case "start", "stop", "restart", "rm", "logs", "inspect", "wait":
		code, err = cmdLifecycle(cmd, args)
	case "rmi":
		code, err = cmdRmi(args)
	case "build":
		code, err = cmdBuild(args)
	case "info":
		code, err = cmdInfo(args)
	case "serve":
		code, err = cmdServe(args)
	case "commit", "save", "load", "push", "exec":
		err = fmt.Errorf("%s is not implemented in this build", cmd)
		code = 1
	default:
		err = fmt.Errorf("unknown command %q (try: pocketbox help)", cmd)
		code = 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pocketbox:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `pocketbox - containers on your phone

  pocketbox ps [-a]                 list containers
  pocketbox images                  list images
  pocketbox pull <ref>              download an image
  pocketbox run [options] <image> [command...]
        -d, --detach          run in the background
        -i, --interactive     keep stdin open (attached only)
        --name NAME           container name
        -e KEY=VALUE          environment (repeatable)
        -p HOST:CONTAINER     publish a port (repeatable)
        -v SRC:DST            bind mount or named volume
        -w DIR                working directory
        -u USER               user
        --rm                  remove when it exits
        --restart POLICY      no|always|unless-stopped
  pocketbox start|stop|restart|rm <container...>
  pocketbox logs [-f] [--tail N] <container>
  pocketbox commit <container> <ref>
  pocketbox rmi <image...>
  pocketbox build -t NAME <context-dir>    (Dockerfile in the context)
  pocketbox inspect <container-or-image>
  pocketbox info
  pocketbox serve [--listen tcp://ADDR:PORT]

Images are resolved against Docker Hub by default; any OCI registry works if
the reference names its host, e.g. ghcr.io/owner/app:tag.
`)
}

func openEngine() (*engine.Engine, error) {
	l, err := dirs.Resolve()
	if err != nil {
		return nil, err
	}
	if err := l.Create(); err != nil {
		return nil, err
	}
	imgs := images.New(l)
	e, err := engine.New(l, imgs)
	if err != nil {
		return nil, err
	}
	e.Runner = runtimeRunner(l)
	cli.AttachHostBridge(imgs, l)
	return e, nil
}

func cmdPs(args []string) (int, error) {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	all := fs.Bool("a", false, "show stopped containers")
	fs.BoolVar(all, "all", false, "show stopped containers")
	quiet := fs.Bool("q", false, "only ids")
	format := fs.String("format", "", "tabular (default) or json")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	e, err := openEngine()
	if err != nil {
		return 1, err
	}
	list := e.List(*all)
	if *format == "json" {
		return 0, printJSON(list)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if !*quiet {
		fmt.Fprintln(w, "CONTAINER ID\tIMAGE\tCOMMAND\tCREATED\tSTATUS\tPORTS\tNAMES")
	}
	for _, c := range list {
		if *quiet {
			fmt.Println(c.ID[:12])
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			c.ID[:12], c.Image, truncate(strings.Join(append(c.Entrypoint, c.Cmd...), " "), 40),
			humanAge(c.Created), statusText(c), portText(c), c.Name)
	}
	return 0, w.Flush()
}

func statusText(c *engine.Container) string {
	if c.State == engine.StateRunning {
		return "Up " + humanDuration(time.Since(c.StartedAt))
	}
	if c.State == engine.StateCreated {
		return "Created"
	}
	return fmt.Sprintf("Exited (%d) %s", c.ExitCode, humanAge(c.FinishedAt))
}

func portText(c *engine.Container) string {
	var parts []string
	for _, p := range c.Ports {
		if p.HostPort != "" {
			parts = append(parts, fmt.Sprintf("0.0.0.0:%s->%s/%s", p.HostPort, p.ContainerPort, p.Protocol))
		} else {
			parts = append(parts, fmt.Sprintf("%s/%s", p.ContainerPort, p.Protocol))
		}
	}
	return strings.Join(parts, ", ")
}

func cmdImages(args []string) (int, error) {
	fs := flag.NewFlagSet("images", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "JSON output")
	quiet := fs.Bool("q", false, "only references")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	e, err := openEngine()
	if err != nil {
		return 1, err
	}
	list, err := e.Images.List()
	if err != nil {
		return 1, err
	}
	if *jsonOut {
		return 0, printJSON(list)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if !*quiet {
		fmt.Fprintln(w, "REPOSITORY\tTAG\tIMAGE ID\tSIZE\tCREATED")
	}
	for _, img := range list {
		if *quiet {
			fmt.Println(img.Reference)
			continue
		}
		repo, tag := splitRef(img.Reference)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", repo, tag, shortDigest(img.Digest), humanSize(img.Size), humanAge(img.Created))
	}
	return 0, w.Flush()
}

func cmdPull(args []string) (int, error) {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, errors.New("usage: pocketbox pull <image>")
	}
	e, err := openEngine()
	if err != nil {
		return 1, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	img, err := e.Images.Pull(ctx, fs.Arg(0), func(p images.Progress) {
		switch p.Event {
		case "layer-done":
			fmt.Fprintf(os.Stderr, "%s: layer %d/%d (%d/%d bytes)\n", p.Image, p.Layer+1, p.Layers, p.Bytes, p.Total)
		case "apply-done":
			fmt.Fprintf(os.Stderr, "%s: extracted layer %d/%d\n", p.Image, p.Layer+1, p.Layers)
		}
	})
	if err != nil {
		return 1, err
	}
	fmt.Printf("%s: %s (%s)\n", img.Reference, shortDigest(img.Digest), humanSize(img.Size))
	return 0, nil
}

func cmdRun(args []string) (int, error) {
	var opts runOptions
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-d" || a == "--detach":
			opts.detach = true
		case a == "-i" || a == "--interactive":
			opts.interactive = true
		case a == "--rm":
			opts.rm = true
		case a == "--name":
			i++
			opts.name = arg(args, i)
		case strings.HasPrefix(a, "--name="):
			opts.name = strings.TrimPrefix(a, "--name=")
		case a == "-e" || a == "--env":
			i++
			opts.env = append(opts.env, arg(args, i))
		case strings.HasPrefix(a, "--env="):
			opts.env = append(opts.env, strings.TrimPrefix(a, "--env="))
		case a == "-p" || a == "--publish":
			i++
			opts.ports = append(opts.ports, arg(args, i))
		case a == "-v" || a == "--volume":
			i++
			opts.volumes = append(opts.volumes, arg(args, i))
		case a == "-w" || a == "--workdir":
			i++
			opts.workdir = arg(args, i)
		case a == "-u" || a == "--user":
			i++
			opts.user = arg(args, i)
		case a == "--restart":
			i++
			opts.restart = arg(args, i)
		case strings.HasPrefix(a, "--restart="):
			opts.restart = strings.TrimPrefix(a, "--restart=")
		case a == "--entrypoint":
			i++
			opts.entrypoint = []string{arg(args, i)}
		case strings.HasPrefix(a, "-"):
			return 2, fmt.Errorf("run: unknown option %s", a)
		default:
			rest = append(args[i:], rest...)
			i = len(args)
		}
	}
	if len(rest) == 0 {
		return 2, errors.New("usage: pocketbox run [options] <image> [command...]")
	}
	opts.image = rest[0]
	opts.command = rest[1:]

	e, err := openEngine()
	if err != nil {
		return 1, err
	}
	return cli.Run(e, opts.toCreate(), opts.detach, opts.interactive, os.Stdout)
}

func arg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

type runOptions struct {
	detach      bool
	interactive bool
	rm          bool
	name        string
	env         []string
	ports       []string
	volumes     []string
	workdir     string
	user        string
	restart     string
	entrypoint  []string
	image       string
	command     []string
}

func (o runOptions) toCreate() engine.CreateOptions {
	ports, _ := native.PortsOf(o.ports)
	var mounts []engine.Mount
	for _, v := range o.volumes {
		parts := strings.Split(v, ":")
		switch len(parts) {
		case 1:
			mounts = append(mounts, engine.Mount{Type: "volume", Source: parts[0], Target: parts[0]})
		case 2:
			mounts = append(mounts, engine.Mount{Type: "bind", Source: parts[0], Target: parts[1]})
		case 3:
			mounts = append(mounts, engine.Mount{Type: "bind", Source: parts[0], Target: parts[1], ReadOnly: parts[2] == "ro"})
		}
	}
	return engine.CreateOptions{
		Name: o.name, Image: o.image, Env: o.env, Cmd: o.command,
		Entrypoint: o.entrypoint, WorkingDir: o.workdir, User: o.user,
		Mounts: mounts, Ports: ports, RestartPolicy: o.restart, AutoRemove: o.rm,
	}
}

func cmdLifecycle(cmd string, args []string) (int, error) {
	var follow bool
	var tail = 0
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-f" || args[i] == "--follow":
			follow = true
		case args[i] == "--tail":
			i++
			fmt.Sscanf(arg(args, i), "%d", &tail)
		case strings.HasPrefix(args[i], "--tail="):
			fmt.Sscanf(strings.TrimPrefix(args[i], "--tail="), "%d", &tail)
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) == 0 {
		return 2, fmt.Errorf("usage: pocketbox %s <container>", cmd)
	}
	e, err := openEngine()
	if err != nil {
		return 1, err
	}
	for _, ref := range rest {
		switch cmd {
		case "start":
			if err := e.Start(ref); err != nil {
				return 1, err
			}
			fmt.Println(ref)
		case "stop":
			if err := e.Stop(ref, 10*time.Second); err != nil {
				return 1, err
			}
			fmt.Println(ref)
		case "restart":
			_ = e.Stop(ref, 10*time.Second)
			if err := e.Start(ref); err != nil {
				return 1, err
			}
			fmt.Println(ref)
		case "rm":
			if err := e.Remove(ref, true); err != nil {
				return 1, err
			}
			fmt.Println(ref)
		case "wait":
			code, err := e.Wait(ref)
			if err != nil {
				return 1, err
			}
			fmt.Println(code)
		case "inspect":
			c, err := e.Get(ref)
			if err != nil {
				return 1, err
			}
			return 0, printJSON(c)
		case "logs":
			c, err := e.Get(ref)
			if err != nil {
				return 1, err
			}
			return cli.Logs(e, c, tail, follow, os.Stdout)
		}
	}
	return 0, nil
}

func cmdRmi(args []string) (int, error) {
	e, err := openEngine()
	if err != nil {
		return 1, err
	}
	for _, ref := range args {
		img, err := e.Images.Get(ref)
		if err != nil {
			return 1, err
		}
		if err := e.Images.Remove(img); err != nil {
			return 1, err
		}
		fmt.Println(ref)
	}
	return 0, nil
}

func cmdBuild(args []string) (int, error) {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	tag := fs.String("t", "", "image name and tag")
	fs.StringVar(tag, "tag", "", "image name and tag")
	file := fs.String("f", "", "Dockerfile path (default: <context>/Dockerfile)")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	ctxDir := fs.Arg(0)
	if ctxDir == "" {
		ctxDir = "."
	}
	if *tag == "" {
		return 2, errors.New("build: -t is required")
	}
	df := *file
	if df == "" {
		df = ctxDir + "/Dockerfile"
	}
	raw, err := os.ReadFile(df)
	if err != nil {
		return 1, err
	}
	e, err := openEngine()
	if err != nil {
		return 1, err
	}
	opts := engine.BuildOptions{Tag: *tag, Dockerfile: string(raw), ContextDir: ctxDir,
		Progress: func(step, total int, msg string) {
			fmt.Fprintf(os.Stderr, "step %d/%d: %s\n", step, total, msg)
		}}
	if _, err := e.Build(context.Background(), opts); err != nil {
		return 1, err
	}
	fmt.Println("built", *tag)
	return 0, nil
}

func cmdInfo(args []string) (int, error) {
	e, err := openEngine()
	if err != nil {
		return 1, err
	}
	containers, _ := os.ReadDir(e.Layout.RootFS())
	type folder struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	out := struct {
		Version    string   `json:"version"`
		Runtime    string   `json:"runtime"`
		DataRoot   string   `json:"dataRoot"`
		Containers int      `json:"containers"`
		Images     int      `json:"images"`
		Volumes    int      `json:"volumes"`
		RootFS     []folder `json:"guestDirs"`
	}{Version: version, Runtime: cli.RuntimeName(), DataRoot: e.Layout.Root}
	out.Containers = len(e.List(true))
	if imgs, err := e.Images.List(); err == nil {
		out.Images = len(imgs)
	}
	out.Volumes = len(e.Volumes())
	for _, d := range containers {
		out.RootFS = append(out.RootFS, folder{Name: d.Name()})
	}
	if len(args) > 0 && args[0] == "--plain" {
		fmt.Printf("pocketbox %s\nruntime %s\ndata %s\ncontainers %d\nimages %d\n",
			out.Version, out.Runtime, out.DataRoot, out.Containers, out.Images)
		return 0, nil
	}
	return 0, printJSON(out)
}

func cmdServe(args []string) (int, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "tcp://127.0.0.1:2375", "listen address")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	e, err := openEngine()
	if err != nil {
		return 1, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("pocketbox daemon listening on %s\n", *listen)
	return 0, cli.Serve(ctx, e, *listen, os.Stdout)
}

func printJSON(v interface{}) error {
	return cli.PrintJSON(os.Stdout, v)
}

func splitRef(ref string) (string, string) {
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

func shortDigest(d fmt.Stringer) string {
	s := d.String()
	if i := strings.IndexByte(s, ':'); i >= 0 && len(s) > i+13 {
		return s[:i+13]
	}
	return s
}

func humanSize(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "kMGTPE"[exp])
}

func humanAge(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return humanDuration(time.Since(t)) + " ago"
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

var _ = buildkitclient.DiffID
