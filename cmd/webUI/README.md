# Relay WebUI

Simple web interface for managing TCP/UDP relay configurations.

## Features

- 📝 Create/Edit/Delete relay configs via web UI
- 💾 JSON file storage (`relays.json`)
- 🎯 RESTful API
- 📦 Single binary (embedded static files)
- 🎨 Clean Vue3 interface (no build tools needed)

## Quick Start

```bash
# Build
go build -o webui .

# Run
./webui

# Open browser
open http://localhost:8080
```

## API Endpoints

```
GET    /api/relays      - List all relays
GET    /api/relays/:id  - Get single relay
POST   /api/relays      - Create relay
PUT    /api/relays/:id  - Update relay
DELETE /api/relays/:id  - Delete relay
```

## Config Format

```json
[
  {
    "id": "uuid",
    "name": "DNS Relay",
    "src": ":53",
    "dst": "1.1.1.1:53",
    "udp": true
  }
]
```

## Tech Stack

- Backend: Go + Gin
- Frontend: Vue3 (CDN) + Vanilla JS
- Storage: JSON file
- Deployment: Single binary
