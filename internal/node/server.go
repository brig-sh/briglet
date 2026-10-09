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
		ErrorLog: log.New(os.Stderr, "kubelet-server: ", log.LstdFlags),
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
