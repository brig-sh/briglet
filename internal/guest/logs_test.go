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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogSink(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.log")
	if err := os.WriteFile(p, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := newLogSink(p)
	if err != nil {
		t.Fatal(err)
	}
	r, w := io.Pipe()
	l.Pump(r, "stdout")
	_, _ = w.Write([]byte("one\ntw"))
	_, _ = w.Write([]byte("o\n" + strings.Repeat("x", maxLogLine+10) + "\nend"))
	w.Close()
	l.Wait(2 * time.Second)
	if b, _ := os.ReadFile(p + ".1"); string(b) != "old\n" {
		t.Errorf("previous log = %q", b)
	}
	b, _ := os.ReadFile(p)
	var got []string
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		rec, ok := ParseLogLine([]byte(line))
		if !ok || rec.Stream != "stdout" || time.Since(rec.Time) > time.Minute {
			t.Fatalf("bad record %q", line)
		}
		tag := "F"
		if rec.Partial {
			tag = "P"
		}
		got = append(got, tag+":"+strings.ReplaceAll(string(rec.Content), strings.Repeat("x", 100), "X"))
	}
	want := []string{"F:one", "F:two", "P:" + strings.Repeat("X", 163) + strings.Repeat("x", 84), "F:" + strings.Repeat("x", 10), "P:end"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("records\n got %q\nwant %q", got, want)
	}
	if tail := l.Tail(1, 10); tail != "end" {
		t.Errorf("Tail = %q", tail)
	}
}
