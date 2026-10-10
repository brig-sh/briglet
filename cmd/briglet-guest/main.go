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

//go:build linux

// Command briglet-guest runs inside a briglet pod VM. `run` supervises the
// pod's containers as the VM's main process; `exec` runs a command in one
// of them; `ctl` sends a command to the supervisor. See internal/guest.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/brig-sh/briglet/internal/guest"
)

const usage = `usage: briglet-guest <command>

commands:
  run [POD_JSON]                          supervise the pod (default ` + guest.PodFile + `)
  exec -c NAME [-u UID[:GID]] [-w DIR] [-e K=V]... -- ARGV...
                                          run a command in a running container
  ctl JSON                                send a request to the supervisor, for example
                                          '{"op":"restart","container":"app","grace":30}'
`

type envFlags []string

func (e *envFlags) String() string     { return fmt.Sprint(*e) }
func (e *envFlags) Set(v string) error { *e = append(*e, v); return nil }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case guest.ContainerCommand:
		guest.ContainerInit()
	case "run":
		pod := guest.PodFile
		if len(os.Args) > 2 {
			pod = os.Args[2]
		}
		if err := guest.Run(pod); err != nil {
			fmt.Fprintf(os.Stderr, "briglet-guest run: %v\n", err)
			os.Exit(1)
		}
	case "exec":
		fs := flag.NewFlagSet("exec", flag.ExitOnError)
		var opts guest.ExecOptions
		var env envFlags
		fs.StringVar(&opts.Container, "c", "", "container name")
		fs.StringVar(&opts.User, "u", "", "uid[:gid] to run as instead of the container's user")
		fs.StringVar(&opts.Cwd, "w", "", "working directory instead of the container's")
		fs.Var(&env, "e", "KEY=VALUE to add to the container's environment (repeatable)")
		_ = fs.Parse(os.Args[2:])
		opts.Env = env
		if opts.Container == "" || fs.NArg() == 0 {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		code, err := guest.Exec(opts, fs.Args())
		if err != nil {
			fmt.Fprintf(os.Stderr, "briglet-guest exec: %v\n", err)
			if code == 0 {
				code = 126
			}
		}
		os.Exit(code)
	case "ctl":
		if len(os.Args) != 3 {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		var req guest.CtlRequest
		if err := json.Unmarshal([]byte(os.Args[2]), &req); err != nil {
			fmt.Fprintf(os.Stderr, "briglet-guest ctl: %v\n", err)
			os.Exit(2)
		}
		if err := guest.Ctl(req); err != nil {
			fmt.Fprintf(os.Stderr, "briglet-guest ctl: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}
