# Sounder

**English** | [中文](README.zh-CN.md)

An agent cloud platform built on [PiG](https://pi-in-go.dev), the Go implementation of Pi.

Sounder splits the control plane from execution. Clients never run an agent; every tool call
happens inside a Worker that you can put in a container, a Kubernetes pod, or on a laptop.

```
Web / Desktop / Android
        │  HTTP + SSE
        ▼
   sounder-server          session orchestration, auth, event proxy
        │  RPC
        ▼
  sounder-gateway          reverse tunnel + worker registry + SSE fan-out
        │  WebSocket
        ▼
  sounder-worker           Go + PiG SDK, one Runtime per session
```

## Why this shape

- **Workers are disposable.** A session survives `kill -9` because PiG writes an append-only
  JSONL log; `Manager.Restore` reopens it on boot.
- **Desktop workers live behind NAT.** They dial out to the gateway, so no inbound port is needed.
- **The UI is a thin client.** Web, Desktop and Android share one HTML file; the only difference
  is where the API base URL comes from.

## Components

| Path | Binary | Role |
|---|---|---|
| `sounder-worker/` | `sounder-worker` | Runs PiG sessions, tools, policy and approvals |
| `sounder-gateway/` | `sounder-gateway` | Worker tunnel, registry, SSE fan-out |
| `sounder-server/` | `sounder-server` | Session lifecycle, PG persistence, event proxy |
| `sounder-web/` | nginx | Static web client |
| `sounder-desktop/` | `sounder-desktop`, `sounder.apk` | Wails v3 shell for desktop and Android |
| `sounder-verify/` | `v2-isolation`, `v3-recovery` | Verification tools for the architecture assumptions |

## Quick start

```bash
cp .env.example .env
# put a model key in .env, for example:
#   MINIMAX_CN_API_KEY=...
make up
open http://localhost:3000
```

`make up` starts eight services: `sounder-web`, `sounder-server`, `sounder-gateway`,
`sounder-worker`, `postgres`, `redis`, `minio` and `minio-init`.

The token in `.env` (`SOUNDER_TOKEN`) is used for the web UI and for gateway authentication.

## Run from source

```bash
# terminal 1
cd sounder-gateway && go run ./cmd/sounder-gateway -addr=127.0.0.1:9090 -token=dev

# terminal 2
cd sounder-worker && go run ./cmd/sounder-worker \
  -addr=127.0.0.1:8080 -data=/tmp/sounder -model=minimax-cn/MiniMax-M3 \
  -gateway=ws://127.0.0.1:9090/v1/tunnel -gateway-token=dev -worker-id=w1

# terminal 3
cd sounder-server && go run ./cmd/sounder-server \
  -addr=127.0.0.1:8000 -gateway=http://127.0.0.1:9090 \
  -gateway-token=dev -token=dev
```

Create a session and send a prompt:

```bash
TOKEN=dev
SID=$(curl -s -X POST localhost:8000/v1/sessions \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"minimax-cn/MiniMax-M3"}' | jq -r .id)

curl -s -X POST localhost:8000/v1/sessions/$SID/prompts \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"message":"Use bash to run echo hello"}'

curl -N localhost:8000/v1/sessions/$SID/events -H "Authorization: Bearer $TOKEN"
```

When a tool needs approval, `approval.requested` shows up in the stream. Approve it with:

```bash
curl -s -X POST "localhost:8000/v1/approvals/<id>?workerId=w1" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"allow":true}'
```

## What is verified

| Capability | Status |
|---|---|
| Real model streaming (MiniMax M3) | ✅ |
| Tool execution with human approval | ✅ |
| Session recovery after `kill -9` | ✅ |
| Control plane persistence (PostgreSQL) | ✅ |
| Gateway tunnel end to end | ✅ |
| Android APK build | ✅ |
| Desktop GUI on a graphical session | ⚠️ not run yet |
| iOS build | ⚠️ needs full Xcode |
| Kubernetes manifests | ❌ not written |

## Policy and safety

PiG does not sandbox model output. Sounder adds a policy layer in front of every tool call:

- `read` / `grep` / `ls` run freely.
- `write` / `edit` are checked against the workspace path.
- `bash` and outbound tools block on human approval, and a rejection is returned to the model as
  an error tool result.
- Ten high-risk command patterns (`curl | sh`, writes to `.git/hooks`, `rm -rf /`, …) always
  require approval, regardless of session state.

## Documentation

- [Architecture and design notes (Chinese)](pig-agent-cloud-architecture.md)
- [Worker](sounder-worker/README.md)
- [Gateway](sounder-gateway/README.md)
- [Control plane](sounder-server/README.md)
- [Web](sounder-web/README.md)
- [Desktop / Android](sounder-desktop/README.md)

## Status

Early. The core loop works end to end, but the project is pre-1.0 and PiG itself is still 0.x.
Pin the PiG commit in `go.sum` and expect breaking changes when upgrading.

License: TBD.
