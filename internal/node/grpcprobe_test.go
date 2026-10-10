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
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// healthServer answers Health/Check with the given status byte and
// records the request's service name.
func healthServer(t *testing.T, status byte, grpcStatus string, service *string) string {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if len(body) > 7 {
			*service = string(body[7:])
		}
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status")
		_, _ = w.Write([]byte{0, 0, 0, 0, 2, 0x08, status})
		w.Header().Set("Grpc-Status", grpcStatus)
	})
	srv := httptest.NewServer(h2c.NewHandler(h, &http2.Server{}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestGRPCHealthCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tests := []struct {
		name    string
		status  byte
		grpc    string
		service string
		ok      bool
		msg     string
	}{
		{"serving", 1, "0", "", true, ""},
		{"serving a named service", 1, "0", "db", true, ""},
		{"not serving", 2, "0", "", false, `service unhealthy (responded with "NOT_SERVING")`},
		{"unimplemented", 0, "12", "", false, "code = 12"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			addr := healthServer(t, tc.status, tc.grpc, &got)
			ok, msg := grpcHealthCheck(ctx, addr, addr, tc.service, false)
			if ok != tc.ok || !strings.Contains(msg, tc.msg) {
				t.Fatalf("got %v %q, want %v %q", ok, msg, tc.ok, tc.msg)
			}
			if tc.service != "" && got != tc.service {
				t.Fatalf("server saw service %q, want %q", got, tc.service)
			}
		})
	}
}

func TestGRPCHealthCheckRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if ok, msg := grpcHealthCheck(ctx, "127.0.0.1:1", "127.0.0.1:1", "", false); ok || !strings.Contains(msg, "Unavailable") {
		t.Fatalf("got %v %q, want an Unavailable failure", ok, msg)
	}
}
