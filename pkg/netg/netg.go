// Package netg is the guest's network bridge.
//
// iOS cannot host sockets on behalf of other processes, but the runtime host
// can. Inside the guest, programs reach the host through /pb/host/net/<n>:
// write a base64 request line, read the base64 response. `netg serve` is the
// guest side of that protocol and translates the request into a real socket
// call chain executed by the host process.
//
// The protocol is deliberately tiny and line oriented so it survives the
// runtime's pipe handling:
//
//	request:  CONNECT tcp <host>:<port> <base64payload>\n
//	          REQUEST <method> <url> <base64-body>\n
//	          RESOLVE <hostname>\n
//	response: <status>\n<base64body>\n
package netg

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DirName is the request directory inside the app data root.
const DirName = "host/net"

// Request is one host network operation.
type Request struct {
	Op      string            `json:"op"` // request | connect | resolve
	Method  string            `json:"method,omitempty"`
	URL     string            `json:"url,omitempty"`
	Host    string            `json:"host,omitempty"`
	Port    int               `json:"port,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"` // base64
	Timeout int               `json:"timeout,omitempty"`
}

// Response is the host's answer.
type Response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"` // base64
	Error   string            `json:"error,omitempty"`
}

// Exchange performs one request through the host, given the directory the host
// polls for request files.
func Exchange(dir string, req Request, timeout time.Duration) (Response, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Response{}, err
	}
	id := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	reqPath := filepath.Join(dir, id+".req")
	respPath := filepath.Join(dir, id+".resp")
	raw, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	tmp := reqPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return Response{}, err
	}
	if err := os.Rename(tmp, reqPath); err != nil {
		return Response{}, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(respPath); err == nil && len(data) > 0 {
			var resp Response
			if err := json.Unmarshal(data, &resp); err != nil {
				return Response{}, err
			}
			_ = os.Remove(reqPath)
			_ = os.Remove(respPath)
			return resp, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = os.Remove(reqPath)
	return Response{}, fmt.Errorf("netg: host did not answer within %s", timeout)
}

// Serve handles requests arriving from the host in dirUntilStop. Each request
// is executed with the standard library; it is the path used when the daemon
// runs where sockets are available. In the app the host process handles the
// requests instead, and this loop is only used by tests and the Linux build.
func Serve(dir string, stop <-chan struct{}) error {
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		entries, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".req") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var req Request
			if err := json.Unmarshal(raw, &req); err != nil {
				continue
			}
			resp := Execute(req)
			out, _ := json.Marshal(resp)
			_ = os.WriteFile(strings.TrimSuffix(path, ".req")+".resp", out, 0o644)
			_ = os.Remove(path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Execute performs one request with the standard library. The iOS host mirrors
// this with NSURLSession so the app never needs to fork a helper.
func Execute(req Request) Response {
	timeout := time.Duration(req.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	switch req.Op {
	case "request":
		return executeHTTP(req, timeout)
	case "connect":
		return executeConnect(req, timeout)
	case "resolve":
		addrs, err := net.LookupHost(req.Host)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{Status: 200, Body: base64.StdEncoding.EncodeToString([]byte(strings.Join(addrs, " ")))}
	}
	return Response{Error: "netg: unknown op " + req.Op}
}

func executeHTTP(req Request, timeout time.Duration) Response {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if req.Body != "" {
		raw, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return Response{Error: err.Error()}
		}
		body = strings.NewReader(string(raw))
	}
	hr, err := http.NewRequest(method, req.URL, body)
	if err != nil {
		return Response{Error: err.Error()}
	}
	for k, v := range req.Headers {
		hr.Header.Set(k, v)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(hr)
	if err != nil {
		return Response{Error: err.Error()}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{Error: err.Error()}
	}
	headers := map[string]string{}
	for k := range resp.Header {
		headers[k] = resp.Header.Get(k)
	}
	return Response{Status: resp.StatusCode, Headers: headers,
		Body: base64.StdEncoding.EncodeToString(data)}
}

func executeConnect(req Request, timeout time.Duration) Response {
	addr := net.JoinHostPort(req.Host, fmt.Sprint(req.Port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return Response{Error: err.Error()}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if req.Body != "" {
		data, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return Response{Error: err.Error()}
		}
		if _, err := conn.Write(data); err != nil {
			return Response{Error: err.Error()}
		}
	}
	reader := bufio.NewReader(conn)
	out, err := io.ReadAll(io.LimitReader(reader, 64<<20))
	if err != nil && !errors.Is(err, io.EOF) {
		if len(out) == 0 {
			return Response{Error: err.Error()}
		}
	}
	return Response{Status: 200, Body: base64.StdEncoding.EncodeToString(out)}
}
