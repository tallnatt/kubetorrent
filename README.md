# KubeTorrent

Multi-user BitTorrent web client setup for Kubernetes featuring:

- **Single WireGuard Tunnel for All BitTorrent Traffic**: A single Pod (`kubetorrent-clients`) runs `gluetun` (WireGuard + `iptables` kill-switch) alongside per-user `qbittorrent` containers sharing the Pod network namespace. All BitTorrent peer/tracker/DHT traffic is forced through WireGuard, while WebUI traffic stays inside the cluster.
- **Separate Login Landing Page (Password + Passkey / WebAuthn)**: `kubetorrent-portal` runs outside the VPN tunnel, serves `/login` (Username/Password + Passkey sign-in) and `/account` (Passkey registration & password management), and reverse-proxies authenticated users to their isolated qBittorrent WebUI.
- **Persistent & User-Segregated Downloads on Host Storage**: Each user's qBittorrent config/resume state is stored on a PVC (`subPath: <user>`), and downloaded files are written to a node `hostPath` directory (`/srv/kubetorrent/downloads/<user>`), mounted at `/downloads` inside each user's container.
- **Kustomize Manifests**: Base manifests in `k8s/base/` and an example local overlay in `k8s/overlays/local/`.

## Architecture

```text
Browser (HTTPS)
   │
   ▼
Ingress ──▶ kubetorrent-portal (outside WireGuard)
               │  • /login  (Password + Passkey)
               │  • /account (Manage Passkeys & Password)
               │  • /*      (Reverse proxy per authenticated user)
               ▼
        kubetorrent-clients Pod (NetworkPolicy-restricted)
         ├── wireguard (gluetun + iptables kill-switch)
         ├── qbittorrent-alice (:8081, /downloads -> hostPath/alice)
         └── qbittorrent-bob   (:8082, /downloads -> hostPath/bob)
               │
               ▼ (all external traffic via WireGuard wg0)
           Internet
```

## Quick Start

### 1. Build the Portal Image

```bash
cd portal
docker build -t ghcr.io/tallnatt/kubetorrent-portal:latest .
```

### 2. Configure Secrets & Domain

1. Update `k8s/base/secret-wireguard.yaml` with your WireGuard provider's keys, endpoint, and address.
2. Generate a random session secret for `k8s/base/secret-portal.yaml`:
   ```bash
   openssl rand -hex 32
   ```
3. Update `RP_ID` and `RP_ORIGINS` in `k8s/base/configmap-users.yaml` and the host in `k8s/base/ingress.yaml` to match your domain (Passkeys/WebAuthn require HTTPS or `localhost`).
4. Set the host directory path in `k8s/base/clients-deployment.yaml` (default: `/srv/kubetorrent/downloads`).

### 3. Deploy with Kustomize

```bash
kubectl apply -k k8s/base
```

For local testing (`localhost`, `COOKIE_SECURE=false`, `/tmp/kubetorrent-downloads`):

```bash
kubectl apply -k k8s/overlays/local
```

### 4. Register a Passkey

1. Sign in at `/login` using a configured username and `initial_password` (from `users.json`).
2. Visit `/account` to register one or more Passkeys (Touch ID, Windows Hello, YubiKey, etc.) and change your password.
3. On future visits to `/login`, click **Sign in with Passkey**.

## Adding Users

To add a new user (e.g., `charlie` on WebUI port `8083` and torrent port `6883`):
1. Add `{ "username": "charlie", "port": 8083, "initial_password": "..." }` to `k8s/base/configmap-users.yaml`.
2. Add a `qbittorrent-charlie` container (and `seed_user charlie 8083 6883` in the init container) to `k8s/base/clients-deployment.yaml`.
3. Add `8083` to `FIREWALL_INPUT_PORTS` in `clients-deployment.yaml`, `services.yaml`, and `networkpolicy.yaml`.

## Running Portal Tests

```bash
cd portal
go test -v ./...
```
