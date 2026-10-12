# Re-recording the demo

The GIF at the top of the README is generated from `docs/demo.tape` with
[VHS](https://github.com/charmbracelet/vhs). To re-record it:

```sh
vhs docs/demo.tape    # records the cluster in ~/.briglet with hull from PATH
```

The recording in the repository comes from a throwaway cluster whose node
is named `mac`, so no machine name shows up:

```sh
bin/briglet down                      # stop your cluster, keep its state
export BRIGLET_STATE_DIR=~/.briglet-demo BRIGLET_NODE_NAME=mac
bin/briglet up
# Name the state directory with an Env line in a copy of the tape. It goes
# after the Set block: vhs ignores Set lines that follow another command.
awk -v sd="$BRIGLET_STATE_DIR" '!d && /^Hide/{print "Env BRIGLET_STATE_DIR \"" sd "\""; d=1} {print}' \
  docs/demo.tape > /tmp/demo.tape && vhs /tmp/demo.tape
bin/briglet down --wipe; unset BRIGLET_STATE_DIR BRIGLET_NODE_NAME
bin/briglet up                        # your cluster again
```

Add an `Env HULL_BIN "..."` line the same way when hull is not on PATH.

This writes `docs/assets/demo.gif` and `docs/assets/demo.mp4`.

## Prerequisites

- `vhs`, `ttyd`, `ffmpeg` and `tmux` on PATH (`brew install vhs tmux`).
- A running briglet cluster on this Mac. The tape reads its kubeconfig and
  Hull store from `BRIGLET_STATE_DIR` (default `~/.briglet`) and does not
  start or stop the cluster.
- At least one Ready node, and `nginx:alpine` and `busybox` pullable (set
  `BRIGLET_REGISTRY_MIRROR` before starting the cluster if Docker Hub is
  slow).
- `hull` reachable through `HULL_BIN` or PATH.
- No `demo` namespace; the tape deletes a stale one before it starts.

## What the tape does

The terminal runs tmux with two panes and no status bar. The top pane
follows the pod: first `kubectl get pods -n demo -w`, so Pending,
ContainerCreating and Running are visible, then `kubectl logs -f web`,
which shows the nginx entrypoint and one access-log line per request. The
bottom pane (60% of the height) is where the commands are typed. It lists
the nodes, runs the pod, runs `uname -a` in it, exposes it as a ClusterIP
Service and reads it from a client pod, exposes it as a NodePort and
curls it from the Mac, lists the Hull VMs, and deletes the namespace.
The screen is never cleared. It touches nothing outside `demo`.

Hidden setup only sets the environment, recreates `demo` and starts tmux.
Hidden time is not recorded.

## Timings and quirks

- A Service takes several seconds to start answering after `expose`. On
  the shared test cluster a client pod started 12 s after the ClusterIP
  expose got "connection refused" once, then worked. A NodePort needed
  about 10 s to answer in one early run and under 5 s in the recordings.
  The tape leaves 3.5 s before the client pod and 6 s for it to finish;
  if a step shows an error, re-record.
- `kubectl run --rm -it` does not work (attach returns 404), so the client
  pod runs `wget` through `sh -c` and its output is read with
  `kubectl logs`.
- The client pod's VM stays listed as `stopped` in `hull ps` until the
  namespace is deleted.

## The install recording

The GIF at the top of [docs/setup.md](setup.md) is generated from
`docs/install.tape`. It needs a second Apple silicon Mac with Homebrew,
the Xcode command line tools, the hull cask, and none of Go, kubectl or
briglet. The tape runs here and types into that Mac over SSH, so the
prompt, the paths and the node name are the only things that identify
it:

```sh
scp docs/install-shell.sh user@clean-mac:install-shell.sh
DEMO_HOST=user@clean-mac vhs docs/install.tape
```

`install-shell.sh` puts Homebrew on PATH, sets a plain `$ ` prompt and
names the node `mac`. The tape installs Go and kubectl with Homebrew,
clones briglet, runs `hack/fetch-prereqs.sh` and `make`, starts the
cluster, runs an nginx pod and compares `uname` on the Mac and in the
pod. A hidden last step runs `briglet down --wipe` and removes the clone;
Go, kubectl and `~/.local/share/briglet` stay on the second Mac.

The tape writes `install-raw.gif` and `install-raw.mp4`. The first pull of
`nginx:alpine` leaves the screen idle for about 50 s, so the committed
files cut that stretch, crop the empty bottom of the screen and hold the
last frame for 10 s. Find the cut points in the raw recording (when
`kubectl wait` is entered, and just before `condition met`), then:

```sh
CUT_FROM=56.5 CUT_TO=102.6
F="[0:v]trim=0:$CUT_FROM,setpts=PTS-STARTPTS[a];[0:v]trim=start=$CUT_TO,setpts=PTS-STARTPTS[b];[a][b]concat=n=2:v=1[c];[c]crop=1400:560:0:0,tpad=stop_mode=clone:stop_duration=10"
ffmpeg -i install-raw.mp4 -filter_complex "$F[out]" -map "[out]" \
  -c:v libx264 -pix_fmt yuv420p -crf 20 -movflags +faststart docs/assets/install.mp4
ffmpeg -i install-raw.gif -filter_complex \
  "$F,split[s0][s1];[s0]palettegen=max_colors=64[p];[s1][p]paletteuse=dither=none[out]" \
  -map "[out]" -loop 0 docs/assets/install.gif
```

VHS trims trailing spaces from the line it waits on, so the tape's
`WaitPattern` matches a bare `$`.
