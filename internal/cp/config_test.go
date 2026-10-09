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
	"os"
	"strings"
	"testing"
)

func TestNodeNameFromEnv(t *testing.T) {
	t.Setenv("BRIGLET_NODE_NAME", "mac")
	got, err := NodeName()
	if err != nil || got != "mac" {
		t.Fatalf("NodeName() = %q, %v; want mac", got, err)
	}
}

func TestNodeNameRejectsInvalid(t *testing.T) {
	t.Setenv("BRIGLET_NODE_NAME", "Not_A_Name")
	if _, err := NodeName(); err == nil {
		t.Fatal("NodeName() accepted an invalid name")
	}
}

func TestNodeNameFallsBackToHostname(t *testing.T) {
	t.Setenv("BRIGLET_NODE_NAME", "")
	h, err := os.Hostname()
	if err != nil {
		t.Skip(err)
	}
	want, _, _ := strings.Cut(strings.ToLower(h), ".")
	got, err := NodeName()
	if err != nil || got != want {
		t.Fatalf("NodeName() = %q, %v; want %q", got, err, want)
	}
}
