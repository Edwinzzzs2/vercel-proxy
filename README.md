# Vercel Proxy

Simple http proxy for Vercel.

### Delpoy

[![Deploy with Vercel](https://vercel.com/button)](https://vercel.com/new/clone?repository-url=https%3A%2F%2Fgithub.com%2FTBXark%2Fvercel-proxy)

### Docker

```bash
docker run -d --name vercel-proxy --restart unless-stopped \
  -p 3000:3000 \
  -e PROXY_AUTH_TOKEN='replace-with-a-long-random-secret' \
  ghcr.io/edwinzzzs2/vercel-proxy:latest
```

Or with docker compose:

```bash
git clone https://github.com/Edwinzzzs2/vercel-proxy.git
cd vercel-proxy
cp .env.example .env
# Replace PROXY_AUTH_TOKEN in .env. A strong value can be generated with:
# openssl rand -hex 32
docker compose up -d
```

The image supports both `linux/amd64` and `linux/arm64`. GitHub Actions publishes it to
GitHub Container Registry after every push to `main`, and when a version tag such as
`v1.0.0` is pushed. The first published package may need to be changed to **Public** in
the repository owner's GitHub Packages settings before other users can pull it without
authentication.

Docker Compose requires `PROXY_AUTH_TOKEN`. Callers must send the same value in
the `X-Proxy-Token` header. `PROXY_DOMAIN_WHITELIST` is optional and accepts
comma-separated domain rules; leaving it empty allows all target domains.
Authentication prevents unauthorized use but does not encrypt plain HTTP traffic; use
HTTPS when transmitting sensitive credentials in production.

```bash
curl -H 'X-Proxy-Token: replace-with-a-long-random-secret' \
  'http://server-ip:3000/https://example.com'
```

To customize behavior, mount a JSON config file and pass `--config`:

```bash
docker run -d --name vercel-proxy -p 3000:3000 \
  -e PROXY_AUTH_TOKEN='replace-with-a-long-random-secret' \
  -v $(pwd)/config.json:/config/config.json \
  ghcr.io/edwinzzzs2/vercel-proxy:latest --addr :3000 --config /config/config.json
```

### Usage

```javascript
fetch("https://project-name.vercel.app/https://example.com?param1=value1&param2=value2")
.then((res) => res.text())
.then(console.log.bind(console))
.catch(console.error.bind(console));

```

```bash
curl -L https://project-name.vercel.app/https:/example.com?param1=value1&param2=value2
```

Just add `https://project-name.vercel.app/` before the url you want to proxy.

### License

**vercel-proxy** is released under the MIT license. [See LICENSE](LICENSE) for details.
