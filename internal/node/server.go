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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/brig-sh/briglet/internal/guest"
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

// lookup finds the pod's worker and the named container, an init
// container included.
func (s *kubeletServer) lookup(w http.ResponseWriter, r *http.Request) (*podWorker, podState, *corev1.Container, bool) {
	ns, name, container := r.PathValue("ns"), r.PathValue("pod"), r.PathValue("container")
	pw := s.pm.find(ns, name)
	if pw == nil {
		http.Error(w, fmt.Sprintf("pod %s/%s not found on this node", ns, name), http.StatusNotFound)
		return nil, podState{}, nil, false
	}
	pod, _ := pw.current()
	c := containerOf(pod, container)
	if c == nil {
		http.Error(w, fmt.Sprintf("container %s is not valid for pod %s", container, name), http.StatusNotFound)
		return nil, podState{}, nil, false
	}
	return pw, pw.snapshot(), c, true
}

// kernelLine matches console output that is not a container's: kernel
// messages, the VMM's own lines, and hull's stop notice.
var kernelLine = regexp.MustCompile(`^(\[\s*\d+\.\d+\] |\[hvi\] |guest stopped: |booting \S+/boot-kernel with )`)

// containerLogs serves a container's log file, which the guest supervisor
// writes in the CRI format into the pod's guest directory. follow,
// tailLines, timestamps, previous, sinceSeconds, sinceTime and limitBytes
// are honored as the kubelet honors them.
func (s *kubeletServer) containerLogs(w http.ResponseWriter, r *http.Request) {
	pw, st, c, ok := s.lookup(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	opts := logOptions{
		follow:     q.Get("follow") == "true" || q.Get("follow") == "1",
		timestamps: q.Get("timestamps") == "true" || q.Get("timestamps") == "1",
		tail:       -1,
		limit:      -1,
	}
	previous := q.Get("previous") == "true" || q.Get("previous") == "1"
	for name, dst := range map[string]*int64{"tailLines": &opts.tail, "limitBytes": &opts.limit, "sinceSeconds": nil} {
		v := q.Get(name)
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			http.Error(w, name+" must be a non-negative integer", http.StatusBadRequest)
			return
		}
		if dst != nil {
			*dst = n
		} else {
			opts.since = time.Now().Add(-time.Duration(n) * time.Second)
		}
	}
	if v := q.Get("sinceTime"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			http.Error(w, "sinceTime must be RFC 3339", http.StatusBadRequest)
			return
		}
		opts.since = t
	}
	path := filepath.Join(pw.podDirs().Guest, "logs", c.Name+".log")
	if previous {
		path += ".1"
		opts.follow = false
	}
	f, err := os.Open(path)
	if err != nil {
		switch {
		case previous:
			http.Error(w, fmt.Sprintf("previous terminated container %q in pod %q not found", c.Name, st.Name), http.StatusBadRequest)
		default:
			reason := "ContainerCreating"
			if st.Waiting != nil {
				reason = st.Waiting.Reason
			}
			http.Error(w, fmt.Sprintf("container %q in pod %q is waiting to start: %s", c.Name, st.Name, reason), http.StatusBadRequest)
		}
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain")
	running := func() bool {
		cur := pw.snapshot()
		g := cur.Guest.Find(c.Name)
		return cur.Running && g != nil && g.State == guest.StateRunning
	}
	if err := copyLogs(r.Context(), f, w, opts, running); err != nil {
		log.Printf("logs %s/%s %s: %v", st.Namespace, st.Name, c.Name, err)
	}
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
