---
title: Debrid Link Setup
description: Configure Debrid Link provider.
---

Debrid Link is a supported Debrid provider.

## Configuration

```json
{
  "debrids": [
    {
      "provider": "debridlink",
      "name": "Debrid Link",
      "api_key": "YOUR_API_KEY"
    }
  ]
}
```

Get your API key from the Debrid Link dashboard.

All configuration options from [Real Debrid](./real-debrid/) apply (rate limits, workers, proxy, etc.).

See [Configuration Reference](../configuration/#debrid-providers) for full options.
