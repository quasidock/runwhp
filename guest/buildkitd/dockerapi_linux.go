//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// dockerAPI is runwhp's Engine API, as the builder reaches it: through
// Docker's socket, bound to the API server that started the builder.
type dockerAPI struct {
	c *http.Client
}

// apiBase is the API's address over the socket: its version is the
// server's, which answers every version runwhp speaks with its own.
const apiBase = "http://runwhp/v1.56"

func newDockerAPI(socket string) *dockerAPI {
	return &dockerAPI{c: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableCompression: true,
	}}}
}

// errNoImage is an image the store does not have.
var errNoImage = errors.New("no such image")

// imageInfo is what the builder needs of an image in runwhp's store.
type imageInfo struct {
	ID           string `json:"Id"`
	RepoDigests  []string
	Os           string
	Architecture string
}

func (a *dockerAPI) inspect(ctx context.Context, name string) (imageInfo, error) {
	var info imageInfo
	resp, err := a.do(ctx, http.MethodGet, "/images/"+escapeRef(name)+"/json", nil, "")
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return info, errNoImage
	}
	if err := apiError(resp); err != nil {
		return info, err
	}
	return info, json.NewDecoder(resp.Body).Decode(&info)
}

// save is the image as an archive, an OCI layout among Docker's files.
func (a *dockerAPI) save(ctx context.Context, name string) (io.ReadCloser, error) {
	resp, err := a.do(ctx, http.MethodGet, "/images/get?names="+url.QueryEscape(name), nil, "")
	if err != nil {
		return nil, err
	}
	if err := apiError(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

// load loads an archive into the store, and returns once it is there, or
// the load failed.
func (a *dockerAPI) load(ctx context.Context, archive io.Reader) error {
	resp, err := a.do(ctx, http.MethodPost, "/images/load?quiet=1", archive, "application/x-tar")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := apiError(resp); err != nil {
		return err
	}
	return jsonStreamError(resp.Body)
}

func (a *dockerAPI) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := a.c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runwhp's Engine API: %w", err)
	}
	return resp, nil
}

// escapeRef keeps a reference's slashes, which the API's routes take in
// an image's name.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func apiError(resp *http.Response) error {
	if resp.StatusCode < 300 {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct{ Message string }
	if json.Unmarshal(data, &e) == nil && e.Message != "" {
		return errors.New(e.Message)
	}
	return fmt.Errorf("runwhp's Engine API: %s: %s", resp.Status, strings.TrimSpace(string(data)))
}

// jsonStreamError reads a JSON message stream to its end, and returns the
// error it reports, if any.
func jsonStreamError(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var m struct {
			Error       string
			ErrorDetail *struct{ Message string }
		}
		if err := dec.Decode(&m); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if m.ErrorDetail != nil && m.ErrorDetail.Message != "" {
			return errors.New(m.ErrorDetail.Message)
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
	}
}
