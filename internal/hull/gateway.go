// Copyright (c) 2026, NOFire AI
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hull

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Forward is one host-to-guest port forward, as the gateway API renders it
// (netgw.Forward in hull). Remote must be ip:port.
type Forward struct {
	Protocol string `json:"protocol"`
	Local    string `json:"local"`
	Remote   string `json:"remote"`
}

// Gateway is a client for the gateway's HTTP API on a unix socket.
type Gateway struct {
	APISock string
	client  *http.Client
}

// NewGateway returns a client for the API socket. The transport is kept for
// the life of the client, so a long-lived agent does not leak connections.
func NewGateway(apiSock string) *Gateway {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", apiSock)
		},
		MaxIdleConns:    2,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Gateway{APISock: apiSock, client: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
}

func (g *Gateway) call(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		payload = bytes.NewReader(b)
	}
	// The host is a placeholder; the dialer connects to the socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://gateway"+path, payload)
	if err != nil {
		return nil, 0, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("gateway API %s: %w", g.APISock, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return out, resp.StatusCode, err
}

func statusErr(what string, body []byte, status int) error {
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(status)
	}
	return fmt.Errorf("gateway %s: HTTP %d: %s", what, status, msg)
}

// Forwards lists the forwards the gateway runs.
func (g *Gateway) Forwards(ctx context.Context) ([]Forward, error) {
	body, status, err := g.call(ctx, http.MethodGet, "/forwards", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusErr("list forwards", body, status)
	}
	var out []Forward
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("gateway forwards: %w", err)
	}
	return out, nil
}

// AddForward publishes Local on the host and carries it to Remote.
func (g *Gateway) AddForward(ctx context.Context, f Forward) error {
	body, status, err := g.call(ctx, http.MethodPost, "/forwards", f)
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return statusErr("add forward "+f.Local, body, status)
	}
	return nil
}

// RemoveForward withdraws the forward on Local. A missing one is not an error.
func (g *Gateway) RemoveForward(ctx context.Context, protocol, local string) error {
	q := url.Values{"protocol": {protocol}, "local": {local}}
	body, status, err := g.call(ctx, http.MethodDelete, "/forwards?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNotFound && status != http.StatusNoContent {
		return statusErr("remove forward "+local, body, status)
	}
	return nil
}

// ProbeTCP dials addr (ip:port) inside the virtual network and reports
// whether it accepted.
func (g *Gateway) ProbeTCP(ctx context.Context, addr string) (bool, error) {
	body, status, err := g.call(ctx, http.MethodGet, "/probe/tcp?"+url.Values{"addr": {addr}}.Encode(), nil)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusBadRequest:
		return false, statusErr("probe "+addr, body, status)
	default:
		return false, nil
	}
}

// Healthy reports whether the API answers.
func (g *Gateway) Healthy(ctx context.Context) error {
	_, err := g.Forwards(ctx)
	return err
}
