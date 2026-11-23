#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

read -rp "Domain for KubeTorrent (e.g. torrent.example.com): " DOMAIN
read -rp "Email for Let's Encrypt (Certbot): " LE_EMAIL
read -rp "Path to wg-proton.conf [./wg-proton.conf]: " WG_CONF_PATH
WG_CONF_PATH="${WG_CONF_PATH:-./wg-proton.conf}"

if [ ! -f "$WG_CONF_PATH" ]; then
  echo "Error: WireGuard config file not found at $WG_CONF_PATH" >&2
  exit 1
fi

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

{
  cat <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: wireguard-config
  namespace: kubetorrent
type: Opaque
stringData:
  wg-proton.conf: |
EOF
  sed 's/^/    /' "$WG_CONF_PATH"
} > "${ROOT_DIR}/k8s/base/secret-wireguard.yaml"

sed -i "s|torrent\.example\.com|${DOMAIN}|g" \
  "${ROOT_DIR}/k8s/base/configmap-users.yaml" \
  "${ROOT_DIR}/k8s/base/ingress.yaml"

sed -i "s|admin@example\.com|${LE_EMAIL}|g" \
  "${ROOT_DIR}/k8s/base/configmap-users.yaml"

echo "Applying Kustomize manifests from ${ROOT_DIR}/k8s/base..."
kubectl apply -k "${ROOT_DIR}/k8s/base"

echo "Issuing initial Let's Encrypt TLS certificate via Certbot..."
kubectl delete job kubetorrent-certbot-init -n kubetorrent --ignore-not-found >/dev/null 2>&1 || true
kubectl create job --from=cronjob/kubetorrent-certbot-renew kubetorrent-certbot-init -n kubetorrent
kubectl wait --for=condition=complete --timeout=180s job/kubetorrent-certbot-init -n kubetorrent

echo "Done! KubeTorrent is deployed at https://${DOMAIN} with Let's Encrypt TLS."
