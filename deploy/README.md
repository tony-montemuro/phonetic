# Deploying phonetic

phonetic runs as a native binary under systemd; only Kokoro runs in
Docker. These are the one-time steps on the host (Arch Linux with an NVIDIA
GPU). After them, `make deploy DEPLOY_HOST=user@host` from the Mac builds,
installs and restarts phonetic.

## 1. Packages

With the NVIDIA driver already installed:

```sh
sudo pacman -S ffmpeg docker docker-compose nvidia-container-toolkit
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl enable docker && sudo systemctl restart docker
```

## 2. Kokoro (GPU)

```sh
scp deploy/kokoro.compose.yml user@host:   # from the Mac
sudo docker compose -f kokoro.compose.yml up -d  # on the host
curl -s http://127.0.0.1:8880/v1/audio/voices | head -c 200
```

It restarts with Docker (`restart: unless-stopped`).

## 3. Settings and secrets

```sh
sudo install -d -m 0755 /etc/phonetic
sudo install -m 0600 /dev/null /etc/phonetic/env
sudoedit /etc/phonetic/env   # start from deploy/phonetic.env.example
```

## 4. Network

phonetic only needs a private network your phone is also on. Either:

- **Tailscale:** enable MagicDNS and HTTPS certificates in the admin
  console, `tailscale up`, then `sudo tailscale serve --bg 8080` (HTTPS
  with a real certificate, forwarded to phonetic on loopback). Set
  `PHONETIC_ADDR=127.0.0.1:8080` and
  `PHONETIC_PUBLIC_URL=https://<host>.<tailnet>.ts.net`.
- **WireGuard:** set `PHONETIC_ADDR` to the host's tunnel address (e.g.
  `10.8.0.1:8080`) and `PHONETIC_PUBLIC_URL=http://10.8.0.1:8080`. Traffic
  is encrypted by the tunnel. If phonetic starts before the tunnel, it
  retries every 5 seconds (see `phonetic.service`).

Never listen on a LAN or public address: basic auth is the only other
protection.

## 5. Stay awake

```sh
sudo systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target
```

## 6. Deploy and check

```sh
make deploy DEPLOY_HOST=user@host            # from the Mac
journalctl -u phonetic -f                    # on the host
curl -su phonetic https://<public url>/feed.xml | head  # prompts for the password
```

## 7. Subscribe

- **Apple Podcasts:** Library → ⋯ → Follow a Show by URL → the feed URL
  (`<public url>/feed.xml`). It should ask for the user and password; if it
  doesn't, or refuses a plain-HTTP feed, use Downcast.
- **Downcast** (fallback): Add Podcast → URL, with the same credentials.
