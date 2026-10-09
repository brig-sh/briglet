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

// Command briglet runs a Kubernetes control plane natively on macOS with the
// Mac itself as the node.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/brig-sh/briglet/internal/cp"
	"github.com/brig-sh/briglet/internal/node"
)

const usage = `usage: briglet <command> [flags]

commands:
  up              start etcd, the control plane and the node agent
  down [--wipe]   stop everything; --wipe also removes the state dir
  status          show component pids and health
  node            run the node agent in the foreground
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "up":
		err = cmdUp(ctx)
	case "down":
		err = cmdDown(os.Args[2:])
	case "status":
		err = cmdStatus(ctx)
	case "node":
		err = cmdNode(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "briglet %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func cmdUp(ctx context.Context) error {
	start := time.Now()
	c, err := cp.NewCluster()
	if err != nil {
		return err
	}
	if err := c.Up(ctx, os.Stdout); err != nil {
		return err
	}
	fmt.Printf("briglet up took %.2fs; node %s is Ready\n", time.Since(start).Seconds(), c.NodeName)
	fmt.Printf("export KUBECONFIG=%s\n", c.Paths.AdminKubeconfig)
	return nil
}

func cmdDown(args []string) error {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	wipe := fs.Bool("wipe", false, "remove the state dir after stopping")
	_ = fs.Parse(args)
	c, err := cp.NewCluster()
	if err != nil {
		return err
	}
	return c.Down(os.Stdout, *wipe)
}

func cmdStatus(ctx context.Context) error {
	c, err := cp.NewCluster()
	if err != nil {
		return err
	}
	return c.Status(ctx, os.Stdout)
}

func cmdNode(ctx context.Context, args []string) error {
	p, err := cp.NewPaths()
	if err != nil {
		return err
	}
	name, err := cp.NodeName()
	if err != nil {
		return err
	}
	cfg := node.Config{
		StateDir:    p.Root,
		HullBin:     cp.HullBin(),
		HostIP:      cp.GatewayIP,
		HullStore:   p.HullStore,
		GatewaySock: p.GatewaySock,
		PodsDir:     p.Pods,
		IPAMFile:    p.IPAM,
		PodCIDR:     cp.ClusterCIDR,

		APIServiceIP:  cp.ServiceAPIServerIP,
		ServiceCIDR:   cp.ServiceCIDR,
		ClusterDNS:    cp.ClusterDNSIP,
		ClusterDomain: cp.ClusterDomain,
		Debug:         os.Getenv("BRIGLET_DEBUG") != "",
	}
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	fs.StringVar(&cfg.Kubeconfig, "kubeconfig", p.Kubeconfig("node"), "kubeconfig with the node's client credentials")
	fs.StringVar(&cfg.Name, "name", name, "Node name")
	fs.StringVar(&cfg.NodeIP, "node-ip", cp.NodeIP, "InternalIP reported for the node; the apiserver dials it for logs and exec")
	fs.IntVar(&cfg.Port, "port", cp.KubeletPort, "kubelet HTTPS port")
	fs.StringVar(&cfg.TLSCertFile, "tls-cert-file", filepath.Join(p.PKI, "kubelet-serving.crt"), "kubelet serving cert")
	fs.StringVar(&cfg.TLSKeyFile, "tls-private-key-file", filepath.Join(p.PKI, "kubelet-serving.key"), "kubelet serving key")
	fs.StringVar(&cfg.ClientCAFile, "client-ca-file", filepath.Join(p.PKI, "ca.crt"), "CA that signs accepted client certs")
	_ = fs.Parse(args)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	return node.Run(ctx, cfg)
}
