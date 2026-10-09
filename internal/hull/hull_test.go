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

package hull

import (
	"slices"
	"strings"
	"testing"
)

func TestRunArgsSharesAndSecretEnv(t *testing.T) {
	args := RunArgs(RunSpec{
		Name: "x", Image: "busybox", Pull: "missing", MemMB: 512, CPUs: 1,
		GatewaySock: "/s", GatewayCIDR: "10.87.0.10/24",
		Env:       []string{"PLAIN=visible"},
		SecretEnv: []string{"TOKEN=hidden-value"},
		Shares: []Share{
			{Host: "/h/cm", Guest: "/etc/cm", ReadOnly: true},
			{Host: "/h/tmp", Guest: "/tmp/x"},
		},
		Argv: []string{"sh", "-c", "true"},
	})
	line := strings.Join(args, " ")
	if strings.Contains(line, "hidden-value") {
		t.Fatalf("secret value on the command line: %s", line)
	}
	for _, want := range []string{"--env PLAIN=visible", "--env TOKEN ", "--shared-dir /h/cm:/etc/cm:ro", "--shared-dir /h/tmp:/tmp/x:rw"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
	dash := slices.Index(args, "--")
	if dash < 0 || slices.Index(args, "--shared-dir") > dash || args[dash+1] != "busybox" {
		t.Errorf("flags must precede -- and the image: %s", line)
	}
}
