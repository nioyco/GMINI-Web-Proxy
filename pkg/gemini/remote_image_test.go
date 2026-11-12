package gemini

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func newImgClient(t *testing.T, allow bool) *Client {
	t.Helper()
	cfg := testConfig()
	cfg.AllowRemoteImages = allow
	c, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestIsPublicIP(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8": true, "93.184.216.34": true, "2606:4700::1111": true,
		"127.0.0.1": false, "::1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"169.254.169.254": false, "fe80::1": false, "fd00::1": false, "100.64.0.1": false,
		"0.0.0.0": false, "::": false, "224.0.0.1": false, "::ffff:127.0.0.1": false,
	}
	for ip, want := range cases {
		if got := isPublicIP(net.ParseIP(ip)); got != want {
			t.Errorf("isPublicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

// Remote URLs are refused unless explicitly allowed; inline data always works.
func TestRemoteImagesDisabledByDefault(t *testing.T) {
	c := newImgClient(t, false)
	_, err := c.imageBytes(context.Background(), InputImage{URL: "http://127.0.0.1:1/x.png"})
	if !errors.Is(err, ErrRemoteImagesDisabled) {
		t.Errorf("err = %v, want ErrRemoteImagesDisabled", err)
	}
	b, err := c.imageBytes(context.Background(), InputImage{Data: []byte("abc")})
	if err != nil || string(b) != "abc" {
		t.Errorf("inline data: %q %v", b, err)
	}
}

// Even when allowed, the default policy refuses loopback destinations (the httptest
// server below listens on 127.0.0.1) and never reaches the handler.
func TestRemoteImageBlocksLoopback(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte("img"))
	}))
	defer ts.Close()
	c := newImgClient(t, true)
	if _, err := c.imageBytes(context.Background(), InputImage{URL: ts.URL + "/a.png"}); err == nil ||
		!strings.Contains(err.Error(), "not allowed") {
		t.Errorf("err = %v, want destination-not-allowed", err)
	}
	if hits.Load() != 0 {
		t.Errorf("server was contacted despite the block")
	}
}

func portOf(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	_, p, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	return p
}

// Redirect re-validation (simulated on loopback via a port-based test policy): a
// first hop that is allowed redirects to a destination that is not; the second
// hop is refused and its handler never runs.
func TestRemoteImageRedirectRevalidated(t *testing.T) {
	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHits.Add(1)
		w.Write([]byte("SECRET"))
	}))
	defer internal.Close()
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png":
			w.Write([]byte("PNGDATA"))
		default:
			http.Redirect(w, r, internal.URL+"/secret", http.StatusFound)
		}
	}))
	defer public.Close()

	c := newImgClient(t, true)
	allowedPort := portOf(t, public)
	c.imageDest = func(ip net.IP, port string) bool { return port == allowedPort } // stand-in for "public"

	// Control: the allowed hop works.
	b, err := c.imageBytes(context.Background(), InputImage{URL: public.URL + "/ok.png"})
	if err != nil || string(b) != "PNGDATA" {
		t.Fatalf("allowed fetch: %q %v", b, err)
	}
	// Redirect into the disallowed destination is refused.
	_, err = c.imageBytes(context.Background(), InputImage{URL: public.URL + "/redir"})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("redirect err = %v, want destination-not-allowed", err)
	}
	if internalHits.Load() != 0 {
		t.Errorf("redirect target was contacted")
	}
}

func TestRemoteImageRedirectLimitAndSize(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loop":
			http.Redirect(w, r, ts.URL+"/loop", http.StatusFound)
		case "/big":
			w.Header().Set("Content-Length", "99999999")
			w.Write([]byte("x"))
		}
	}))
	defer ts.Close()
	c := newImgClient(t, true)
	c.imageDest = func(net.IP, string) bool { return true }
	if _, err := c.imageBytes(context.Background(), InputImage{URL: ts.URL + "/loop"}); err == nil ||
		!strings.Contains(err.Error(), "redirects") {
		t.Errorf("loop err = %v", err)
	}
	if _, err := c.imageBytes(context.Background(), InputImage{URL: ts.URL + "/big"}); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Errorf("size err = %v", err)
	}
}
