# Free Proxy

1. Start the add-on.
2. Use `default` as the model, or call `http://<host>:8080/v1/models` and choose an exact ID.
3. Open the add-on Web UI to browse and copy model IDs, or click **Rerank free models** to refresh the saved `default` order.

Set `openrouter_api_key` in the add-on settings to enable OpenRouter. Only models whose catalog reports zero prices are exposed. If a model's price changes, the default route skips it before sending a completion request upstream.
