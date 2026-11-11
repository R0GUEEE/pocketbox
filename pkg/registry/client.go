// Package registry is a minimal Docker Registry v2 / OCI distribution client:
// token auth, manifest and blob fetch, and blob upload.
package registry

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pocketbox/pocketbox/pkg/digest"
	"github.com/pocketbox/pocketbox/pkg/reference"
)

// ErrBlobUnknown is returned when the registry has no such blob.
var ErrBlobUnknown = errors.New("registry: blob unknown")

// ErrNotFound means the repository or manifest does not exist.
var ErrNotFound = errors.New("registry: not found")

// Credentials for one registry host.
type Credentials struct {
	Username string
	Password string
	// Token is a bearer token supplied by the caller (skips the token dance).
	Token string
}

// Client talks to one registry host.
type Client struct {
	// Host is the API host, e.g. registry-1.docker.io or localhost:5000.
	Host  string
	Creds Credentials
	HTTP  *http.Client

	mu         sync.Mutex
	tokens     map[string]tokenEntry // by repository scope
	insecure   bool
	schemeOnce sync.Once
	scheme     string
}

type tokenEntry struct {
	token   string
	expires time.Time
}

// Options configure a Client.
type Options struct {
	Insecure bool
	Timeout  time.Duration
	// Transport replaces the default HTTP transport. The guest build sets it to
	// the host bridge so no socket is ever opened inside the sandbox.
	Transport http.RoundTripper
}

// New builds a client for a registry host.
func New(host string, creds Credentials, opts Options) *Client {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	pool := 16
	if s := defaultLocalStack; s != "" {
		// iSH's network stack gets slow with parallel TLS; a small pool keeps
		// layer downloads from starving everything else on the device.
		pool = 4
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          pool,
		MaxIdleConnsPerHost:   pool,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     false,
	}
	var rt http.RoundTripper = tr
	if opts.Transport != nil {
		rt = opts.Transport
	}
	c := &Client{
		Host:   host,
		Creds:  creds,
		tokens: map[string]tokenEntry{},
		HTTP:   &http.Client{Transport: rt, Timeout: timeout},
	}
	if opts.Insecure {
		c.insecure = true
		tr.TLSClientConfig.InsecureSkipVerify = true
	}
	c.scheme = "https"
	if opts.Insecure && (strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1")) {
		c.scheme = "http"
	}
	return c
}

// defaultLocalStack is set on the iOS/iSH build so transports can be tuned.
var defaultLocalStack string

func (c *Client) baseURL() string { return c.scheme + "://" + c.Host }

type authChallenge struct {
	scheme string
	params map[string]string
}

func parseChallenge(h string) *authChallenge {
	h = strings.TrimSpace(h)
	if h == "" {
		return nil
	}
	parts := strings.SplitN(h, " ", 2)
	ch := &authChallenge{scheme: strings.ToLower(parts[0]), params: map[string]string{}}
	if len(parts) == 1 {
		return ch
	}
	for _, kv := range splitParams(parts[1]) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		ch.params[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(v, `"`)
	}
	return ch
}

func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; {
		case ch == '"':
			inQuote = !inQuote
			cur.WriteByte(ch)
		case ch == ',' && !inQuote:
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if cur.Len() > 0 {
		out = append(out, strings.TrimSpace(cur.String()))
	}
	return out
}

// token fetches (and caches) a bearer token for the given scope.
func (c *Client) token(ctx context.Context, ch *authChallenge, scope string) (string, error) {
	if c.Creds.Token != "" {
		return c.Creds.Token, nil
	}
	c.mu.Lock()
	if t, ok := c.tokens[scope]; ok && time.Now().Before(t.expires) {
		c.mu.Unlock()
		return t.token, nil
	}
	c.mu.Unlock()

	realm := ch.params["realm"]
	if realm == "" {
		return "", fmt.Errorf("registry: auth challenge without realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if svc := ch.params["service"]; svc != "" {
		q.Set("service", svc)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	if c.Creds.Username != "" || c.Creds.Password != "" {
		req.SetBasicAuth(c.Creds.Username, c.Creds.Password)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("registry: token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry: token request: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		IssuedAt    string `json:"issued_at"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("registry: token response: %w", err)
	}
	tok := tr.Token
	if tok == "" {
		tok = tr.AccessToken
	}
	if tok == "" {
		return "", fmt.Errorf("registry: token response has no token")
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	if ttl > time.Minute {
		ttl -= 30 * time.Second
	}
	c.mu.Lock()
	c.tokens[scope] = tokenEntry{token: tok, expires: time.Now().Add(ttl)}
	c.mu.Unlock()
	return tok, nil
}

// do performs a request and retries once against a token challenge.
func (c *Client) do(ctx context.Context, method, path, scope string, hdr http.Header) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, path, scope, hdr)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	ch := parseChallenge(resp.Header.Get("WWW-Authenticate"))
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if ch == nil || ch.scheme != "bearer" {
		return nil, fmt.Errorf("registry: unauthorized (%s)", scope)
	}
	if _, err := c.token(ctx, ch, scope); err != nil {
		return nil, err
	}
	req, err = c.newRequest(ctx, method, path, scope, hdr)
	if err != nil {
		return nil, err
	}
	return c.HTTP.Do(req)
}

func (c *Client) newRequest(ctx context.Context, method, path, scope string, hdr http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	c.mu.Lock()
	t, ok := c.tokens[scope]
	c.mu.Unlock()
	if ok && c.Creds.Token == "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	} else if c.Creds.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Creds.Token)
	} else if c.Creds.Username != "" || c.Creds.Password != "" {
		req.SetBasicAuth(c.Creds.Username, c.Creds.Password)
	}
	return req, nil
}

func errFor(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &e) == nil && len(e.Errors) > 0 {
		if e.Errors[0].Code == "BLOB_UNKNOWN" {
			return ErrBlobUnknown
		}
		if e.Errors[0].Code == "MANIFEST_UNKNOWN" || e.Errors[0].Code == "NAME_UNKNOWN" {
			return fmt.Errorf("%w: %s", ErrNotFound, e.Errors[0].Message)
		}
		return fmt.Errorf("registry: %s: %s", e.Errors[0].Code, e.Errors[0].Message)
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return fmt.Errorf("registry: %s: %s", resp.Status, msg)
}

func manifestPath(r reference.Reference) string {
	if r.Digest != "" {
		return "/v2/" + r.Repository + "/manifests/" + string(r.Digest)
	}
	return "/v2/" + r.Repository + "/manifests/" + url.PathEscape(r.Tag)
}

// GetManifest fetches a manifest (or index) and returns its body, media type
// and the digest the registry reports.
func (c *Client) GetManifest(ctx context.Context, r reference.Reference, accept []string) ([]byte, string, digest.Digest, error) {
	scope := "repository:" + r.Repository + ":pull"
	hdr := http.Header{}
	if len(accept) == 0 {
		accept = AcceptManifests
	}
	hdr.Set("Accept", strings.Join(accept, ", "))
	resp, err := c.do(ctx, http.MethodGet, manifestPath(r), scope, hdr)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", errFor(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", "", err
	}
	mt := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	var d digest.Digest
	if dn := resp.Header.Get("Docker-Content-Digest"); dn != "" {
		if parsed, err := digest.Parse(dn); err == nil {
			d = parsed
		}
	}
	if d == "" {
		sum := sha256.Sum256(body)
		d, _ = digest.New("sha256", hex.EncodeToString(sum[:]))
	}
	return body, mt, d, nil
}

// AcceptManifests is the Accept header covering every manifest flavour.
var AcceptManifests = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}

// Blob opens a blob stream. The caller must close it.
func (c *Client) Blob(ctx context.Context, repo string, d digest.Digest) (io.ReadCloser, int64, error) {
	scope := "repository:" + repo + ":pull"
	resp, err := c.do(ctx, http.MethodGet, "/v2/"+repo+"/blobs/"+string(d), scope, nil)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, 0, errFor(resp)
	}
	return resp.Body, resp.ContentLength, nil
}

// HasBlob reports whether the registry already stores a blob.
func (c *Client) HasBlob(ctx context.Context, repo string, d digest.Digest) (bool, error) {
	scope := "repository:" + repo + ":pull"
	resp, err := c.do(ctx, http.MethodHead, "/v2/"+repo+"/blobs/"+string(d), scope, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, nil
}

// Upload pushes a blob with a monolithic PUT.
func (c *Client) Upload(ctx context.Context, repo string, d digest.Digest, size int64, body io.Reader) error {
	scope := "repository:" + repo + ":push,pull"
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Content-Length", strconv.FormatInt(size, 10))
	resp, err := c.do(ctx, http.MethodPost, "/v2/"+repo+"/blobs/uploads/", scope, hdr)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("registry: begin upload: %s", resp.Status)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return errors.New("registry: upload location missing")
	}
	u, err := url.Parse(loc)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("digest", string(d))
	u.RawQuery = q.Encode()

	path := u.Path
	if u.Host != "" && u.Host != c.Host {
		// Upload to a different host (e.g. blob CDN): do it directly.
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), body)
		if err != nil {
			return err
		}
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
		c.mu.Lock()
		t, ok := c.tokens[scope]
		c.mu.Unlock()
		if ok {
			req.Header.Set("Authorization", "Bearer "+t.token)
		}
		r2, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		defer r2.Body.Close()
		if r2.StatusCode != http.StatusCreated {
			return errFor(r2)
		}
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL()+path+"?"+u.RawQuery, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	c.mu.Lock()
	t, ok := c.tokens[scope]
	c.mu.Unlock()
	if ok {
		req.Header.Set("Authorization", "Bearer "+t.token)
	} else if c.Creds.Username != "" {
		req.SetBasicAuth(c.Creds.Username, c.Creds.Password)
	}
	r2, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusCreated {
		return errFor(r2)
	}
	return nil
}

// PutManifest uploads a manifest with the given media type.
func (c *Client) PutManifest(ctx context.Context, repo, ref, mediaType string, body []byte) (digest.Digest, error) {
	scope := "repository:" + repo + ":push,pull"
	path := "/v2/" + repo + "/manifests/" + url.PathEscape(ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL()+path, bytesReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mediaType)
	c.mu.Lock()
	t, ok := c.tokens[scope]
	c.mu.Unlock()
	if ok {
		req.Header.Set("Authorization", "Bearer "+t.token)
	} else if c.Creds.Username != "" {
		req.SetBasicAuth(c.Creds.Username, c.Creds.Password)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", errFor(resp)
	}
	d, _ := digest.New("sha256", hex.EncodeToString(sum256(body)))
	return d, nil
}

// Ping checks that the API responds, following the auth challenge when needed.
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.HTTP.Get(c.baseURL() + "/v2/")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized {
		return nil
	}
	return fmt.Errorf("registry: ping: %s", resp.Status)
}

func sum256(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

type byteReader struct {
	b []byte
	i int
}

func bytesReader(b []byte) io.Reader { return &byteReader{b: b} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
