#!/bin/sh
# Replace the installed amd64 binary with the latest GitHub release and restart.
set -eu
repo="${REPO:-rshun/grok-tg-bot}"
dest="${DEST:-/usr/local/bin/grok-tg-bot}"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
gh release download --repo "$repo" --pattern grok-tg-bot --output "$tmp"
install -m 0755 "$tmp" "$dest"
systemctl restart grok-tg-bot
systemctl --no-pager --lines=20 status grok-tg-bot
