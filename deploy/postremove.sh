#!/bin/sh
set -e

case "$1" in
    remove|purge)
        systemctl stop slagworksd-site 2>/dev/null || true
        systemctl stop slagworksd-global 2>/dev/null || true
        systemctl disable slagworksd-site 2>/dev/null || true
        systemctl disable slagworksd-global 2>/dev/null || true
        systemctl daemon-reload
        ;;
esac
