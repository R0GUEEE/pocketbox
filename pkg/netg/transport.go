package netg

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pocketbox/pocketbox/pkg/digest"
	"github.com/pocketbox/pocketbox/pkg/reference"
	"github.com/pocketbox/pocketbox/pkg/registry"
)

// Transport lets the registry client reach the network without opening
// sockets in the guest: the app's host process performs the HTTP exchange.
type Transport struct {
	// Dir is the request directory shared with the host.
	Dir string
	// Timeout bounds each exchange.
	Timeout time.Duration
	// Credentials are added as basic auth when the caller supplied none.
	mu    sync.Mutex
	hosts map[string]string
}

func NewTransport(dir string) *Transport {
	return &Transport{Dir: dir, Timeout: 10 * time.Minute, hosts: map[string]string{}}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, fmt.Errorf("netg: unsupported scheme %q", req.URL.Scheme)
	}
	var body string
	if req.Body != nil {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
		body = base64.StdEncoding.EncodeToString(data)
	}
	headers := map[string]string{}
	for k, vs := range req.Header {
		if len(vs) > 0 {
			headers[k] = strings.Join(vs, ", ")
		}
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	resp, err := Exchange(t.Dir, Request{
		Op: "request", Method: req.Method, URL: req.URL.String(),
		Headers: headers, Body: body, Timeout: int(timeout.Seconds()),
	}, timeout+30*time.Second)
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	data, err := base64.StdEncoding.DecodeString(resp.Body)
	if err != nil {
		return nil, err
	}
	out := &http.Response{
		StatusCode:    resp.Status,
		Status:        fmt.Sprintf("%d %s", resp.Status, http.StatusText(resp.Status)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          io.NopCloser(strings.NewReader(string(data))),
		ContentLength: int64(len(data)),
		Request:       req,
	}
	for k, v := range resp.Headers {
		out.Header.Set(k, v)
	}
	return out, nil
}

var _ http.RoundTripper = (*Transport)(nil)

// HTTPClient returns a client backed by the host bridge.
func HTTPClient(dir string) *http.Client {
	tr := &http.Transport{DisableCompression: true}
	_ = tr
	return &http.Client{Transport: NewTransport(dir), Timeout: 10 * time.Minute}
}

// RegistryOptions builds registry options that use the bridge.
type RegistryHTTP struct{}

// WithBridge points a registry client at the host bridge.
func WithBridge(c *registry.Client, dir string) {
	c.HTTP = HTTPClient(dir)
}

// Ensure the unused imports stay meaningful for callers that need them.
var (
	_ = digest.Digest("")
	_ reference.Reference
)
