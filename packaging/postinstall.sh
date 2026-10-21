#!/bin/sh
set -e
mkdir -p /etc/duod/traefik
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload || true
fi
echo "duod installed. Create /etc/duod/duod.yaml (see /etc/duod/duod.yaml.example) and /etc/duod/env, then: systemctl enable --now duod"
