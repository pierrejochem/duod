#!/bin/sh
set -e
# Only on real removal, not on upgrade (deb: "remove", rpm: "0").
case "$1" in
  remove|0)
    if [ -d /run/systemd/system ]; then
      systemctl disable --now duod || true
    fi
    ;;
esac
