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

package node

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/brig-sh/briglet/internal/guest"
)

type logOptions struct {
	follow     bool
	timestamps bool
	// tail is the number of records from the end, -1 for all. limit caps
	// the bytes written, -1 for no cap.
	tail  int64
	limit int64
	since time.Time
}

// logFollowPoll is how often a followed log is checked for new records.
const logFollowPoll = 200 * time.Millisecond

// copyLogs writes the records of a CRI log file to w as the kubelet does:
// each record's content, newline-terminated unless the record is partial,
// prefixed with its RFC 3339 time when timestamps are asked for. With
// follow it keeps reading until running reports the container stopped and
// nothing new arrived, or ctx ends.
func copyLogs(ctx context.Context, f *os.File, w io.Writer, opts logOptions, running func() bool) error {
	fl, _ := w.(http.Flusher)
	lw := &limitWriter{w: w, left: opts.limit}
	emit := func(line []byte) error {
		rec, ok := guest.ParseLogLine(line)
		if !ok || (!opts.since.IsZero() && rec.Time.Before(opts.since)) {
			return nil
		}
		if opts.timestamps {
			if _, err := lw.Write([]byte(rec.Time.Format(time.RFC3339Nano) + " ")); err != nil {
				return err
			}
		}
		if _, err := lw.Write(rec.Content); err != nil {
			return err
		}
		if !rec.Partial {
			_, err := lw.Write([]byte{'\n'})
			return err
		}
		return nil
	}
	br := bufio.NewReaderSize(f, 64<<10)
	var lines [][]byte
	var pending []byte
	// The first pass reads what is there, keeping the last tail records.
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			lines = append(lines, line[:len(line)-1])
			if opts.tail >= 0 && int64(len(lines)) > opts.tail {
				lines = lines[1:]
			}
		} else {
			pending = line
		}
		if err != nil {
			break
		}
	}
	for _, line := range lines {
		if err := emit(line); err != nil {
			return ignoreLimit(err)
		}
	}
	if fl != nil {
		fl.Flush()
	}
	if !opts.follow {
		return nil
	}
	for {
		line, err := br.ReadBytes('\n')
		pending = append(pending, line...)
		if len(pending) > 0 && pending[len(pending)-1] == '\n' {
			if err := emit(pending[:len(pending)-1]); err != nil {
				return ignoreLimit(err)
			}
			pending = nil
			if fl != nil {
				fl.Flush()
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if !running() {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(logFollowPoll):
		}
	}
}

var errLimit = errors.New("limitBytes reached")

func ignoreLimit(err error) error {
	if errors.Is(err, errLimit) {
		return nil
	}
	return err
}

// limitWriter stops after left bytes; left < 0 means no limit.
type limitWriter struct {
	w    io.Writer
	left int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.left < 0 {
		return l.w.Write(p)
	}
	if l.left == 0 {
		return 0, errLimit
	}
	if int64(len(p)) > l.left {
		n, _ := l.w.Write(p[:l.left])
		l.left = 0
		return n, errLimit
	}
	l.left -= int64(len(p))
	return l.w.Write(p)
}
