# GMINI-Web-Proxy

**English** · [Français](README.fr.md)

![Go](https://img.shields.io/badge/Go-stdlib%20only-00ADD8)
![Platform](https://img.shields.io/badge/platform-macOS%20%2F%20Linux-lightgrey)
![License](https://img.shields.io/badge/license-MIT-green)

A local HTTP proxy, written in Go with the standard library only, that exposes your own Google Gemini **web** session (`gemini.google.com`) through **OpenAI**, **OpenAI Responses**, **Anthropic** and **Google-native** compatible APIs, with a local web dashboard on `/_ui`.

> ⚠️ **Unofficial project, not affiliated with Google.** It relies on internal Gemini web endpoints and on your account cookies. This use may violate Google's terms of service and could get your account restricted or suspended. Google can also break it at any time without notice. Use at your own risk.

## Features

- Streaming, reasoning (`reasoning_content`), tool calling and image input
- Dynamic model catalogue read from your account
- Automatic pause when a quota limit or a silent model fallback is detected
- Retries and guardrails for agentic clients (stalls, refusals, unverified action claims)
- Dashboard with request history, diagnostics, tool statistics and anonymised export
- Binds to `127.0.0.1` and requires a local API key by default

## Current limitations

- The dashboard and the proxy's in-chat messages are **in French**.
- The cookie refresh script used by `GEMINI_COOKIE_REFRESH_CMD` is **not included**. Without your own script, the "connect to Google" button and auto-refresh do not work: import the cookie manually.
- The Antigravity relay for requests longer than 300 s depends on patches to a separate project that are **not published yet**.
- Developed and used mainly on macOS.

## Quick start

Requirements: Go 1.22+ and a Google account with access to Gemini.

```bash
git clone https://github.com/nioyco/GMINI-Web-Proxy.git
```

```bash
cd GMINI-Web-Proxy
go build -o gmini-web-proxy .
cp .env.example .env
chmod 0600 .env
```

In `.env`, set at least:

- `GEMINI_API_KEYS`: a random local key, for example from `openssl rand -hex 24`
- `GEMINI_COOKIE_FILE`: the path to your cookie file

Create the cookie file (mode `0600`) with a single line containing the three cookies from a signed-in `gemini.google.com` session:

```text
__Secure-1PSID=...; __Secure-1PSIDTS=...; SAPISID=...
```

These cookies give access to your Google account. Never share or commit them.

Start the proxy:

```bash
./gmini-web-proxy -config .env -listen 127.0.0.1:8081
```

Then open `http://127.0.0.1:8081/_ui`.

## Use it from a client

| API | Endpoint | Base URL |
|---|---|---|
| OpenAI Chat Completions | `POST /v1/chat/completions` | `http://127.0.0.1:8081/v1` |
| OpenAI Responses | `POST /v1/responses` | `http://127.0.0.1:8081/v1` |
| Anthropic Messages | `POST /v1/messages` | `http://127.0.0.1:8081` |
| Google native | `POST /v1beta/models/{model}` | `http://127.0.0.1:8081` |

List the model ids available to your account:

```bash
curl -s http://127.0.0.1:8081/v1/models
```

## Documentation

The full reference (configuration variables, dashboard, troubleshooting) is currently in French: [README.fr.md](README.fr.md). See also [CHANGELOG.md](CHANGELOG.md), [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md).

Issues and pull requests are welcome, in English or French. Never paste cookies, API keys, state files or raw traces.

## Credits

Derived from and extending [n0madic/go-gemini-web2api](https://github.com/n0madic/go-gemini-web2api) (MIT). Protocol inspiration: [Sophomoresty/gemini-web2api](https://github.com/Sophomoresty/gemini-web2api) and [HanaokaYuzu/Gemini-API](https://github.com/HanaokaYuzu/Gemini-API).

## License

[MIT](LICENSE)
