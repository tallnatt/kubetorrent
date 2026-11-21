#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

read -rp "Domain for KubeTorrent (e.g. torrent.example.com): " DOMAIN
read -rp "WireGuard Private Key: " WG_PRIV
read -rp "WireGuard Peer Public Key: " WG_PUB
read -rp "WireGuard Interface Address (e.g. 10.64.0.2/32): " WG_ADDR
read -rp "WireGuard Endpoint IP: " WG_IP
read -rp "WireGuard Endpoint Port [51820]: " WG_PORT
WG_PORT="${WG_PORT:-51820}"

SESSION_SECRET="$(openssl rand -hex 32)"

cat > "${ROOT_DIR}/k8s/base/secret-portal.yaml" <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: portal-secret
  namespace: kubetorrent
type: Opaque
stringData:
  SESSION_SECRET: "${SESSION_SECRET}"
EOF

cat > "${ROOT_DIR}/k8s/base/secret-wireguard.yaml" <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: wireguard-config
  namespace: kubetorrent
type: Opaque
stringData:
  WIREGUARD_PRIVATE_KEY: "${WG_PRIV}"
  WIREGUARD_PUBLIC_KEY: "${WG_PUB}"
  WIREGUARD_ADDRESSES: "${WG_ADDR}"
  WIREGUARD_ENDPOINT_IP: "${WG_IP}"
  WIREGUARD_ENDPOINT_PORT: "${WG_PORT}"
EOF

sed -i "s|torrent\.example\.com|${DOMAIN}|g" \
  "${ROOT_DIR}/k8s/base/configmap-users.yaml" \
  "${ROOT_DIR}/k8s/base/ingress.yaml"

echo "Updated secrets, domain (${DOMAIN}), and ingress."
echo "Deploy with: kubectl apply -k k8s/base"
