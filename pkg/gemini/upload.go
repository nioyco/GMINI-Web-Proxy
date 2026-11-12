package gemini

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// uploadEndpoint is Google's push (Scotty) endpoint for image attachments. It
// accepts an unauthenticated multipart POST and returns an opaque file reference.
const uploadEndpoint = "https://content-push.googleapis.com/upload/"

// uploadPushID is the fixed push channel id the Gemini web client uses for uploads.
const uploadPushID = "feeds/mcudyrk2a4khkz"

// maxImageBytes caps both a fetched remote image and an upload response read.
const maxImageBytes = 20 << 20

// uploadedImage is a successfully uploaded attachment: its push-service reference
// and the filename presented to the model.
type uploadedImage struct {
	ref      string
	filename string
}

// imageRefsField builds the inner[0][3] attachment list as [[[ref], filename], …],
// or nil when there are no images (so JSON serializes the slot as null).
func imageRefsField(images []uploadedImage) any {
	if len(images) == 0 {
		return nil
	}
	out := make([]any, len(images))
	for i, im := range images {
		out[i] = []any{[]any{im.ref}, im.filename}
	}
	return out
}

// uploadImages resolves and uploads each input image, returning the successful
// uploads. A failed image (bad data, fetch error, upload error) is logged and
// skipped so the request still proceeds with the remaining images and text.
func (c *Client) uploadImages(ctx context.Context, imgs []InputImage) []uploadedImage {
	if len(imgs) == 0 {
		return nil
	}
	out := make([]uploadedImage, 0, len(imgs))
	for i := range imgs {
		if ctx.Err() != nil {
			break // caller gone: stop fetching/uploading
		}
		data, err := c.imageBytes(ctx, imgs[i])
		if err != nil {
			c.log.Warn("skipping image: cannot obtain bytes", "index", i, "err", err)
			continue
		}
		name := imgs[i].Filename
		if name == "" {
			name = "image.png"
		}
		ref, err := c.uploadImage(ctx, data, name)
		if err != nil {
			c.log.Warn("skipping image: upload failed", "index", i, "err", err)
			continue
		}
		out = append(out, uploadedImage{ref: ref, filename: name})
	}
	return out
}

// ErrRemoteImagesDisabled is returned for a remote image_url when
// GEMINI_ALLOW_REMOTE_IMAGES is not enabled.
var ErrRemoteImagesDisabled = errors.New("remote image URLs are disabled (set GEMINI_ALLOW_REMOTE_IMAGES=1 to allow; data: URIs are always accepted)")

// maxRedirects bounds redirect chains when fetching a remote image.
const maxRedirects = 3

// imageBytes returns the image's bytes. Inline data is used as is; a remote URL is
// fetched only when remote images are explicitly allowed.
func (c *Client) imageBytes(ctx context.Context, img InputImage) ([]byte, error) {
	if len(img.Data) > 0 {
		return img.Data, nil
	}
	if img.URL == "" {
		return nil, errors.New("image has neither inline data nor URL")
	}
	if !c.cfg.AllowRemoteImages {
		return nil, ErrRemoteImagesDisabled
	}
	return c.fetchRemoteImage(ctx, img.URL)
}

// destAllowed is the default destination policy for remote image fetches: only
// public unicast addresses.
func destAllowed(ip net.IP, _ string) bool {
	return isPublicIP(ip)
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// isPublicIP rejects loopback, private (RFC 1918 / ULA), link-local, CGNAT,
// unspecified and multicast addresses (SSRF targets such as 127.0.0.1, 10.x,
// 169.254.169.254).
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 0 || cgnat.Contains(ip4) {
			return false
		}
	}
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified())
}

// newImageHTTPClient builds the client used for remote image fetches. Destination
// filtering is done in the dialer's Control hook, i.e. on the *resolved* IP of
// every connection attempt — so it also covers redirects (each hop dials anew),
// literal IPs and DNS rebinding. It deliberately ignores any configured proxy:
// dialing a proxy would make the check meaningless.
func (c *Client) newImageHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if !c.imageDest(net.ParseIP(host), port) {
				return fmt.Errorf("remote image destination %s is not allowed (loopback/private/link-local)", host)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: time.Duration(c.cfg.RequestTimeout) * time.Second,
		Transport: &http.Transport{
			Proxy:       nil,
			DialContext: dialer.DialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
			}
			return nil // the destination itself is vetted by the dialer
		},
	}
}

// fetchRemoteImage downloads a remote image with destination filtering, a redirect
// limit and a hard size cap (an oversized image is an error, not a truncation).
func (c *Client) fetchRemoteImage(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("invalid remote image URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.imageHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch image: http %d", resp.StatusCode)
	}
	if resp.ContentLength > maxImageBytes {
		return nil, fmt.Errorf("remote image exceeds %d bytes", maxImageBytes)
	}
	b, err := readLimited(resp.Body, maxImageBytes)
	if errors.Is(err, ErrResponseTooLarge) {
		return nil, fmt.Errorf("remote image exceeds %d bytes", maxImageBytes)
	}
	return b, err
}

// uploadImage POSTs the image to the push endpoint (multipart, no auth) and
// returns the resulting file reference string.
func (c *Client) uploadImage(ctx context.Context, data []byte, filename string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadEndpoint, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Push-ID", uploadPushID)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upload http %d", resp.StatusCode)
	}
	ref := strings.TrimSpace(string(body))
	if ref == "" {
		return "", errors.New("empty upload reference")
	}
	return ref, nil
}
