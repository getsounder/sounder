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

## Deployment modes

| Mode | Entry | Notes |
|---|---|---|
| Development | `docker-compose.yml` | Builds images from source |
| Production (Compose) | `docker-compose.prod.yml` | Pulls registry images, fail-fast on missing secrets, per-service limits |
| Kubernetes | `deploy/k8s/` | Kustomize; StatefulSet worker, NetworkPolicy, HPA |

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

## Local end-to-end check

The whole stack can be exercised without a cloud account:

```bash
docker compose up -d --build          # 8 services
curl localhost:8000/healthz           # {"status":"ok"}
curl -H "Authorization: Bearer dev-token" localhost:8000/v1/workers
./scripts/e2e-auth.sh                 # user system e2e: 74 assertions
```

The auth e2e script covers local registration/login, Cookie + Bearer sessions,
session ownership isolation, legacy-token admin visibility, session revocation,
password change/reset, identity unbinding, CORS, OAuth-disabled behavior,
body limits and registration rate limiting. It creates throwaway accounts in
the local database (random emails) and deletes the session it creates.

- session create → prompt → SSE (`message.*`, `turn.end`, `error`) works end to end;
- the worker container can write `/data` (the Dockerfile chowns the named volume);
- `OPTIONS` preflight returns `204`; configured Web origins are echoed with credentials.
- the Android APK (`sounder-desktop/bin/sounder.apk`) connects to `http://10.0.2.2:8000`
  from the emulator, lists sessions, opens history and streams events.

Real model output needs a real key in `.env` (`MINIMAX_CN_API_KEY`, `OPENAI_API_KEY` or
`ANTHROPIC_API_KEY`). With a placeholder key the turn ends with a provider `401`, which is
expected; the control-plane flow above still works.

## What is verified

| Capability | Status |
|---|---|
| Real model streaming (MiniMax M3) | ✅ |
| Tool execution with human approval | ✅ |
| Session recovery after `kill -9` | ✅ |
| Control plane persistence (PostgreSQL) | ✅ |
| Gateway tunnel end to end | ✅ |
| Android APK build + emulator session flow | ✅ |
| Local auth e2e (`scripts/e2e-auth.sh`, 74 assertions) | ✅ |
| Full docker-compose stack (8 services) | ✅ |
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
- [User system: local accounts + Google/GitHub (Chinese)](docs/user-system.md)
- [Worker](sounder-worker/README.md)
- [Gateway](sounder-gateway/README.md)
- [Control plane](sounder-server/README.md)
- [Web](sounder-web/README.md)
- [Desktop / Android](sounder-desktop/README.md)

## Status

Early. The core loop works end to end, but the project is pre-1.0 and PiG itself is still 0.x.
Pin the PiG commit in `go.sum` and expect breaking changes when upgrading.

License: [MIT](LICENSE).
