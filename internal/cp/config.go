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

// Package cp launches the briglet control plane as native macOS processes.
package cp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/brig-sh/briglet/internal/hull"
)

// Network facts shared by the control plane and the node agent.
const (
	GatewayIP          = "10.87.0.1"
	ClusterCIDR        = "10.87.0.0/24"
	ServiceCIDR        = "10.96.0.0/12"
	ServiceAPIServerIP = "10.96.0.1"
	// NodeIP is the node's InternalIP: the address the apiserver dials for
	// logs and exec. Loopback works offline. Pods reach the host at GatewayIP.
	NodeIP          = "127.0.0.1"
	APIServerPort   = 6443
	EtcdClientPort  = 2379
	EtcdPeerPort    = 2380
	EtcdMetricsPort = 2381
	KCMPort         = 10257
	SchedulerPort   = 10259
	KubeletPort     = 10250
	SAIssuer        = "https://kubernetes.default.svc.cluster.local"
	// EgressProxyAddr is where the node agent serves the apiserver's
	// cluster egress CONNECT proxy.
)

// Paths is the layout of the state directory.
type Paths struct {
	Root            string
	PKI             string
	Etcd            string
	Logs            string
	Run             string
	Kubeconfigs     string
	AdminKubeconfig string
	// HullStore is hull's --store-dir. hull mounts a case-sensitive APFS
	// image there, backed by HullStore + ".sparseimage".
	HullStore string
	// GatewaySock is the gateway control socket. hull derives the QEMU
	// stream socket (+".qemu", used by hvi) and `hull run` derives the API
	// socket (+".api") from it.
	GatewaySock string
	GatewayAPI  string
	// Pods holds one directory per pod UID with the agent's record of it.
	Pods string
	// IPAM is the pod address allocation file.
	IPAM string
}

// StateDir returns the briglet state directory: $BRIGLET_STATE_DIR or ~/.briglet.
func StateDir() (string, error) {
	if d := os.Getenv("BRIGLET_STATE_DIR"); d != "" {
		return filepath.Abs(d)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".briglet"), nil
}

// NewPaths returns the layout rooted at StateDir.
func NewPaths() (Paths, error) {
	root, err := StateDir()
	if err != nil {
		return Paths{}, err
	}
	return Paths{
		Root:            root,
		PKI:             filepath.Join(root, "pki"),
		Etcd:            filepath.Join(root, "etcd"),
		Logs:            filepath.Join(root, "logs"),
		Run:             filepath.Join(root, "run"),
		Kubeconfigs:     filepath.Join(root, "kubeconfig"),
		AdminKubeconfig: filepath.Join(root, "admin.kubeconfig"),
		HullStore:       filepath.Join(root, "hull-store"),
		GatewaySock:     filepath.Join(root, "gw.sock"),
		GatewayAPI:      filepath.Join(root, "gw.sock.api"),
		Pods:            filepath.Join(root, "pods"),
		IPAM:            filepath.Join(root, "ipam.json"),
	}, nil
}

// Mkdirs creates the state directory tree with owner-only permissions.
func (p Paths) Mkdirs() error {
	for _, d := range []string{p.Root, p.PKI, p.Etcd, p.Logs, p.Run, p.Kubeconfigs, p.Pods} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	// etcd refuses to start when its data dir is wider than 0700.
	return os.Chmod(p.Etcd, 0o700)
}

// Kubeconfig returns the path of a component kubeconfig.
func (p Paths) Kubeconfig(component string) string {
	return filepath.Join(p.Kubeconfigs, component+".kubeconfig")
}

// Binaries holds the control plane executable paths.
type Binaries struct {
	Etcd              string
	APIServer         string
	ControllerManager string
	Scheduler         string
}

// BinDir returns the directory briglet looks in for its prerequisites:
// $BRIGLET_BIN_DIR, else $XDG_DATA_HOME/briglet/bin, else
// ~/.local/share/briglet/bin. hack/fetch-prereqs.sh fills it.
func BinDir() (string, error) {
	if d := os.Getenv("BRIGLET_BIN_DIR"); d != "" {
		return filepath.Abs(d)
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "briglet", "bin"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "briglet", "bin"), nil
}

// HullBin returns the hull binary to run, resolved the way hull.Bin does
// against BinDir.
func HullBin() string {
	dir, _ := BinDir()
	return hull.Bin(dir)
}

// ResolveBinaries locates etcd, kube-apiserver, kube-controller-manager
// and kube-scheduler. BRIGLET_ETCD_BIN and BRIGLET_K8S_BIN_DIR name them
// outright; otherwise each is taken from BinDir, then from PATH.
func ResolveBinaries() (Binaries, error) {
	dir, err := BinDir()
	if err != nil {
		return Binaries{}, err
	}
	find := func(name, explicit string) string {
		if explicit != "" {
			return explicit
		}
		if p := filepath.Join(dir, name); executable(p) {
			return p
		}
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
		// Reported as missing below, at the place the script would put it.
		return filepath.Join(dir, name)
	}
	k8sDir := os.Getenv("BRIGLET_K8S_BIN_DIR")
	inK8sDir := func(name string) string {
		if k8sDir == "" {
			return ""
		}
		return filepath.Join(k8sDir, name)
	}
	b := Binaries{
		Etcd:              find("etcd", os.Getenv("BRIGLET_ETCD_BIN")),
		APIServer:         find("kube-apiserver", inK8sDir("kube-apiserver")),
		ControllerManager: find("kube-controller-manager", inK8sDir("kube-controller-manager")),
		Scheduler:         find("kube-scheduler", inK8sDir("kube-scheduler")),
	}
	var missing []string
	for _, f := range []string{b.Etcd, b.APIServer, b.ControllerManager, b.Scheduler} {
		if !executable(f) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return b, fmt.Errorf("control plane binaries not found or not executable: %s (run hack/fetch-prereqs.sh, or set BRIGLET_BIN_DIR, BRIGLET_K8S_BIN_DIR and BRIGLET_ETCD_BIN)", strings.Join(missing, ", "))
	}
	return b, nil
}

// executable reports whether p is a regular file with an execute bit set.
func executable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

// NodeName returns $BRIGLET_NODE_NAME, or the short hostname, lowercased.
// The name ends up in the node's certificates, so changing it on an
// existing state directory needs `briglet down --wipe`.
func NodeName() (string, error) {
	if n := os.Getenv("BRIGLET_NODE_NAME"); n != "" {
		if errs := validation.IsDNS1123Subdomain(n); len(errs) > 0 {
			return "", fmt.Errorf("BRIGLET_NODE_NAME %q: %s", n, strings.Join(errs, "; "))
		}
		return n, nil
	}
	h, err := os.Hostname()
	if err != nil {
		return "", err
	}
	h, _, _ = strings.Cut(h, ".")
	return strings.ToLower(h), nil
}
