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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/brig-sh/briglet/internal/cp"
)

const usage = `usage: briglet <command> [flags]

commands:
  up              start etcd and the control plane
  down [--wipe]   stop everything; --wipe also removes the state dir
  status          show component pids and health
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
	fmt.Printf("briglet up took %.2fs\n", time.Since(start).Seconds())
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
