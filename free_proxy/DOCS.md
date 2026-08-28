# Free Proxy

1. Start the add-on.
2. Call `http://<host>:8080/v1/models`.
3. Copy one returned ID, such as `opencode/big-pickle`, into `default_model` and your OpenAI-compatible client.

Set `openrouter_api_key` in the add-on settings to enable OpenRouter. Only models whose catalog reports zero prices are exposed. If a model's price changes, the request is rejected before it reaches the model provider.
