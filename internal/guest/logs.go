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
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// CRI log format: one line per record, `<time> <stream> <tag> <content>`,
// where tag F ends a line and P marks a piece of a longer one.
const (
	criTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"
	// maxLogLine is where a longer line is cut into P records, as
	// containerd does.
	maxLogLine = 16 << 10
	tailKeep   = 100
)

// logSink writes a container's stdout and stderr to its log file in the CRI
// format. A log left by the previous run moves to <path>.1 first.
type logSink struct {
	mu   sync.Mutex
	f    *os.File
	wg   sync.WaitGroup
	tail []string
	// partialOpen marks the last tail entry as waiting for more of a
	// line without a newline yet.
	partialOpen bool
}

func newLogSink(path string) (*logSink, error) {
	if _, err := os.Stat(path); err == nil {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	return &logSink{f: f}, nil
}

// Pump copies r into the log as the named stream until EOF.
func (l *logSink) Pump(r io.ReadCloser, stream string) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer r.Close()
		br := bufio.NewReaderSize(r, maxLogLine)
		for {
			line, err := br.ReadSlice('\n')
			switch {
			case errors.Is(err, bufio.ErrBufferFull):
				l.write(stream, "P", line)
				continue
			case len(line) > 0 && line[len(line)-1] == '\n':
				l.write(stream, "F", line[:len(line)-1])
			case len(line) > 0:
				l.write(stream, "P", line)
			}
			if err != nil {
				return
			}
		}
	}()
}

func (l *logSink) write(stream, tag string, content []byte) {
	var b bytes.Buffer
	b.WriteString(time.Now().UTC().Format(criTimeFormat))
	b.WriteString(" " + stream + " " + tag + " ")
	b.Write(content)
	b.WriteByte('\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.f.Write(b.Bytes())
	switch tag {
	case "F":
		l.tail = append(l.tail, string(content))
		l.partialOpen = false
	default:
		// A partial line joins the tail entry it belongs to, so the
		// fallback termination message sees output without a newline.
		if l.partialOpen && len(l.tail) > 0 {
			l.tail[len(l.tail)-1] += string(content)
		} else {
			l.tail = append(l.tail, string(content))
		}
		l.partialOpen = true
	}
	if len(l.tail) > tailKeep {
		l.tail = l.tail[len(l.tail)-tailKeep:]
	}
}

// Wait waits up to d for both streams to reach EOF.
func (l *logSink) Wait(d time.Duration) {
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(max(d, 0)):
	}
}

// Tail returns the last lines of output, at most n lines and max bytes,
// which is what the kubelet keeps for FallbackToLogsOnError.
func (l *logSink) Tail(n, maxBytes int) string {
	l.Wait(time.Second)
	l.mu.Lock()
	defer l.mu.Unlock()
	lines := l.tail
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	s := strings.Join(lines, "\n")
	if len(s) > maxBytes {
		s = s[len(s)-maxBytes:]
	}
	return s
}

// LogRecord is one parsed line of a CRI log file.
type LogRecord struct {
	Time    time.Time
	Stream  string
	Partial bool
	Content []byte
}

// ParseLogLine parses one CRI log line without its newline.
func ParseLogLine(line []byte) (LogRecord, bool) {
	var r LogRecord
	ts, rest, ok := bytes.Cut(line, []byte{' '})
	if !ok {
		return r, false
	}
	t, err := time.Parse(time.RFC3339Nano, string(ts))
	if err != nil {
		return r, false
	}
	stream, rest, ok := bytes.Cut(rest, []byte{' '})
	if !ok {
		return r, false
	}
	tag, content, _ := bytes.Cut(rest, []byte{' '})
	r.Time, r.Stream, r.Partial, r.Content = t, string(stream), string(tag) == "P", content
	return r, true
}
