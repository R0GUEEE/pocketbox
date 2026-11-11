// Package api implements the Docker Engine API subset the app and the docker
// CLI use: ping, version, info, container and image listings, start/stop/logs
// and a pull endpoint.
//
// It speaks HTTP/1.1 over TCP. Unix sockets are not available inside the iOS
// app sandbox, so remote clients use tcp://<phone>:2375, optionally behind the
// app's TLS terminator.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbox/pocketbox/pkg/engine"
)

// APIVersion is the Docker API level the engine reports.
const APIVersion = "1.43"

// Version is the daemon version string.
var Version = "0.1.0"

type server struct {
	eng  *engine.Engine
	logf func(string, ...interface{})
}

// Serve accepts connections on listen ("tcp://host:port") until ctx ends.
func Serve(ctx context.Context, eng *engine.Engine, listen string, log io.Writer) error {
	addr := strings.TrimPrefix(strings.TrimPrefix(listen, "tcp://"), "http://")
	if addr == listen && !strings.Contains(listen, ":") {
		addr = listen + ":2375"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s := &server{eng: eng, logf: func(f string, a ...interface{}) {
		if log != nil {
			fmt.Fprintf(log, f+"\n", a...)
		}
	}}
	mux := http.NewServeMux()
	s.routes(mux)
	srv := &http.Server{Handler: logging(mux, s.logf), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	err = srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func logging(h http.Handler, logf func(string, ...interface{})) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		logf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func (s *server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/_ping", s.ping)
	mux.HandleFunc("/version", s.version)
	mux.HandleFunc("/info", s.info)
	mux.HandleFunc("/containers/json", s.containersList)
	mux.HandleFunc("/containers/", s.containersByID)
	mux.HandleFunc("/containers/create", s.containerCreate)
	mux.HandleFunc("/images/json", s.imagesList)
	mux.HandleFunc("/images/", s.imagesByRef)
	mux.HandleFunc("/images/create", s.imagePull)
	mux.HandleFunc("/build", s.build)
	mux.HandleFunc("/volumes", s.volumes)
	mux.HandleFunc("/events", s.events)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "page not found")
	})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Server", "pocketbox/"+Version)
	w.Header().Set("Api-Version", APIVersion)
	w.Header().Set("Ostype", "linux")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"message": msg})
}

func (s *server) ping(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Api-Version", APIVersion)
	w.Header().Set("Ostype", "linux")
	w.Header().Set("Builder-Version", "pocketbox-1")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func (s *server) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"Version":       Version,
		"ApiVersion":    APIVersion,
		"MinAPIVersion": "1.24",
		"Os":            "linux",
		"Arch":          archName(),
		"KernelVersion": "ish-guest",
		"GitCommit":     "pocketbox",
		"GoVersion":     "go",
		"Components": []map[string]interface{}{
			{"Name": "Engine", "Version": Version, "Details": map[string]string{
				"ApiVersion": APIVersion, "Arch": archName(), "Os": "linux"}},
		},
	})
}

func (s *server) info(w http.ResponseWriter, r *http.Request) {
	containers := s.eng.List(true)
	running := 0
	for _, c := range containers {
		if c.State == engine.StateRunning {
			running++
		}
	}
	imgs, _ := s.eng.Images.List()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ID":                "pocketbox-" + Version,
		"Name":              "pocketbox",
		"ServerVersion":     Version,
		"Containers":        len(containers),
		"ContainersRunning": running,
		"ContainersPaused":  0,
		"ContainersStopped": len(containers) - running,
		"Images":            len(imgs),
		"Volumes":           len(s.eng.Volumes()),
		"Driver":            "pocketbox-guest",
		"LoggingDriver":     "jsonless-file",
		"CgroupDriver":      "none",
		"KernelVersion":     "ish-guest",
		"OperatingSystem":   "Linux (pocketbox guest)",
		"OSType":            "linux",
		"Architecture":      archName(),
		"NCPU":              4,
		"MemTotal":          2 << 30,
		"ServerErrors":      nil,
		"DockerRootDir":     s.eng.Layout.Root,
		"Plugins": map[string]interface{}{
			"Volume":        []string{"local"},
			"Network":       []string{"bridge", "host", "none"},
			"Authorization": nil,
			"Log":           nil,
		},
		"RegistryConfig": map[string]interface{}{"IndexConfigs": map[string]interface{}{}},
	})
}

func (s *server) containersList(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
	list := s.eng.List(all)
	out := make([]map[string]interface{}, 0, len(list))
	for _, c := range list {
		out = append(out, containerSummary(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func containerSummary(c *engine.Container) map[string]interface{} {
	names := []string{"/" + c.Name}
	ports := []map[string]interface{}{}
	for _, p := range c.Ports {
		ports = append(ports, map[string]interface{}{
			"PrivatePort": atoi(p.ContainerPort), "PublicPort": atoi(p.HostPort),
			"Type": p.Protocol,
		})
	}
	status := "Created"
	if c.State == engine.StateRunning {
		status = "Up " + time.Since(c.StartedAt).Round(time.Second).String()
	} else if c.State == engine.StateExited {
		status = fmt.Sprintf("Exited (%d)", c.ExitCode)
	}
	labels := c.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return map[string]interface{}{
		"Id": c.ID, "Names": names, "Image": c.Image, "ImageID": c.ImageID,
		"Command": strings.Join(append(c.Entrypoint, c.Cmd...), " "),
		"Created": c.Created.Unix(), "State": c.State, "Status": status,
		"Ports": ports, "Labels": labels, "HostConfig": map[string]interface{}{"NetworkMode": c.Network},
		"NetworkSettings": map[string]interface{}{"Ports": map[string]interface{}{}},
		"Mounts":          []interface{}{},
	}
}

func (s *server) containersByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/containers/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = strings.Join(parts[1:], "/")
	}
	c, err := s.eng.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.inspect(c))
	case action == "" && r.Method == http.MethodDelete:
		force := r.URL.Query().Get("force") == "1"
		if err := s.eng.Remove(c.ID, force); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case action == "json":
		writeJSON(w, http.StatusOK, s.inspect(c))
	case action == "start":
		// The docker CLI sends start options as a JSON body with no
		// Content-Length; only the HostConfig keys actually used here matter.
		var startOpts struct {
			DetachKeys string `json:"detachKeys"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&startOpts)
		if err := s.eng.Start(c.ID); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case action == "stop":
		var stopOpts struct {
			Signal string `json:"signal"`
			T      int    `json:"t"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&stopOpts)
		timeout := 10 * time.Second
		if t := r.URL.Query().Get("t"); t != "" {
			timeout = time.Duration(atoi(t)) * time.Second
		}
		if err := s.eng.Stop(c.ID, timeout); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case action == "restart":
		_ = s.eng.Stop(c.ID, 10*time.Second)
		if err := s.eng.Start(c.ID); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case action == "kill":
		if err := s.eng.Kill(c.ID); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case action == "wait":
		code, err := s.eng.Wait(c.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"StatusCode": code})
	case action == "logs":
		s.logs(w, r, c)
	case strings.HasPrefix(action, "rename"):
		name := r.URL.Query().Get("name")
		if name == "" {
			writeErr(w, http.StatusBadRequest, "name query parameter is required")
			return
		}
		if err := s.eng.Rename(c.ID, name); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusNotFound, "unsupported container action "+action)
	}
}

func (s *server) logs(w http.ResponseWriter, r *http.Request, c *engine.Container) {
	follow := r.URL.Query().Get("follow") == "1" || r.URL.Query().Get("follow") == "true"
	// The docker CLI streams both streams multiplexed; without the framing the
	// client would print garbage, so logs are answered as a raw stream and the
	// client is told the container had no TTY.
	q := r.URL.Query()
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	tail := atoi(r.URL.Query().Get("tail"))
	if r.URL.Query().Get("tail") == "all" {
		tail = 0
	}
	if !follow {
		var buf strings.Builder
		if _, err := logsTo(&buf, s.eng, c, tail); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		_, _ = io.WriteString(w, buf.String())
		return
	}
	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	stop := r.Context().Done()
	f, err := s.eng.LogsFile(c.ID)
	if err != nil {
		return
	}
	defer f.Close()
	buf := make([]byte, 8192)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			for {
				n, err := f.Read(buf)
				if n > 0 {
					_, _ = w.Write(buf[:n])
					if flusher != nil {
						flusher.Flush()
					}
				}
				if err != nil {
					break
				}
			}
			if !s.eng.Running(c.ID) {
				return
			}
		}
	}
}

func logsTo(w io.Writer, e *engine.Engine, c *engine.Container, tail int) (int, error) {
	data, err := readLog(e, c)
	if err != nil {
		return 0, err
	}
	if tail > 0 {
		lines := strings.Split(strings.TrimRight(data, "\n"), "\n")
		if len(lines) > tail {
			lines = lines[len(lines)-tail:]
		}
		data = strings.Join(lines, "\n") + "\n"
	}
	return io.WriteString(w, data)
}

func (s *server) inspect(c *engine.Container) map[string]interface{} {
	state := map[string]interface{}{
		"Status": c.State, "Running": c.State == engine.StateRunning,
		"Paused": false, "Restarting": false, "OOMKilled": false,
		"ExitCode": c.ExitCode, "Error": c.Error, "Pid": c.Pid,
	}
	if !c.StartedAt.IsZero() {
		state["StartedAt"] = c.StartedAt.Format(time.RFC3339Nano)
	}
	if !c.FinishedAt.IsZero() {
		state["FinishedAt"] = c.FinishedAt.Format(time.RFC3339Nano)
	}
	env := c.Env
	if env == nil {
		env = []string{}
	}
	labels := c.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	mounts := []map[string]interface{}{}
	for _, m := range c.Mounts {
		mounts = append(mounts, map[string]interface{}{
			"Type": m.Type, "Source": m.Source, "Destination": m.Target, "RW": !m.ReadOnly,
		})
	}
	return map[string]interface{}{
		"Id": c.ID, "Name": "/" + c.Name, "Created": c.Created.Format(time.RFC3339Nano),
		"Image": c.ImageID, "State": state,
		"Config": map[string]interface{}{
			"Hostname": c.Hostname, "Image": c.Image, "Env": env, "ImageId": c.ImageID,
			"Cmd": c.Cmd, "Entrypoint": c.Entrypoint, "WorkingDir": c.WorkingDir,
			"User": c.User, "Labels": labels, "Tty": false, "OpenStdin": false,
		},
		"HostConfig": map[string]interface{}{
			"NetworkMode": c.Network, "Privileged": c.Privileged,
			"RestartPolicy": map[string]interface{}{"Name": restartName(c.RestartPolicy)},
			"AutoRemove":    c.AutoRemove,
		},
		"Mounts": mounts,
	}
}

func restartName(policy string) string {
	if policy == "" {
		return "no"
	}
	if i := strings.IndexByte(policy, ':'); i >= 0 {
		return policy[:i]
	}
	return policy
}

func (s *server) containerCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		Hostname     string              `json:"Hostname"`
		User         string              `json:"User"`
		Env          []string            `json:"Env"`
		Cmd          []string            `json:"Cmd"`
		Entrypoint   []string            `json:"Entrypoint"`
		Image        string              `json:"Image"`
		WorkingDir   string              `json:"WorkingDir"`
		Labels       map[string]string   `json:"Labels"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		HostConfig   struct {
			Binds        []string `json:"Binds"`
			PortBindings map[string][]struct {
				HostPort string `json:"HostPort"`
			} `json:"PortBindings"`
			Privileged    bool   `json:"Privileged"`
			NetworkMode   string `json:"NetworkMode"`
			AutoRemove    bool   `json:"AutoRemove"`
			RestartPolicy struct {
				Name string `json:"Name"`
			} `json:"RestartPolicy"`
		} `json:"HostConfig"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	var mounts []engine.Mount
	for _, b := range body.HostConfig.Binds {
		parts := strings.Split(b, ":")
		if len(parts) >= 2 {
			m := engine.Mount{Type: "bind", Source: parts[0], Target: parts[1]}
			if len(parts) == 3 && parts[2] == "ro" {
				m.ReadOnly = true
			}
			mounts = append(mounts, m)
		}
	}
	var ports []engine.PortBinding
	for cport, binds := range body.HostConfig.PortBindings {
		proto, port, _ := strings.Cut(cport, "/")
		if proto == "" {
			proto = "tcp"
		}
		if len(binds) == 0 {
			ports = append(ports, engine.PortBinding{ContainerPort: port, Protocol: proto})
		}
		for _, b := range binds {
			ports = append(ports, engine.PortBinding{ContainerPort: port, HostPort: b.HostPort, Protocol: proto})
		}
	}
	c, err := s.eng.Create(engine.CreateOptions{
		Name: r.URL.Query().Get("name"), Image: body.Image, Env: body.Env,
		Entrypoint: body.Entrypoint, Cmd: body.Cmd, WorkingDir: body.WorkingDir,
		User: body.User, Hostname: body.Hostname, Labels: body.Labels,
		Mounts: mounts, Ports: ports, Privileged: body.HostConfig.Privileged,
		Network: body.HostConfig.NetworkMode, AutoRemove: body.HostConfig.AutoRemove,
		RestartPolicy: body.HostConfig.RestartPolicy.Name,
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"Id": c.ID, "Warnings": "[]"})
}

func (s *server) imagesList(w http.ResponseWriter, r *http.Request) {
	list, err := s.eng.Images.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]interface{}{}
	for _, img := range list {
		repo, tag := splitRef(img.Reference)
		out = append(out, map[string]interface{}{
			"Id": img.Digest.String(), "ParentId": "", "RepoTags": []string{repo + ":" + tag},
			"RepoDigests": []string{repo + "@" + img.Digest.String()},
			"Created":     img.Created.Unix(), "Size": img.Size, "VirtualSize": img.Size,
			"SharedSize": -1, "Containers": -1, "Labels": img.Labels,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) imagesByRef(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/images/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 2 && parts[1] == "json" {
		img, err := s.eng.Images.Get(parts[0])
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, img)
		return
	}
	img, err := s.eng.Images.Get(strings.Join(parts, "/"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.eng.Images.Remove(img); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}
	writeJSON(w, http.StatusOK, img)
}

func (s *server) imagePull(w http.ResponseWriter, r *http.Request) {
	from := r.URL.Query().Get("fromImage")
	tag := r.URL.Query().Get("tag")
	ref := from
	if tag != "" {
		ref = from + ":" + tag
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	_, err := s.eng.Images.Pull(r.Context(), ref, func(p enginePullProgress) {
		_ = enc.Encode(map[string]string{"status": p.Event + " " + p.Image})
		if flusher != nil {
			flusher.Flush()
		}
	})
	if err != nil {
		_ = enc.Encode(map[string]string{"error": err.Error()})
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	_ = enc.Encode(map[string]string{"status": "Image " + ref + " pulled"})
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *server) build(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, "POST /build is served by the CLI (pocketbox build) in this build")
}

func (s *server) volumes(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		list := s.eng.Volumes()
		out := []map[string]interface{}{}
		for _, v := range list {
			out = append(out, map[string]interface{}{
				"Name": v.Name, "Driver": "local", "Mountpoint": v.Mountpoint,
				"CreatedAt": v.Created.Format(time.RFC3339), "Labels": v.Labels,
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"Volumes": out, "Warnings": nil})
		return
	}
	if r.Method == http.MethodPost {
		var body struct {
			Name   string            `json:"Name"`
			Labels map[string]string `json:"Labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		v, err := s.eng.CreateVolume(body.Name, body.Labels)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{"Name": v.Name, "Driver": "local"})
		return
	}
	writeErr(w, http.StatusMethodNotAllowed, "unsupported")
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"Type": "engine", "Action": "ping", "time": time.Now().Unix(),
	})
}

func archName() string {
	return "arm64"
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
