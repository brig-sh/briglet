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
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
	utilexec "k8s.io/utils/exec"

	"github.com/brig-sh/briglet/internal/hull"
)

// spdyExecutor runs a command for remotecommand.ServeExec, which serves
// the SPDY exec protocol. Clients built on client-go's SPDY executor use
// it, Sonobuoy's `retrieve` among them.
type spdyExecutor struct {
	sock string
	env  []string
}

func (e spdyExecutor) ExecInContainer(ctx context.Context, _, _, container string, cmd []string,
	in io.Reader, out, errw io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize, _ time.Duration) error {
	req := hull.OpenRequest{Argv: cmd, Env: e.env, TTY: tty, Rows: 24, Cols: 80}
	if tty && resize != nil {
		select {
		case sz, ok := <-resize:
			if ok && sz.Height > 0 && sz.Width > 0 {
				req.Rows, req.Cols = sz.Height, sz.Width
			}
		case <-time.After(300 * time.Millisecond):
		}
	}
	sess, err := hull.DialAgent(ctx, e.sock, req)
	if err != nil {
		return err
	}
	defer sess.Close()
	if tty && resize != nil {
		go func() {
			for sz := range resize {
				_ = sess.Resize(sz.Height, sz.Width)
			}
		}()
	}
	if in != nil {
		go func() { _ = sess.CopyStdin(in) }()
	} else {
		_ = sess.CopyStdin(strings.NewReader(""))
	}
	var o, ew io.Writer
	if out != nil {
		o = out
	}
	if errw != nil {
		ew = errw
	}
	code, err := sess.Wait(o, ew)
	log.Printf("exec (spdy): %s %q exited %d", container, cmd, code)
	if err != nil {
		return err
	}
	if code != 0 {
		return utilexec.CodeExitError{Err: fmt.Errorf("exit status %d", code), Code: code}
	}
	return nil
}

// execSPDY serves an exec request that asks for a SPDY upgrade.
func execSPDY(w http.ResponseWriter, r *http.Request, sock, pod, uid, container string, argv, env []string) {
	opts, err := remotecommand.NewOptions(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	remotecommand.ServeExec(w, r, spdyExecutor{sock: sock, env: env}, pod, uid, container, argv, opts,
		execIdleTimeout, remotecommand.DefaultStreamCreationTimeout, remotecommand.SupportedStreamingProtocols)
}
