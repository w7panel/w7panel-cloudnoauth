#!/bin/sh
set -eu

/usr/local/bin/iptables-setup

runtime_uid="${SIDECAR_RUNTIME_UID:-1337}"
exec su-exec "${runtime_uid}:${runtime_uid}" "$@"
