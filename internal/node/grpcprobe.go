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

package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"

	"golang.org/x/net/http2"
	corev1 "k8s.io/api/core/v1"
)

// grpcProbe calls grpc.health.v1.Health/Check on the container port through
// its gateway forward, as the kubelet's prober does. Only SERVING passes.
func (w *podWorker) grpcProbe(ctx context.Context, st *podState, g *corev1.GRPCAction) (bool, string) {
	port := g.Port
	local := st.Forwards[strconv.Itoa(int(port))]
	if local == "" {
		var err error
		if local, err = w.ensureForward(ctx, port); err != nil {
			return false, err.Error()
		}
	}
	service := ""
	if g.Service != nil {
		service = *g.Service
	}
	tlsMode := g.Mode != nil && *g.Mode == corev1.GRPCProbeModeTLS
	return grpcHealthCheck(ctx, local, net.JoinHostPort(st.IP, strconv.Itoa(int(port))), service, tlsMode)
}

// grpcHealthCheck asks the server behind local (the gateway forward) for
// the health of service; authority is the address the request names. It
// writes the unary call by hand over HTTP/2: a HealthCheckRequest carries
// one string and a HealthCheckResponse one enum, which does not justify a
// gRPC dependency.
func grpcHealthCheck(ctx context.Context, local, authority, service string, useTLS bool) (bool, string) {
	tr := &http2.Transport{
		AllowHTTP: !useTLS,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp", local)
			if err != nil || !useTLS {
				return conn, err
			}
			tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}}) //nolint:gosec // as the kubelet's prober
			if err := tc.HandshakeContext(ctx); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return tc, nil
		},
	}
	defer tr.CloseIdleConnections()

	var msg []byte
	if service != "" {
		msg = append([]byte{0x0a, byte(len(service))}, service...) // field 1, length-delimited
	}
	frame := make([]byte, 5, 5+len(msg))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(msg)))
	frame = append(frame, msg...)

	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, scheme+"://"+authority+"/grpc.health.v1.Health/Check", bytes.NewReader(frame))
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("Te", "trailers")
	req.Header.Set("User-Agent", probeUserAgent)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return false, fmt.Sprintf("rpc error: code = Unavailable desc = %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeOutput))
	if err != nil {
		return false, fmt.Sprintf("rpc error: code = Unavailable desc = %v", err)
	}
	code, desc := resp.Header.Get("Grpc-Status"), resp.Header.Get("Grpc-Message")
	if code == "" {
		code, desc = resp.Trailer.Get("Grpc-Status"), resp.Trailer.Get("Grpc-Message")
	}
	if code == "" {
		return false, fmt.Sprintf("rpc error: no grpc-status in the response (HTTP %d)", resp.StatusCode)
	}
	if code != "0" {
		return false, fmt.Sprintf("rpc error: code = %s desc = %s", code, desc)
	}
	status, ok := healthStatus(body)
	if !ok {
		return false, "rpc error: malformed HealthCheckResponse"
	}
	if status != "SERVING" {
		return false, fmt.Sprintf("service unhealthy (responded with %q)", status)
	}
	return true, ""
}

// healthStatus decodes the status of a gRPC-framed HealthCheckResponse.
func healthStatus(body []byte) (string, bool) {
	if len(body) < 5 || body[0] != 0 {
		return "", false
	}
	n := int(binary.BigEndian.Uint32(body[1:5]))
	if len(body) < 5+n {
		return "", false
	}
	msg := body[5 : 5+n]
	var v uint64 // an absent field is the zero value, UNKNOWN
	for len(msg) > 0 {
		tag, k := binary.Uvarint(msg)
		if k <= 0 || tag&7 != 0 { // only varints appear in this message
			return "", false
		}
		msg = msg[k:]
		val, k := binary.Uvarint(msg)
		if k <= 0 {
			return "", false
		}
		msg = msg[k:]
		if tag>>3 == 1 {
			v = val
		}
	}
	names := []string{"UNKNOWN", "SERVING", "NOT_SERVING", "SERVICE_UNKNOWN"}
	if v < uint64(len(names)) {
		return names[v], true
	}
	return strconv.FormatUint(v, 10), true
}
