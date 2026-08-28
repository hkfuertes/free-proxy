# Free Proxy

An OpenAI-compatible proxy for zero-price OpenCode and OpenRouter models. It does not install the OpenCode CLI.

## Model policy

`GET /v1/models` returns IDs that are used unchanged in `POST /v1/chat/completions`:

```text
opencode/big-pickle
openrouter/nvidia/nemotron-3.5-lightning:free
```

- Both providers discover zero-price models at startup.
- Before every completion, the requested model's price is checked again.
- An unknown, paid, or no-longer-free model returns `400` **without sending a completion request upstream**.
- `/v1/models` is the startup snapshot. Restart the add-on or container to refresh the displayed list.

OpenRouter is enabled only when an API key is configured. Its catalog is filtered so every published price must be zero.

## API

- `GET /healthz`
- `GET /v1/models`
- `GET /v1/models/{id}`
- `POST /v1/chat/completions`

OpenCode requires upstream SSE. When the client does not request `stream: true`, the proxy aggregates it into a normal OpenAI chat completion.

## Standalone Docker

The production image is `scratch`: a static Go binary plus only the CA bundle required for HTTPS.

```sh
docker build -t free-proxy .
docker run --rm \
  -e LISTEN_ADDR=:8080 \
  -p 127.0.0.1:8080:8080 \
  free-proxy
```

To enable OpenRouter, set the key only in the execution environment:

```sh
export OPENROUTER_API_KEY='...'
docker run --rm \
  -e LISTEN_ADDR=:8080 \
  -e OPENROUTER_API_KEY \
  -p 127.0.0.1:8080:8080 \
  free-proxy
```

Test it:

```sh
curl http://127.0.0.1:8080/v1/models
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"opencode/big-pickle","messages":[{"role":"user","content":"Say hello"}]}'
```

Do not publish the port without `PROXY_API_KEY` or a trusted network/reverse proxy.

## Home Assistant add-on

`config.yaml` makes this directory a Home Assistant add-on. Its settings page has these options:

| Option | Purpose |
|---|---|
| `openrouter_api_key` | Enables OpenRouter. |
| `proxy_api_key` | Protects the proxy API. |
| `default_model` | An exact ID from `/v1/models`; defaults to `opencode/big-pickle`. |
| `listen_addr` | Defaults to `0.0.0.0:8080`. |

The add-on reads these options from `/data/options.json`. Equivalent environment variables take precedence, so the same image works standalone and as an add-on:

| Variable | Purpose |
|---|---|
| `OPENROUTER_API_KEY` | OpenRouter key. |
| `PROXY_API_KEY` | Key required from proxy clients. |
| `DEFAULT_MODEL` | Default model, including its provider prefix. |
| `LISTEN_ADDR` | Listening address. |
| `OPENCODE_API_KEY` | Defaults to `public`; accepts a real Zen key. |
| `OPENCODE_UPSTREAM` / `OPENCODE_CATALOG` | Zen and catalog URLs. |
| `OPENROUTER_UPSTREAM` | OpenRouter base URL. |
| `OPTIONS_PATH` | Add-on options path; defaults to `/data/options.json`. |

The native Home Assistant add-on settings schema cannot populate a selector dynamically from the Internet. Call `/v1/models`, then paste the exact ID into `default_model`; the API uses that same ID.

Configure an OpenAI-compatible Home Assistant client with:

- **Base URL:** `http://<proxy-host>:8080/v1`
- **Model:** one returned by `GET /v1/models`
- **API key:** `proxy_api_key` if configured; any value otherwise.

## Verify

```sh
docker run --rm -v "$PWD:/src" -w /src golang:1.24-alpine go test ./...
```
