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

package cp

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 365 * 24 * time.Hour
	// Leaf certs closer than this to expiry are reissued on `up`.
	renewBefore = 30 * 24 * time.Hour
)

// CertSpec describes one leaf certificate issued by the cluster CA.
type CertSpec struct {
	Name   string // file basename under pki/
	CN     string
	Orgs   []string
	DNS    []string
	IPs    []net.IP
	Server bool
	Client bool
}

// CA is the cluster certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// LeafSpecs returns every leaf certificate the cluster needs.
func LeafSpecs(nodeName string) []CertSpec {
	loopback := []net.IP{net.ParseIP("127.0.0.1")}
	return []CertSpec{
		{
			Name: "apiserver", CN: "kube-apiserver", Server: true,
			DNS: []string{"localhost", "kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local"},
			// The LAN addresses let node agents on other hosts verify the
			// apiserver when they join.
			IPs: append([]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP(ServiceAPIServerIP), net.ParseIP(GatewayIP)}, hostIPv4s()...),
		},
		{Name: "apiserver-kubelet-client", CN: "kube-apiserver-kubelet-client", Client: true},
		{Name: "apiserver-etcd-client", CN: "kube-apiserver-etcd-client", Client: true},
		// etcd's gRPC gateway dials the server with the server cert, so it needs client usage too.
		{Name: "etcd-server", CN: "etcd", DNS: []string{"localhost"}, IPs: loopback, Server: true, Client: true},
		{Name: "admin", CN: "kubernetes-admin", Orgs: []string{"system:masters"}, Client: true},
		{Name: "controller-manager", CN: "system:kube-controller-manager", Client: true},
		{Name: "controller-manager-serving", CN: "kube-controller-manager", DNS: []string{"localhost"}, IPs: loopback, Server: true},
		{Name: "scheduler", CN: "system:kube-scheduler", Client: true},
		{Name: "scheduler-serving", CN: "kube-scheduler", DNS: []string{"localhost"}, IPs: loopback, Server: true},
		{Name: "node", CN: "system:node:" + nodeName, Orgs: []string{"system:nodes"}, Client: true},
		{
			Name: "kubelet-serving", CN: "system:node:" + nodeName, Orgs: []string{"system:nodes"}, Server: true,
			DNS: []string{nodeName, nodeName + ".local", "localhost"},
			// The LAN addresses let the apiserver dial this node's kubelet
			// from other hosts, as a multi-node cluster does.
			IPs: append([]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP(GatewayIP)}, hostIPv4s()...),
		},
	}
}

// EnsurePKI creates or reuses the CA, the leaf certs and the service account key pair.
func EnsurePKI(dir, nodeName string) (*CA, error) {
	ca, err := ensureCA(dir)
	if err != nil {
		return nil, err
	}
	for _, s := range LeafSpecs(nodeName) {
		if err := ensureLeaf(dir, ca, s); err != nil {
			return nil, fmt.Errorf("cert %s: %w", s.Name, err)
		}
	}
	if err := ensureSAKey(dir); err != nil {
		return nil, fmt.Errorf("service account key: %w", err)
	}
	return ca, nil
}

func ensureCA(dir string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if cert, key, err := loadPair(certPath, keyPath); err == nil {
		if time.Now().Add(renewBefore).Before(cert.NotAfter) {
			return &CA{Cert: cert, Key: key}, nil
		}
		return nil, fmt.Errorf("CA %s expires %s; run `briglet down --wipe`", certPath, cert.NotAfter)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("load CA: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "briglet-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	if err := writePair(certPath, keyPath, der, key); err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

func ensureLeaf(dir string, ca *CA, s CertSpec) error {
	certPath, keyPath := filepath.Join(dir, s.Name+".crt"), filepath.Join(dir, s.Name+".key")
	if cert, _, err := loadPair(certPath, keyPath); err == nil && leafMatches(cert, ca, s) {
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := ca.Sign(s, &key.PublicKey)
	if err != nil {
		return err
	}
	return writePair(certPath, keyPath, der, key)
}

// Sign issues a leaf certificate for pub according to s.
func (ca *CA) Sign(s CertSpec, pub *ecdsa.PublicKey) ([]byte, error) {
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: s.CN, Organization: s.Orgs},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(leafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		DNSNames:              s.DNS,
		IPAddresses:           s.IPs,
	}
	if s.Server {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	if s.Client {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
	}
	return x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.Key)
}

// leafMatches reports whether an existing cert can be kept: same CA, same
// subject and SANs, and not close to expiry. A renamed host gets new certs.
func leafMatches(c *x509.Certificate, ca *CA, s CertSpec) bool {
	if c.CheckSignatureFrom(ca.Cert) != nil || time.Now().Add(renewBefore).After(c.NotAfter) {
		return false
	}
	if c.Subject.CommonName != s.CN || !slices.Equal(c.Subject.Organization, s.Orgs) || !slices.Equal(c.DNSNames, s.DNS) {
		return false
	}
	if len(c.IPAddresses) != len(s.IPs) {
		return false
	}
	for i := range s.IPs {
		if !c.IPAddresses[i].Equal(s.IPs[i]) {
			return false
		}
	}
	return true
}

func ensureSAKey(dir string) error {
	keyPath, pubPath := filepath.Join(dir, "sa.key"), filepath.Join(dir, "sa.pub")
	if _, err := os.Stat(keyPath); err == nil {
		if _, err := os.Stat(pubPath); err == nil {
			return nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644)
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}

// hostIPv4s lists this Mac's routable IPv4 addresses. Loopback and
// link-local addresses are left out.
func hostIPv4s() []net.IP {
	var out []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ip4)
		}
	}
	return out
}

func writePair(certPath, keyPath string, der []byte, key *ecdsa.PrivateKey) error {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func loadPair(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, nil, fmt.Errorf("%s or %s: no PEM data", certPath, keyPath)
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, fmt.Errorf("%s does not match %s", keyPath, certPath)
	}
	return cert, key, nil
}

// readPEM returns the raw contents of a PEM file, for embedding in kubeconfigs.
func readPEM(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b), nil
}
