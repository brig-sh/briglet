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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// storeMarker is the file hull writes at the root of a store it mounted.
const storeMarker = ".case-sensitive-apfs"

// resolveHull returns the absolute path of the hull binary from HULL_BIN or
// the default. The pid file check compares it against `ps -o comm=`.
func resolveHull() (string, error) {
	bin := HullBin()
	if !filepath.IsAbs(bin) {
		p, err := exec.LookPath(bin)
		if err != nil {
			return "", fmt.Errorf("hull binary %q: %w (set HULL_BIN)", bin, err)
		}
		bin = p
	}
	st, err := os.Stat(bin)
	if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		return "", fmt.Errorf("hull binary %s not found or not executable (set HULL_BIN)", bin)
	}
	return bin, nil
}

// checkGatewayServices makes sure the gateway routes Service addresses:
// the hull binary must know --service-cidr, and a gateway that already runs
// must have been started with it.
func (c *Cluster) checkGatewayServices(pid int) error {
	if pid != 0 {
		out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
		if err != nil {
			return fmt.Errorf("read the running gateway's arguments: %w", err)
		}
		if !strings.Contains(string(out), "--service-cidr") {
			return fmt.Errorf("the running gateway (pid %d) has no --service-cidr; run `briglet down` and `briglet up` with a HULL_BIN that supports it", pid)
		}
		return nil
	}
	out, _ := exec.Command(c.Hull.Bin, "network-gateway", "--help").CombinedOutput()
	if !strings.Contains(string(out), "--service-cidr") {
		return fmt.Errorf("%s network-gateway has no --service-cidr flag, which Services need; set HULL_BIN to a hull built from the gateway services branch", c.Hull.Bin)
	}
	return nil
}

// storeMounted reports whether hull has the store volume mounted.
func (c *Cluster) storeMounted() bool {
	_, err := os.Stat(filepath.Join(c.Paths.HullStore, storeMarker))
	return err == nil
}

// clearGatewaySockets removes sockets a gateway left behind when it died.
// It runs only when the pid file shows no gateway running.
func (c *Cluster) clearGatewaySockets() error {
	for _, s := range []string{c.Paths.GatewaySock, c.Paths.GatewaySock + ".qemu", c.Paths.GatewayAPI} {
		if err := os.Remove(s); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// removeInstances stops and removes every hull instance in the briglet
// store. It runs before the gateway stops, after the node agent.
func (c *Cluster) removeInstances(out io.Writer) error {
	if !c.storeMounted() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	list, err := c.Hull.Ps(ctx)
	if err != nil {
		return fmt.Errorf("list hull instances: %w", err)
	}
	var errs []error
	for _, in := range list {
		if err := c.Hull.StopAndRemove(ctx, in.ID, 5); err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Fprintf(out, "%-24s removed (was %s)\n", "hull "+in.ID, in.Status)
	}
	return errors.Join(errs...)
}

// detachStore unmounts the hull store volume so the state dir can be
// removed. It refuses when the volume stays mounted.
func (c *Cluster) detachStore(out io.Writer) error {
	if !c.storeMounted() {
		return nil
	}
	for _, args := range [][]string{{"detach", c.Paths.HullStore}, {"detach", "-force", c.Paths.HullStore}} {
		b, err := exec.Command("hdiutil", args...).CombinedOutput()
		if err == nil {
			fmt.Fprintf(out, "%-24s detached %s\n", "hull store", c.Paths.HullStore)
			return nil
		}
		if !c.storeMounted() {
			return nil
		}
		if args[1] == "-force" {
			return fmt.Errorf("detach hull store %s: %v: %s", c.Paths.HullStore, err, strings.TrimSpace(string(b)))
		}
		time.Sleep(time.Second)
	}
	return nil
}
