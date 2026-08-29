# Changelog

## 0.1.3

- Retry `default` requests when an upstream returns an error envelope with HTTP 200.
- Return HTTP 502 instead of forwarding that invalid success response for a pinned model.
