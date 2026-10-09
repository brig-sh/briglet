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
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// apiserverClientCN is the subject of the client cert the apiserver uses
// for logs and exec. Only it may call the endpoints that reach into pods.
const apiserverClientCN = "kube-apiserver-kubelet-client"

// serveKubelet runs the kubelet HTTPS endpoint. Every client must present a
// certificate signed by the cluster CA; the pod endpoints further require
// the apiserver's kubelet client identity.
func serveKubelet(ctx context.Context, addr, certFile, keyFile, clientCAFile, nodeName string, pm *podManager) error {
	caPEM, err := os.ReadFile(clientCAFile)
	if err != nil {
		return fmt.Errorf("client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("client CA %s: no certificates", clientCAFile)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if errors.Is(err, os.ErrNotExist) {
		log.Printf("serving cert %s not found; using a self-signed cert, so apiserver-to-node calls will fail verification", certFile)
		cert, err = selfSigned(nodeName)
	}
	if err != nil {
		return fmt.Errorf("serving cert: %w", err)
	}
	s := &kubeletServer{pm: pm}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /pods", apiserverOnly(http.HandlerFunc(s.pods)))
	mux.Handle("GET /containerLogs/{ns}/{pod}/{container}", apiserverOnly(http.HandlerFunc(s.containerLogs)))
	mux.Handle("/exec/{ns}/{pod}/{container}", apiserverOnly(http.HandlerFunc(s.exec)))
	mux.Handle("/exec/{ns}/{pod}/{uid}/{container}", apiserverOnly(http.HandlerFunc(s.exec)))
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
			MinVersion:   tls.VersionTLS12,
		},
		// HTTP/1.1 only: exec upgrades the connection, which needs a hijack.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		ErrorLog:     log.New(os.Stderr, "kubelet-server: ", log.LstdFlags),
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("kubelet server listening on %s", addr)
	if err := srv.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// apiserverOnly rejects every client cert but the apiserver's kubelet
// client. The real kubelet asks the apiserver with a SubjectAccessReview;
// on a one-node cluster whose CA also signs the admin, controller and node
// certs, a fixed subject is the simpler rule.
func apiserverOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || r.TLS.PeerCertificates[0].Subject.CommonName != apiserverClientCN {
			cn := ""
			if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
				cn = r.TLS.PeerCertificates[0].Subject.CommonName
			}
			log.Printf("kubelet server: forbidden %s %s for %q", r.Method, r.URL.Path, cn)
			http.Error(w, fmt.Sprintf("Forbidden (user=%s, path=%s)", cn, r.URL.Path), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type kubeletServer struct {
	pm *podManager
}

func (s *kubeletServer) pods(w http.ResponseWriter, _ *http.Request) {
	list := corev1.PodList{
		TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"},
		Items:    s.pm.boundPods(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(&list)
}

// lookup finds the pod's worker and checks the container name.
func (s *kubeletServer) lookup(w http.ResponseWriter, r *http.Request) (*podWorker, podState, bool) {
	ns, name, container := r.PathValue("ns"), r.PathValue("pod"), r.PathValue("container")
	pw := s.pm.find(ns, name)
	if pw == nil {
		http.Error(w, fmt.Sprintf("pod %s/%s not found on this node", ns, name), http.StatusNotFound)
		return nil, podState{}, false
	}
	st := pw.snapshot()
	if container != st.Container {
		http.Error(w, fmt.Sprintf("container %s is not valid for pod %s", container, name), http.StatusNotFound)
		return nil, podState{}, false
	}
	if !st.Created {
		http.Error(w, fmt.Sprintf("container %q in pod %q is waiting to start", container, name), http.StatusBadRequest)
		return nil, podState{}, false
	}
	return pw, st, true
}

// kernelLine matches console output that is not the container's: kernel
// messages, the VMM's own lines, and hull's stop notice.
var kernelLine = regexp.MustCompile(`^(\[\s*\d+\.\d+\] |\[hvi\] |guest stopped: |booting \S+/boot-kernel with )`)

// lineFilter drops kernelLine lines from a stream and flushes after each
// write when the destination can flush.
type lineFilter struct {
	w       io.Writer
	partial []byte
	flush   func()
}

func (f *lineFilter) Write(p []byte) (int, error) {
	f.partial = append(f.partial, p...)
	for {
		i := bytes.IndexByte(f.partial, '\n')
		if i < 0 {
			break
		}
		line := f.partial[:i+1]
		if !kernelLine.Match(line) {
			if _, err := f.w.Write(line); err != nil {
				return 0, err
			}
		}
		f.partial = f.partial[i+1:]
	}
	if f.flush != nil {
		f.flush()
	}
	return len(p), nil
}

func (f *lineFilter) Close() {
	if len(f.partial) > 0 && !kernelLine.Match(f.partial) {
		_, _ = f.w.Write(f.partial)
	}
	f.partial = nil
}

// containerLogs serves the VM console through `hull logs`, without kernel
// and VMM lines. follow and tailLines are honored; timestamps, previous,
// sinceSeconds and limitBytes are ignored. With follow, tailLines counts
// console lines before the filter.
func (s *kubeletServer) containerLogs(w http.ResponseWriter, r *http.Request) {
	_, st, ok := s.lookup(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	follow := q.Get("follow") == "true" || q.Get("follow") == "1"
	tail := -1
	if v := q.Get("tailLines"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "tailLines must be a non-negative integer", http.StatusBadRequest)
			return
		}
		tail = n
	}
	w.Header().Set("Content-Type", "text/plain")
	if !follow {
		var buf bytes.Buffer
		f := &lineFilter{w: &buf}
		if err := s.pm.hull.Logs(r.Context(), st.Instance, false, -1, f); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.Close()
		_, _ = w.Write(lastN(buf.Bytes(), tail))
		return
	}
	fl, _ := w.(http.Flusher)
	f := &lineFilter{w: w}
	if fl != nil {
		f.flush = fl.Flush
		fl.Flush()
	}
	if err := s.pm.hull.Logs(r.Context(), st.Instance, true, tail, f); err != nil {
		log.Printf("logs %s: %v", st.Instance, err)
	}
	f.Close()
}

func lastN(b []byte, n int) []byte {
	if n < 0 {
		return b
	}
	if n == 0 {
		return nil
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var lines [][]byte
	for sc.Scan() {
		lines = append(lines, append(append([]byte(nil), sc.Bytes()...), '\n'))
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return bytes.Join(lines, nil)
}

func selfSigned(nodeName string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: nodeName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{nodeName, "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
