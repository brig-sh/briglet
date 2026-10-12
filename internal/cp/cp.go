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
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/brig-sh/briglet/internal/hull"
)

const readyTimeout = 90 * time.Second

// Component is one control plane process plus how to tell that it is ready.
type Component struct {
	*Proc
	Port int
	// Health probes the component once; nil means ready.
	Health func(ctx context.Context) error
	// BeforeStart runs before the process starts, when it is not running.
	BeforeStart func() error
	// BeforeStop runs before the process is stopped, whether it runs or not.
	BeforeStop func(out io.Writer) error
	// Preflight runs before the start or adoption of the process; pid is 0
	// when it does not run.
	Preflight func(pid int) error
	// AfterReady runs once the process is ready, on every `up`.
	AfterReady func(ctx context.Context, out io.Writer) error
}

// Cluster is the full set of components in start order.
type Cluster struct {
	Paths Paths
	// Self is the briglet executable; bin/briglet-guest sits next to it.
	Self       string
	NodeName   string
	Hull       *hull.Client
	Components []*Component
}

// NewCluster builds the component list. It does not start anything.
func NewCluster() (*Cluster, error) {
	p, err := NewPaths()
	if err != nil {
		return nil, err
	}
	name, err := NodeName()
	if err != nil {
		return nil, err
	}
	bins, err := ResolveBinaries()
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return nil, err
	}
	hullBin, err := resolveHull()
	if err != nil {
		return nil, err
	}
	pki := func(f string) string { return filepath.Join(p.PKI, f) }
	c := &Cluster{Paths: p, Self: self, NodeName: name, Hull: hull.New(hullBin, p.HullStore)}

	etcd := NewProc(p, "etcd", bins.Etcd,
		"--name=briglet",
		"--data-dir="+p.Etcd,
		fmt.Sprintf("--listen-client-urls=https://127.0.0.1:%d", EtcdClientPort),
		fmt.Sprintf("--advertise-client-urls=https://127.0.0.1:%d", EtcdClientPort),
		fmt.Sprintf("--listen-peer-urls=http://127.0.0.1:%d", EtcdPeerPort),
		fmt.Sprintf("--initial-advertise-peer-urls=http://127.0.0.1:%d", EtcdPeerPort),
		fmt.Sprintf("--initial-cluster=briglet=http://127.0.0.1:%d", EtcdPeerPort),
		fmt.Sprintf("--listen-metrics-urls=http://127.0.0.1:%d", EtcdMetricsPort),
		"--cert-file="+pki("etcd-server.crt"),
		"--key-file="+pki("etcd-server.key"),
		"--trusted-ca-file="+pki("ca.crt"),
		"--client-cert-auth=true",
	)
	apiserver := NewProc(p, "kube-apiserver", bins.APIServer,
		"--etcd-servers="+fmt.Sprintf("https://127.0.0.1:%d", EtcdClientPort),
		"--etcd-cafile="+pki("ca.crt"),
		"--etcd-certfile="+pki("apiserver-etcd-client.crt"),
		"--etcd-keyfile="+pki("apiserver-etcd-client.key"),
		"--bind-address=0.0.0.0",
		fmt.Sprintf("--secure-port=%d", APIServerPort),
		"--advertise-address="+GatewayIP,
		// Cluster-bound dials, such as admission webhooks and the service
		// proxy, go through the node agent's CONNECT proxy into the
		// gateway, where the service table routes them.
		"--egress-selector-config-file="+filepath.Join(p.Root, "egress-selector.yaml"),
		"--tls-cert-file="+pki("apiserver.crt"),
		"--tls-private-key-file="+pki("apiserver.key"),
		"--client-ca-file="+pki("ca.crt"),
		"--service-cluster-ip-range="+ServiceCIDR,
		"--authorization-mode=Node,RBAC",
		"--enable-admission-plugins=NodeRestriction",
		"--enable-bootstrap-token-auth=true",
		"--service-account-issuer="+SAIssuer,
		// Every pod is a VM, so a privileged container stays inside its
		// guest; the guest supervisor honors the flag.
		"--allow-privileged=true",
		// The discovery document would otherwise name the advertise address,
		// 10.87.0.1:6443, which no pod can dial: the apiserver runs on the
		// Mac and only the kubernetes Service reaches it.
		"--service-account-jwks-uri="+SAIssuer+"/openid/v1/jwks",
		"--service-account-key-file="+pki("sa.pub"),
		"--service-account-signing-key-file="+pki("sa.key"),
		"--kubelet-client-certificate="+pki("apiserver-kubelet-client.crt"),
		"--kubelet-client-key="+pki("apiserver-kubelet-client.key"),
		"--kubelet-certificate-authority="+pki("ca.crt"),
		// The node's InternalIP is 127.0.0.1, so logs and exec work without
		// the LAN. 10.87.0.1 lives inside the hull gateway netstack, not on
		// a host interface.
		"--kubelet-preferred-address-types=InternalIP",
	)
	kcmKC := p.Kubeconfig("controller-manager")
	kcm := NewProc(p, "kube-controller-manager", bins.ControllerManager,
		"--kubeconfig="+kcmKC,
		"--authentication-kubeconfig="+kcmKC,
		"--authorization-kubeconfig="+kcmKC,
		"--bind-address=127.0.0.1",
		fmt.Sprintf("--secure-port=%d", KCMPort),
		"--tls-cert-file="+pki("controller-manager-serving.crt"),
		"--tls-private-key-file="+pki("controller-manager-serving.key"),
		"--controllers=*,bootstrapsigner,tokencleaner",
		"--allocate-node-cidrs=true",
		"--cluster-cidr="+ClusterCIDR,
		// One /24 pod network behind the gateway. Every node's IPAM gets
		// half of it, so two agents can share one gateway.
		"--node-cidr-mask-size=25",
		"--service-cluster-ip-range="+ServiceCIDR,
		"--cluster-signing-cert-file="+pki("ca.crt"),
		"--cluster-signing-key-file="+pki("ca.key"),
		"--root-ca-file="+pki("ca.crt"),
		"--service-account-private-key-file="+pki("sa.key"),
		"--use-service-account-credentials=true",
		"--leader-elect=false",
		// The default lives under /usr/libexec, which is read-only on macOS.
		"--flex-volume-plugin-dir="+filepath.Join(p.Root, "volume-plugins"),
	)
	schedKC := p.Kubeconfig("scheduler")
	sched := NewProc(p, "kube-scheduler", bins.Scheduler,
		"--kubeconfig="+schedKC,
		"--authentication-kubeconfig="+schedKC,
		"--authorization-kubeconfig="+schedKC,
		"--bind-address=127.0.0.1",
		fmt.Sprintf("--secure-port=%d", SchedulerPort),
		"--tls-cert-file="+pki("scheduler-serving.crt"),
		"--tls-private-key-file="+pki("scheduler-serving.key"),
		"--leader-elect=false",
	)
	gateway := NewProc(p, "gateway", hullBin, append(c.Hull.GlobalArgs(), "network-gateway",
		"--socket", p.GatewaySock,
		"--api", p.GatewayAPI,
		"--subnet", ClusterCIDR,
		"--gateway-ip", GatewayIP,
		"--service-cidr", ServiceCIDR,
	)...)
	if ba := os.Getenv("HULL_BOOT_ASSETS"); ba != "" {
		// The gateway's hull resolves the same boot assets as the node's.
		gateway.Env = append(gateway.Env, "HULL_BOOT_ASSETS="+ba)
	}
	gwAPI := hull.NewGateway(p.GatewayAPI)
	// The node reports a LAN address when one exists, so other nodes and
	// the Service stitching reach its kubelet; loopback keeps a
	// network-isolated Mac working.
	nodeIP, err := LANAddr()
	if err != nil {
		return nil, err
	}
	node := NewProc(p, "node", self, "node",
		"--kubeconfig="+p.Kubeconfig("node"),
		"--name="+name,
		"--node-ip="+nodeIP,
		"--tls-cert-file="+pki("kubelet-serving.crt"),
		"--tls-private-key-file="+pki("kubelet-serving.key"),
		"--client-ca-file="+pki("ca.crt"),
	)

	c.Components = []*Component{
		{Proc: etcd, Port: EtcdClientPort, Health: func(ctx context.Context) error {
			body, err := c.get(ctx, fmt.Sprintf("https://127.0.0.1:%d/health", EtcdClientPort), "apiserver-etcd-client")
			if err == nil && !strings.Contains(body, `"health":"true"`) {
				err = fmt.Errorf("etcd /health: %s", body)
			}
			return err
		}},
		{Proc: apiserver, Port: APIServerPort, Health: func(ctx context.Context) error {
			_, err := c.get(ctx, fmt.Sprintf("https://127.0.0.1:%d/readyz", APIServerPort), "admin")
			return err
		}, AfterReady: c.ApplyAddons},
		{Proc: kcm, Port: KCMPort, Health: func(ctx context.Context) error {
			_, err := c.get(ctx, fmt.Sprintf("https://127.0.0.1:%d/healthz", KCMPort), "")
			return err
		}},
		{Proc: sched, Port: SchedulerPort, Health: func(ctx context.Context) error {
			_, err := c.get(ctx, fmt.Sprintf("https://127.0.0.1:%d/healthz", SchedulerPort), "")
			return err
		}},
		{Proc: gateway, Health: gwAPI.Healthy, BeforeStart: c.clearGatewaySockets, BeforeStop: c.removeInstances,
			Preflight: c.checkGatewayServices},
		{Proc: node, Port: KubeletPort, Preflight: c.prepareGuestBinary, Health: func(ctx context.Context) error {
			if _, err := c.get(ctx, fmt.Sprintf("https://127.0.0.1:%d/healthz", KubeletPort), "apiserver-kubelet-client"); err != nil {
				return err
			}
			return c.nodeReady(ctx)
		}},
	}
	return c, nil
}

// get performs an HTTPS GET verified against the cluster CA, presenting the
// client cert pki/<clientCert>.{crt,key} when clientCert is not empty.
func (c *Cluster) get(ctx context.Context, url, clientCert string) (string, error) {
	caPEM, err := os.ReadFile(filepath.Join(c.Paths.PKI, "ca.crt"))
	if err != nil {
		return "", err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if clientCert != "" {
		kp, err := tls.LoadX509KeyPair(filepath.Join(c.Paths.PKI, clientCert+".crt"), filepath.Join(c.Paths.PKI, clientCert+".key"))
		if err != nil {
			return "", err
		}
		cfg.Certificates = []tls.Certificate{kp}
	}
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return string(b), fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(b)))
	}
	return string(b), nil
}

// Clientset returns an admin client for the cluster.
func (c *Cluster) Clientset() (*kubernetes.Clientset, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", c.Paths.AdminKubeconfig)
	if err != nil {
		return nil, err
	}
	cfg.Timeout = 5 * time.Second
	return kubernetes.NewForConfig(cfg)
}

func (c *Cluster) nodeReady(ctx context.Context) error {
	cs, err := c.Clientset()
	if err != nil {
		return err
	}
	n, err := cs.CoreV1().Nodes().Get(ctx, c.NodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	ready := false
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			if cond.Status != corev1.ConditionTrue {
				return fmt.Errorf("node %s Ready=%s: %s", c.NodeName, cond.Status, cond.Message)
			}
			ready = true
		}
	}
	if !ready {
		return fmt.Errorf("node %s has no Ready condition yet", c.NodeName)
	}
	// The node lifecycle controller taints a new node not-ready until it
	// observes the Ready condition. Wait for that so the node is schedulable.
	for _, t := range n.Spec.Taints {
		if t.Key == corev1.TaintNodeNotReady || t.Key == corev1.TaintNodeUnreachable {
			return fmt.Errorf("node %s still has taint %s", c.NodeName, t.Key)
		}
	}
	return nil
}

// Up starts every component that is not already running and waits for each
// to become ready before starting the next.
func (c *Cluster) Up(ctx context.Context, out io.Writer) error {
	if err := c.Paths.Mkdirs(); err != nil {
		return err
	}
	if _, err := EnsurePKI(c.Paths.PKI, c.NodeName); err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	if err := WriteKubeconfigs(c.Paths); err != nil {
		return err
	}
	if err := WriteEgressConfig(filepath.Join(c.Paths.Root, "egress-selector.yaml"), filepath.Join(c.Paths.Root, "egress.sock")); err != nil {
		return err
	}
	// Preflights run before anything starts, so a missing prerequisite
	// leaves no half-started cluster.
	for _, comp := range c.Components {
		if comp.Preflight != nil {
			if err := comp.Preflight(comp.Pid()); err != nil {
				return fmt.Errorf("%s: %w", comp.Name, err)
			}
		}
	}
	for _, comp := range c.Components {
		start := time.Now()
		state := "started"
		if comp.Pid() != 0 {
			state = "already running"
		} else {
			if comp.Port != 0 && portInUse(comp.Port) {
				return fmt.Errorf("%s: port %d is in use by a process briglet did not start", comp.Name, comp.Port)
			}
			if comp.BeforeStart != nil {
				if err := comp.BeforeStart(); err != nil {
					return fmt.Errorf("%s: %w", comp.Name, err)
				}
			}
			if err := comp.Start(); err != nil {
				return err
			}
		}
		if err := c.waitReady(ctx, comp); err != nil {
			return err
		}
		fmt.Fprintf(out, "%-24s ready in %5.2fs (pid %d, %s)\n", comp.Name, time.Since(start).Seconds(), comp.Pid(), state)
		if comp.AfterReady != nil {
			if err := comp.AfterReady(ctx, out); err != nil {
				return fmt.Errorf("%s: %w", comp.Name, err)
			}
		}
	}
	return nil
}

func (c *Cluster) waitReady(ctx context.Context, comp *Component) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var last error
	for {
		if last = comp.Health(ctx); last == nil {
			return nil
		}
		select {
		case <-comp.Exited():
			return fmt.Errorf("%s exited (%v) before becoming ready; last log lines from %s:\n%s",
				comp.Name, comp.ExitErr(), comp.LogFile(), comp.TailLog(20))
		case <-ctx.Done():
			return fmt.Errorf("%s not ready after %s: %v; see %s", comp.Name, readyTimeout, last, comp.LogFile())
		case <-tick.C:
		}
	}
}

// Down stops the components in reverse start order. With wipe it also
// removes the state directory.
func (c *Cluster) Down(out io.Writer, wipe bool) error {
	var errs []error
	for i := len(c.Components) - 1; i >= 0; i-- {
		comp := c.Components[i]
		if comp.BeforeStop != nil {
			if err := comp.BeforeStop(out); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		pid := comp.Pid()
		stopped, err := comp.Stop(15 * time.Second)
		switch {
		case err != nil:
			errs = append(errs, err)
		case stopped:
			fmt.Fprintf(out, "%-24s stopped (pid %d)\n", comp.Name, pid)
		default:
			fmt.Fprintf(out, "%-24s not running\n", comp.Name)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if wipe {
		if filepath.Base(c.Paths.Root) != ".briglet" && os.Getenv("BRIGLET_STATE_DIR") == "" {
			return fmt.Errorf("refusing to wipe unexpected state dir %s", c.Paths.Root)
		}
		if err := c.detachStore(out); err != nil {
			return err
		}
		if err := os.RemoveAll(c.Paths.Root); err != nil {
			return err
		}
		fmt.Fprintf(out, "wiped %s\n", c.Paths.Root)
	}
	return nil
}

// Status prints pid and health of every component. It returns an error when
// any component is down or unhealthy.
func (c *Cluster) Status(ctx context.Context, out io.Writer) error {
	bad := 0
	fmt.Fprintf(out, "%-24s %-8s %s\n", "COMPONENT", "PID", "HEALTH")
	for _, comp := range c.Components {
		pid := comp.Pid()
		health := "ok"
		if pid == 0 {
			health, bad = "not running", bad+1
		} else {
			hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := comp.Health(hctx); err != nil {
				health, bad = err.Error(), bad+1
			}
			cancel()
		}
		pidStr := "-"
		if pid != 0 {
			pidStr = strconv.Itoa(pid)
		}
		fmt.Fprintf(out, "%-24s %-8s %s\n", comp.Name, pidStr, health)
	}
	fmt.Fprintf(out, "state dir: %s\nlogs:      %s\n", c.Paths.Root, c.Paths.Logs)
	if bad > 0 {
		return fmt.Errorf("%d component(s) not healthy", bad)
	}
	return nil
}

func portInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
