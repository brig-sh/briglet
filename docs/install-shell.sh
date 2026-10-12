#!/bin/bash
# Login shell for the clean-install recording: Homebrew on PATH, a plain
# prompt, and a neutral node name. Nothing else is preconfigured.
export PATH=/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin
export PS1='$ '
export BRIGLET_NODE_NAME=mac
export HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ENV_HINTS=1
cd ~
exec bash --norc --noprofile -i
