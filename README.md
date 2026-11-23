# KubeTorrent

Multi-user BitTorrent web client setup for Kubernetes featuring:

- **Single WireGuard Gateway Tunnel**: A dedicated `kubetorrent-vpn-gateway` Pod runs `gluetun` (WireGuard + kill-switch) and `pod-gateway` (VXLAN router). Each user's `qbittorrent` Pod routes all external peer/tracker/DHT traffic through this single WireGuard tunnel while keeping cluster-internal WebUI traffic off the VPN.
- **Separate Login Landing Page (Password + Passkey / WebAuthn)**: `kubetorrent-portal` runs outside the WireGuard tunnel, serves `/login` (Username/Password + Passkey sign-in) and `/account` (Passkey registration & password management), and reverse-proxies authenticated users to their own `kubetorrent-client-<user>` Pod.
- **Admin Console (`/admin`) with Zero-Downtime Pod Provisioning**: Admins can create/delete users (automatically provisioning or removing their isolated `Deployment` and `Service` via Kubernetes API without restarting other users' Pods), reset passwords, and revoke Passkeys.
- **Persistent & User-Segregated Downloads on Host Storage**: Each user's qBittorrent config/resume state is stored on the `kubetorrent-configs` PVC (`subPath: <user>`), and downloaded files are stored on the host at `/srv/kubetorrent/downloads/<user>` (`hostPath` with `subPath: <user>`).

## Architecture

```text
Browser (HTTPS)
   │
   ▼
Ingress ──▶ kubetorrent-portal (outside WireGuard)
               │  • /login   (Password + Passkey)
               │  • /account (Manage Passkeys & Password)
               │  • /admin   (Provision/Delete User Pods)
               │  • /*       (Reverse proxy to user's Pod :8080)
               ▼
   ┌───────────────────────────┬───────────────────────────┐
   ▼                           ▼                           ▼
kubetorrent-client-alice    kubetorrent-client-bob    (provisioned via /admin)
(hostPath/alice)            (hostPath/bob)
   │                           │
   └───────────┬───────────────┘  (VXLAN UDP 4789)
               ▼
      kubetorrent-vpn-gateway (gluetun WireGuard + pod-gateway)
               │
               ▼ (all external torrent traffic via wg0)
           Internet
```

## Quick Start

### 1. Configure & Deploy (with Let's Encrypt TLS)

Run the setup script from a machine with `kubectl` access:

```bash
./scripts/configure.sh
```

The script will:
1. Prompt for your domain, Let's Encrypt email, and path to `wg-proton.conf`.
2. Generate `SESSION_SECRET` and `k8s/base/secret-wireguard.yaml`.
3. Apply `k8s/base`.
4. Trigger Certbot (`kubetorrent-certbot-renew` CronJob) to issue your initial Let's Encrypt certificate into `Secret/kubetorrent-tls` (and auto-renew daily).

For local testing without Let's Encrypt (`localhost`, `COOKIE_SECURE=false`, `/tmp/kubetorrent-downloads`):

```bash
kubectl apply -k k8s/overlays/local
```

### 3. Provision Users & Register Passkeys

1. Sign in at `/login` with the `admin` account (`changeme-admin` by default — change it immediately at `/account`).
2. Visit `/admin` to create/delete users (each non-admin user automatically gets an isolated `kubetorrent-client-<user>` Pod & Service) or reset passwords/passkeys.
3. Users can sign in at `/login` and visit `/account` to register Passkeys (Touch ID, Windows Hello, YubiKey, etc.).

## Running Portal Tests

```bash
cd portal
go test -v ./...
```
