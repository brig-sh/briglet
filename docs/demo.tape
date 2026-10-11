# Re-record with: vhs docs/demo.tape
# Needs a running cluster and KUBECONFIG pointing at it (see docs/demo.md).

Output docs/assets/demo.gif
Output docs/assets/demo.mp4

Set Shell "bash"
Set FontSize 16
Set Width 1400
Set Height 900
Set Padding 12
Set Theme "Catppuccin Mocha"
Set TypingSpeed 30ms
Set Framerate 15
Set PlaybackSpeed 1.0

# Hidden setup: environment, then tmux on its own socket (so a stale server
# cannot hand the panes an old environment) with a top pane for the pod and a
# bottom pane (60%) for commands.
Hide
Type "export B=${BRIGLET_STATE_DIR:-$HOME/.briglet}; export KUBECONFIG=$B/admin.kubeconfig SHELL=/bin/bash PS1='$ '" Enter
Type `hull() { ${HULL_BIN:-hull} --store-dir "$B/hull-store" "$@"; }; export -f hull` Enter
Type "kubectl delete ns demo --ignore-not-found >/dev/null && kubectl create ns demo >/dev/null" Enter
Type `tmux -L briglet-demo -f /dev/null new-session "env PS1='\$ ' bash --norc" \; set -g status off \; set -g pane-border-status top \; set -g pane-border-format ' #{pane_title} ' \; select-pane -T 'pod logs' \; split-window -v -p 60 "env PS1='\$ ' bash --norc" \; select-pane -T 'commands'` Enter
Sleep 2s
Show

# 1. The node is this Mac.
Type `kubectl get nodes -o custom-columns=NAME:.metadata.name,OS-IMAGE:.status.nodeInfo.osImage` Enter
Sleep 2s

# 2. Watch pods in the top pane, then start the pod from the bottom one.
Ctrl+B
Up
Sleep 300ms
Type "kubectl get pods -n demo -w" Enter
Ctrl+B
Down
Sleep 300ms
Type "kubectl run web --image=nginx:alpine --port=80 -n demo" Enter
Sleep 3s
Ctrl+B
Up
Sleep 300ms
Ctrl+C
Type "kubectl logs -n demo -f web" Enter
Sleep 1s
Ctrl+B
Down
Sleep 300ms

# 3. The pod has its own Linux kernel.
Type "kubectl exec -n demo web -- uname -a" Enter
Sleep 2s

# 4. A ClusterIP service and a client pod; the request lands in the log.
Type "kubectl expose pod web -n demo --port 80" Enter
Sleep 3.5s
Type `kubectl run client -n demo --image=busybox --restart=Never -- sh -c 'wget -qO- http://web | head -4'` Enter
Sleep 6s
Type "kubectl logs -n demo client" Enter
Sleep 2.5s

# 5. A NodePort, reached from the Mac.
Type "kubectl expose pod web -n demo --name web-np --port 80 --type NodePort" Enter
Sleep 1s
Type "kubectl get svc -n demo web-np" Enter
Sleep 1.5s
Type "NP=$(kubectl get svc -n demo web-np -o jsonpath='{.spec.ports[0].nodePort}')" Enter
Sleep 500ms
Type "curl -s 127.0.0.1:$NP | grep -i nginx | head -3" Enter
Sleep 2.5s

# 6. The microVM behind the pod.
Type "hull ps | grep -e STATUS -e demo_" Enter
Sleep 3s

# 7. Clean up.
Type "kubectl delete ns demo" Enter
Sleep 3s

# Hidden teardown: the private tmux server would otherwise outlive the
# recording and hand its environment to the next one.
Hide
Type "tmux -L briglet-demo kill-server" Enter
