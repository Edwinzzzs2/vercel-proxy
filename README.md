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

### Configuration

The proxy is closed by default. When no variables are configured, proxy requests
return `401` instead of creating an open proxy. Configure these variables in the
Vercel project settings or in the Docker `.env` file:

| Variable | Default | Description |
| --- | --- | --- |
| `PROXY_AUTH_TOKEN` | Empty | Secret required in the `X-Proxy-Token` request header. When empty, no protected target can be accessed. |
| `PROXY_AUTH_WHITELIST` | Empty | Comma-separated targets that can be proxied without `X-Proxy-Token`. Empty means no target is exempt from authentication. |
| `PROXY_DOMAIN_WHITELIST` | Empty | Comma-separated targets the service is allowed to proxy. Empty means all target domains are eligible, but authentication is still enforced. |

Request processing follows this order:

1. Targets outside `PROXY_DOMAIN_WHITELIST` return `403` when that variable is set.
2. Targets matching `PROXY_AUTH_WHITELIST` are proxied without a token.
3. Every other target requires the configured `PROXY_AUTH_TOKEN` and returns `401`
   when the token is missing or invalid.

Both whitelist variables support exact domains and their subdomains, `*` / `?`
wildcards, `-` exclusions, and optional ports. For example, `github.com` also
matches `api.github.com`, while `githubusercontent.com` matches GitHub release
asset hosts such as `release-assets.githubusercontent.com`.

Recommended configuration for Aliyun Codeup and GitHub:

```dotenv
PROXY_AUTH_TOKEN=replace-with-a-long-random-secret
PROXY_AUTH_WHITELIST=openapi-rdc.aliyuncs.com,codeup.aliyun.com,github.com,githubusercontent.com,githubassets.com
PROXY_DOMAIN_WHITELIST=
```

After changing Vercel environment variables, redeploy the project. Whitelisted
targets can be requested directly:

```bash
curl 'https://project-name.vercel.app/https://github.com/owner/repository'
```

Other targets require the token:

```bash
curl -H 'X-Proxy-Token: replace-with-a-long-random-secret' \
  'https://project-name.vercel.app/https://example.com/path'
```

JavaScript callers can send the same header with `fetch`:

```javascript
const response = await fetch(
  "https://project-name.vercel.app/https://example.com/api",
  {
    headers: {
      "X-Proxy-Token": "replace-with-a-long-random-secret",
    },
  },
);
```

A browser address bar or a normal download link cannot attach a custom request
header. Add targets that must support direct browser downloads to
`PROXY_AUTH_WHITELIST`; otherwise use a client such as `curl` or application code
that can send `X-Proxy-Token`. Do not put the token in the query string because URLs
can be stored in browser history, access logs, and referrer headers.

The proxy removes `X-Proxy-Token` before forwarding the request upstream.
Authentication does not encrypt plain HTTP traffic; use HTTPS when transmitting
sensitive credentials in production.

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
