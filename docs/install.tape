# Briglet from scratch on a Mac with no Go, no kubectl and no briglet.
# The tape runs on one Mac and types into a second one over SSH:
# DEMO_HOST names it, and docs/install-shell.sh must be in its home
# directory. Long builds run off camera (Hide/Wait/Show): the command is
# typed on camera, and the recording resumes at its finished output.
# docs/demo.md has the trim step that turns install-raw.* into
# docs/assets/install.*.

Output install-raw.gif
Output install-raw.mp4

Require ssh

Set Shell "bash"
Set FontSize 16
Set Width 1400
Set Height 860
Set Padding 12
Set Theme "Catppuccin Mocha"
Set TypingSpeed 30ms
Set Framerate 15
Set WaitPattern /\$$/
Set WaitTimeout 25m

Hide
Type "ssh -q -t $DEMO_HOST ./install-shell.sh" Enter
Wait
Type "clear" Enter
Wait
Show

Type "# A clean Apple silicon Mac: no Go, no kubectl, no briglet" Enter
Type "command -v go kubectl briglet || echo 'none installed'" Enter
Wait
Sleep 2s

Type "brew install -q go kubernetes-cli >/dev/null 2>&1" Sleep 600ms Enter
Hide
Wait
Show
Type "go version && kubectl version --client | head -1" Enter
Wait
Sleep 2s

Type "git clone -q https://github.com/brig-sh/briglet && cd briglet" Enter
Wait
Sleep 1s

Type "clear" Enter
Wait
Type "# etcd, the apiserver and kubectl, the controller manager and scheduler built for darwin, and hull" Enter
Type "hack/fetch-prereqs.sh 2>&1 | grep '^==>'" Sleep 600ms Enter
Hide
Wait
Show
Sleep 1s
Type "ls ~/.local/share/briglet/bin" Enter
Wait
Sleep 3s

Type "clear" Enter
Wait
Type "make 2>&1 | tail -2" Sleep 400ms Enter
Hide
Wait
Show
Sleep 1s

Type "export BRIGLET_REGISTRY_MIRROR=mirror.gcr.io" Enter
Wait
Type "bin/briglet up" Sleep 600ms Enter
Wait
Sleep 3s

Type "export KUBECONFIG=~/.briglet/admin.kubeconfig" Enter
Wait
Type "kubectl get nodes" Enter
Wait
Sleep 2s

Type "kubectl run web --image=nginx:alpine --port=80" Enter
Wait
Type "kubectl wait --for=condition=Ready pod/web --timeout=180s" Enter
Wait
Sleep 1s
Type "uname -sr; kubectl exec web -- uname -sr" Enter
Wait
Sleep 6s

Hide
Type "bin/briglet down --wipe >/dev/null 2>&1; cd ~ && rm -rf briglet; exit" Enter
Sleep 20s
