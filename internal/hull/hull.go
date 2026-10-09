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

// Package hull drives the hull CLI and talks to the hull network gateway's
// HTTP API. It shells out to the binary and does not import hull packages.
// The one exception is the guest agent protocol in agent.go, which the
// kubelet exec endpoint speaks directly to get resize and stdin control.
package hull

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultBin is the hull the Homebrew cask installs, used when nothing
// else names one.
const DefaultBin = "/opt/homebrew/bin/hull"

// ErrNotFound is returned when hull has no instance under the name.
var ErrNotFound = errors.New("instance not found")

// Client runs hull against one store.
type Client struct {
	Bin      string
	StoreDir string
}

// Bin returns the hull binary to run: $HULL_BIN, else hull under binDir
// (the prerequisites directory, see cp.BinDir), else hull on PATH, else
// DefaultBin.
func Bin(binDir string) string {
	if b := os.Getenv("HULL_BIN"); b != "" {
		return b
	}
	if binDir != "" {
		if p := filepath.Join(binDir, "hull"); isExecutable(p) {
			return p
		}
	}
	if p, err := exec.LookPath("hull"); err == nil {
		return p
	}
	return DefaultBin
}

// isExecutable reports whether p is a regular file with an execute bit set.
func isExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

// New returns a client for the store at storeDir.
func New(bin, storeDir string) *Client {
	return &Client{Bin: bin, StoreDir: storeDir}
}

// GlobalArgs are the flags every hull invocation carries: no telemetry
// prompt, a recorded opt-out, and the briglet store.
func (c *Client) GlobalArgs() []string {
	return []string{"--unattended", "--dnt", "--store-dir", c.StoreDir}
}

func (c *Client) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Bin, append(c.GlobalArgs(), args...)...)
	// A killed hull can leave a VMM child holding the pipes open.
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// output runs hull and returns stdout. A failure carries hull's stderr.
func (c *Client) output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := c.command(ctx, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), wrapErr(args, err, errb.String())
	}
	return out.Bytes(), nil
}

func wrapErr(args []string, err error, stderr string) error {
	said := strings.TrimSpace(stderr)
	if strings.Contains(said, "instance not found") {
		return fmt.Errorf("hull %s: %w", args[0], ErrNotFound)
	}
	if said == "" {
		return fmt.Errorf("hull %s: %w", args[0], err)
	}
	return fmt.Errorf("hull %s: %w: %s", args[0], err, lastLines(said, 4))
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

// RunSpec is one `hull run --detach` invocation.
type RunSpec struct {
	Name        string
	Image       string
	Pull        string // missing, always or never
	MemMB       int
	CPUs        int
	GatewaySock string
	GatewayCIDR string
	StopGrace   int
	Env         []string // KEY=VALUE
	Argv        []string // full guest argv; empty keeps the image default
}

// RunArgs builds the run command line, without the global flags.
func RunArgs(s RunSpec) []string {
	args := []string{"run", "--detach", "--hypervisor", "hvi",
		"--name", s.Name,
		"--net", "shared",
		"--gateway-sock", s.GatewaySock,
		"--gateway-cidr", s.GatewayCIDR,
		"--wait-ip",
		"--mem", strconv.Itoa(s.MemMB),
		"--cpus", strconv.Itoa(s.CPUs),
		"--pull", s.Pull,
	}
	if s.StopGrace > 0 {
		args = append(args, "--stop-grace", strconv.Itoa(s.StopGrace))
	}
	for _, kv := range s.Env {
		args = append(args, "--env", kv)
	}
	// hull's flag parser reads flags after the image unless a -- ends them.
	args = append(args, "--", s.Image)
	return append(args, s.Argv...)
}

// Run boots an instance and waits until the guest holds its address. It
// returns the instance id hull prints.
func (c *Client) Run(ctx context.Context, s RunSpec) (string, error) {
	out, err := c.output(ctx, RunArgs(s)...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Instance is the subset of `hull inspect` briglet reads.
type Instance struct {
	ID            string    `json:"id"`
	ImageDigest   string    `json:"imageDigest"`
	PID           int       `json:"pid"`
	Status        string    `json:"status"`
	StartTime     time.Time `json:"startTime"`
	LogFile       string    `json:"logFile"`
	CmdLine       []string  `json:"cmdLine"`
	IP            string    `json:"ip"`
	ExitCode      *int      `json:"exitCode"`
	ExitedAt      time.Time `json:"exitedAt"`
	StoppedByUser bool      `json:"stoppedByUser"`
	Backend       string    `json:"backend"`
}

// Inspect returns the instance record. It does not check that the VMM is
// alive; see Alive.
func (c *Client) Inspect(ctx context.Context, id string) (*Instance, error) {
	out, err := c.output(ctx, "inspect", id)
	if err != nil {
		return nil, err
	}
	var in Instance
	if err := json.Unmarshal(out, &in); err != nil {
		return nil, fmt.Errorf("hull inspect %s: %w", id, err)
	}
	return &in, nil
}

// Alive reports whether the instance's VMM process still runs. The pid alone
// can be recycled, so the process must also run the VMM binary hull recorded.
func (in *Instance) Alive() bool {
	if in.PID <= 1 {
		return false
	}
	if err := syscall.Kill(in.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(in.PID), "-o", "stat=", "-o", "comm=").Output()
	if err != nil {
		return false
	}
	stat, comm, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if strings.HasPrefix(stat, "Z") {
		return false
	}
	if len(in.CmdLine) == 0 {
		return true
	}
	return filepath.Base(strings.TrimSpace(comm)) == filepath.Base(in.CmdLine[0])
}

// PsEntry is one row of `hull ps`.
type PsEntry struct {
	ID     string
	Status string
	PID    int
	IP     string
}

// Ps lists every instance in the store, stopped ones included.
func (c *Client) Ps(ctx context.Context) ([]PsEntry, error) {
	out, err := c.output(ctx, "ps")
	if err != nil {
		return nil, err
	}
	var list []PsEntry
	for i, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || (i == 0 && f[0] == "ID") || strings.HasPrefix(line, "No instances") {
			continue
		}
		e := PsEntry{ID: f[0], Status: f[1]}
		// Columns: ID STATUS EXIT PID IP CREATED; PID may be blank.
		for _, x := range f[2:] {
			if n, err := strconv.Atoi(x); err == nil && n > 1 && e.PID == 0 && x != "-" {
				e.PID = n
			} else if strings.Count(x, ".") == 3 {
				e.IP = x
			}
		}
		list = append(list, e)
	}
	return list, nil
}

// Stop asks the guest to shut down and kills the VMM after timeout seconds.
func (c *Client) Stop(ctx context.Context, id string, timeout int) error {
	_, err := c.output(ctx, "stop", "--timeout", strconv.Itoa(timeout), id)
	return err
}

// Rm removes an instance. force stops a running one first.
func (c *Client) Rm(ctx context.Context, id string, force bool) error {
	args := []string{"rm"}
	if force {
		args = append(args, "--force")
	}
	_, err := c.output(ctx, append(args, id)...)
	return err
}

// StopAndRemove stops the instance with the grace period and removes it. A
// missing instance is not an error.
func (c *Client) StopAndRemove(ctx context.Context, id string, grace int) error {
	// A failed stop falls through to rm --force, which stops what stop
	// could not.
	_ = c.Stop(ctx, id, grace)
	if err := c.Rm(ctx, id, true); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// Exec runs argv in the guest through `hull exec` and returns its exit code.
// err is set only when hull itself failed.
func (c *Client) Exec(ctx context.Context, id string, env, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	args := []string{"exec"}
	for _, kv := range env {
		args = append(args, "--env", kv)
	}
	args = append(args, id, "--")
	args = append(args, argv...)
	cmd := c.command(ctx, args...)
	cmd.Stdin = stdin
	var errb bytes.Buffer
	cmd.Stdout = stdout
	if stderr != nil {
		cmd.Stderr = io.MultiWriter(stderr, &errb)
	} else {
		cmd.Stderr = &errb
	}
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && !strings.Contains(errb.String(), "error: ") {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, wrapErr(args, err, errb.String())
	}
	return 0, nil
}

// Logs writes the instance console log to w. tail < 0 means everything.
func (c *Client) Logs(ctx context.Context, id string, follow bool, tail int, w io.Writer) error {
	args := []string{"logs"}
	if follow {
		args = append(args, "--follow")
	}
	args = append(args, "--tail", strconv.Itoa(tail), id)
	cmd := c.command(ctx, args...)
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return wrapErr(args, err, errb.String())
	}
	return nil
}

// Pull fetches an image into the store and returns its manifest digest.
func (c *Client) Pull(ctx context.Context, ref string) (string, error) {
	out, err := c.output(ctx, "pull", ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if d, ok := strings.CutPrefix(strings.TrimSpace(line), "Digest: "); ok {
			return strings.TrimSpace(d), nil
		}
	}
	return "", fmt.Errorf("hull pull %s: no digest in output", ref)
}

// AgentSocket is the unix socket the VMM bridges to the in-guest agent.
func (c *Client) AgentSocket(id string) string {
	return filepath.Join(c.StoreDir, "instances", id, "agent.sock")
}

// Version returns the version hull reports, or "" when it cannot run.
func (c *Client) Version(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, c.Bin, "--version").Output()
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return ""
	}
	return strings.TrimPrefix(f[1], "v")
}
