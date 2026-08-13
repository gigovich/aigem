#!/bin/sh
# Start the aigem bot fleet behind `tailscale serve`, which terminates TLS for it
# on this node's MagicDNS name - so the fleet's UI is reachable from a phone at
# https://<machine>.<tailnet>.ts.net without a certificate, a port forward or a
# password of its own.
#
# This script exists because that name cannot be written into a shared unit file:
# it is per machine and per tailnet, and systemd cannot substitute the output of
# a command into ExecStart. So the name is derived here and passed to
# `aigem bot start --origin`, which is what the daemon matches every browser
# request's Origin header against.
#
#   run    derive the origin and exec the fleet          (ExecStart)
#   serve  put tailscale's TLS front door in front of it (ExecStartPost)
#
# Install alongside the unit:
#   cp deploy/systemd/aigem-bots-tailscale.sh ~/.local/bin/
#   chmod +x ~/.local/bin/aigem-bots-tailscale.sh
set -eu

# The loopback address the fleet listens on. Nothing outside this machine can
# reach it; tailscale is what carries the traffic in.
ADDR="${AIGEM_ADDR:-127.0.0.1:7777}"
AIGEM="${AIGEM_BIN:-$HOME/go/bin/aigem}"

die() {
	echo "aigem-bots-tailscale: $*" >&2
	exit 1
}

# cert_domain is the name tailscaled will issue a certificate for, which is
# exactly the name `tailscale serve` answers on.
#
# Not DNSName, which carries a trailing dot and is set even in a tailnet with
# HTTPS switched off. An empty CertDomains is therefore the check that HTTPS is
# available at all, and the one worth failing on: without it `tailscale serve`
# has no certificate to present and the URL would never load.
cert_domain() {
	status=$(tailscale status --json 2>/dev/null) ||
		die "tailscale is not reachable; is tailscaled running and this node logged in?"
	if command -v jq >/dev/null 2>&1; then
		printf '%s' "$status" | jq -r '.CertDomains[0] // empty'
	elif command -v python3 >/dev/null 2>&1; then
		printf '%s' "$status" |
			python3 -c 'import json,sys; print((json.load(sys.stdin).get("CertDomains") or [""])[0])'
	else
		die "need jq or python3 to read the tailnet name from \`tailscale status --json\`"
	fi
}

case "${1:-run}" in
run)
	domain=$(cert_domain)
	[ -n "$domain" ] || die "this tailnet issues no certificates, so there is no HTTPS name to
serve the fleet on. Enable it once in the admin console (DNS -> HTTPS Certificates),
then restart this unit."
	# AIGEM_BOTS is deliberately unquoted: it is a list of bot names, or empty
	# for the whole fleet, and word splitting is what turns it into arguments.
	# shellcheck disable=SC2086
	exec "$AIGEM" bot start --addr "$ADDR" --origin "https://$domain" ${AIGEM_BOTS:-}
	;;
serve)
	# Idempotent, so running it on every start costs nothing.
	#
	# It is deliberately not undone when the fleet stops. The front door is a
	# property of the machine rather than of this run, tailscaled persists it
	# across reboots by design, and the only removal this version of the CLI
	# offers - `tailscale serve reset` - would take every other service on the
	# node down with it. Remove it by hand if you ever want it gone.
	exec tailscale serve --bg --https=443 --yes "http://$ADDR"
	;;
*)
	die "usage: $0 [run|serve]"
	;;
esac
