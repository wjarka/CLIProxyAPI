#!/bin/sh
set -eu
# This init needs temporary NET_ADMIN/SETUID/SETGID/SETPCAP/CHOWN only.
# The private network is created by deployment; no host networking is supported.
iptables -w -P OUTPUT DROP
iptables -w -F OUTPUT
iptables -w -A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -w -A OUTPUT -o lo -j ACCEPT
# Do not let public DNS rebinding reach private, metadata, or non-unicast space.
for network in 0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12 192.0.0.0/24 192.0.2.0/24 192.168.0.0/16 192.88.99.0/24 198.18.0.0/15 198.51.100.0/24 203.0.113.0/24 224.0.0.0/4 240.0.0.0/4; do
    iptables -w -A OUTPUT -d "$network" -j REJECT
done
iptables -w -A OUTPUT -p tcp -m multiport --dports 80,443 -j ACCEPT
# Fail closed if IPv6 exists: no external IPv6 connections, including DNS.
if [ -e /proc/net/if_inet6 ]; then
    ip6tables -w -P OUTPUT DROP
    ip6tables -w -F OUTPUT
    ip6tables -w -A OUTPUT -o lo -j ACCEPT
fi
mkdir -p /run/console-browser
chown 0:0 /run/console-browser
chmod 0700 /run/console-browser
chown 65532:65532 /run/console-browser
exec setpriv --reuid=65532 --regid=65532 --clear-groups --inh-caps=-all --ambient-caps=-all --bounding-set=-all --no-new-privs /usr/bin/tini -- "$@"
