# Free Proxy

An OpenAI-compatible proxy for zero-price OpenCode and OpenRouter models. It does not install the OpenCode CLI.

## Model policy

`GET /v1/models` returns IDs that are used unchanged in `POST /v1/chat/completions`:

```text
default
opencode/big-pickle
openrouter/nvidia/nemotron-3.5-lightning:free
```

- Both providers discover zero-price models at startup.
- Before every completion, the requested model's price is checked again.
- An unknown, paid, or no-longer-free model returns `400` **without sending a completion request upstream**.
- `/v1/models` is the startup snapshot. Restart the add-on or container to refresh the displayed list.
- `default` is a virtual model: it tries the saved ranking, then the remaining free models, retrying only `429` and `5xx` responses.
- Concrete model entries include their advertised `max_tokens`; explicit `max_tokens` and `max_completion_tokens` values are capped before the request is sent upstream.
- Thinking is disabled by default. The proxy requests `reasoning.enabled: false`, removes client reasoning overrides, and skips catalog-marked mandatory-thinking models from `default`.

OpenRouter is enabled only when an API key is configured. Its catalog is filtered so every published price must be zero.

## API

- `GET /healthz`
- `GET /v1/models`
- `GET /v1/models/{id}`
- `POST /v1/chat/completions`
- `POST /rerank`

OpenCode requires upstream SSE. When the client does not request `stream: true`, the proxy aggregates it into a normal OpenAI chat completion.

## Standalone Docker

The production image is `scratch`: a static Go binary plus only the CA bundle required for HTTPS.

```sh
docker build -t free-proxy .
docker run --rm \
  -e LISTEN_ADDR=:8080 \
  -v "$PWD/data:/data" \
  -p 127.0.0.1:8080:8080 \
  free-proxy
```

To enable OpenRouter, set the key only in the execution environment:

```sh
export OPENROUTER_API_KEY='...'
docker run --rm \
  -e LISTEN_ADDR=:8080 \
  -e OPENROUTER_API_KEY \
  -v "$PWD/data:/data" \
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

### Rerank `default`

`POST /rerank` asks an available free model to order every currently exposed free model for general reasoning and coding, then saves the result to `/data/free-proxy-ranking.json`. If that file is absent at startup, the proxy generates it before serving requests; a failed ranking leaves the built-in order active.

```sh
curl -X POST http://127.0.0.1:8080/rerank \
  -H "Authorization: Bearer $PROXY_API_KEY"
```

Mount `/data` in standalone Docker if the ranking must survive container replacement.

## Home Assistant add-on

`config.yaml` makes this directory a Home Assistant add-on. Its settings page has these options:

| Option | Purpose |
|---|---|
| `openrouter_api_key` | Enables OpenRouter. |
| `proxy_api_key` | Protects the proxy API. |
| `default_model` | `default` (the free-model route) or an exact ID from `/v1/models`. |
| `enable_thinking` | Defaults to `false`; set to `true` to allow provider reasoning. |
| `listen_addr` | Defaults to `0.0.0.0:8080`. |

The add-on reads these options from `/data/options.json`. Equivalent environment variables take precedence, so the same image works standalone and as an add-on:

| Variable | Purpose |
|---|---|
| `OPENROUTER_API_KEY` | OpenRouter key. |
| `PROXY_API_KEY` | Key required from proxy clients. |
| `DEFAULT_MODEL` | Default model, including its provider prefix. |
| `ENABLE_THINKING` | Set to `true` to allow provider reasoning; defaults to `false`. |
| `LISTEN_ADDR` | Listening address. |
| `OPENCODE_API_KEY` | Defaults to `public`; accepts a real Zen key. |
| `OPENCODE_UPSTREAM` / `OPENCODE_CATALOG` | Zen and catalog URLs. |
| `OPENROUTER_UPSTREAM` | OpenRouter base URL. |
| `OPTIONS_PATH` | Add-on options path; defaults to `/data/options.json`. |
| `RANKING_PATH` | Saved `default` ranking; defaults to `/data/free-proxy-ranking.json`. |

Open the add-on's Web UI to browse and copy available model IDs, then paste an exact ID into `default_model` if you want to pin one. Set `enable_thinking` in add-on configuration when a client needs model reasoning. Click **Rerank free models** there to refresh `default`; Home Assistant ingress authenticates the UI. The native add-on settings schema cannot provide a custom action button.

Configure an OpenAI-compatible Home Assistant client with:

- **Base URL:** `http://<proxy-host>:8080/v1`
- **Model:** one returned by `GET /v1/models`
- **API key:** `proxy_api_key` if configured; any value otherwise.

## Verify

```sh
docker run --rm -v "$PWD:/src" -w /src golang:1.24-alpine go test ./...
```
