#!/bin/sh
set -e

PROXY_UID=1337
PROXY_INBOUND_PORT=15006
PROXY_OUTBOUND_PORT=15001
APP_HEALTH_PORT=8080
KUBE_API_IP="${KUBERNETES_SERVICE_HOST:-10.96.0.1}"

# Create chains
iptables -t nat -N ANANSE_PROXY_INBOUND
iptables -t nat -N ANANSE_PROXY_OUTBOUND

# == INBOUND (PREROUTING - external traffic entering pod) ==
# Exclusions first (order matters: checked top-to-bottom)
iptables -t nat -A ANANSE_PROXY_INBOUND -p tcp --dport $APP_HEALTH_PORT -j RETURN # K8s probes
iptables -t nat -A ANANSE_PROXY_INBOUND -p tcp --dport $PROXY_INBOUND_PORT -j RETURN # dont redirect proxy port

# Redirect everything else to inbound listener
iptables -t nat -A ANANSE_PROXY_INBOUND -p tcp -j REDIRECT --to-ports $PROXY_INBOUND_PORT

# Activate
iptables -t nat -A PREROUTING -p tcp -j ANANSE_PROXY_INBOUND

# == OUTBOUND (OUTPUT - app-generated traffic leaving pod) ==
# Loop prevention (CRITICAL - proxy's own traffic must pass through)
iptables -t nat -A ANANSE_PROXY_OUTBOUND -m owner --uid-owner $PROXY_UID -j RETURN

# Localhost bypass (app talking to itself)
iptables -t nat -A ANANSE_PROXY_OUTBOUND -d 127.0.0.1/32 -j RETURN

# K8s API bypass
iptables -t nat -A ANANSE_PROXY_OUTBOUND -d $KUBE_API_IP -j RETURN

# Redirect everything else to outbound listener
iptables -t nat -A ANANSE_PROXY_OUTBOUND -p tcp -j REDIRECT --to-ports $PROXY_OUTBOUND_PORT

# Activate
iptables -t nat -A OUTPUT -p tcp -j ANANSE_PROXY_OUTBOUND

# Debug output
iptables -t nat -L -v
echo "Ananse iptables rules applied."
