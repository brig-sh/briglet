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

package guest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveIn(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "etc", "real"), 0o755))
	must(os.MkdirAll(filepath.Join(root, "run", "systemd"), 0o755))
	must(os.Symlink("/run/systemd/resolv.conf", filepath.Join(root, "etc", "resolv.conf")))
	must(os.Symlink("real", filepath.Join(root, "etc", "rel")))
	must(os.Symlink("../../../../../outside", filepath.Join(root, "etc", "escape")))
	must(os.Symlink("loop", filepath.Join(root, "loop")))
	cases := map[string]string{
		"/etc/resolv.conf":  "/run/systemd/resolv.conf",
		"/etc/rel/x":        "/etc/real/x",
		"/etc/escape/y":     "/outside/y",
		"/missing/a/b":      "/missing/a/b",
		"etc/../etc/real/.": "/etc/real",
	}
	for in, want := range cases {
		got, err := resolveIn(root, in)
		if err != nil || got != filepath.Join(root, want) {
			t.Errorf("resolveIn(%q) = %q, %v; want %q", in, got, err, filepath.Join(root, want))
		}
	}
	if _, err := resolveIn(root, "/loop/x"); err == nil {
		t.Error("a symlink loop resolved")
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2"}, []string{"B=3", "C=4"})
	if want := []string{"A=1", "B=3", "C=4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("mergeEnv = %q", got)
	}
	if got := withPath([]string{"A=1"}); got[1] != "PATH="+defaultPath {
		t.Errorf("withPath = %q", got)
	}
}
