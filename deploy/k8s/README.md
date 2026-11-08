# Kubernetes manifests

用 Kustomize 组织，一条命令部署：

```bash
kubectl apply -k deploy/k8s
```

## 部署前必须改

| 文件 | 改什么 |
|---|---|
| `config.yaml` | `sounder-secrets` 里的 `SOUNDER_TOKEN`、`POSTGRES_PASSWORD`、`MINIMAX_CN_API_KEY` |
| 各 `*.yaml` | 镜像 `ghcr.io/getsounder/*:dev` → 你的 registry 与 digest |
| `web.yaml` | Ingress 的 `host` 与 `tls.secretName` |

> `config.yaml` 里的 Secret 是示例值，不要直接用于生产。建议用 External Secrets / Sealed Secrets
> 管理，或 `kubectl create secret` 单独创建后从 kustomization 里移除该文件。

## 资源清单

| 资源 | 数量 | 说明 |
|---|---|---|
| Namespace | 1 | `sounder` |
| ConfigMap / Secret | 1 / 1 | 环境变量与凭证 |
| Deployment | 4 | gateway、server、web、redis |
| StatefulSet | 2 | postgres、worker |
| Service | 6 | 含 worker 的无头 Service |
| Ingress | 1 | nginx，SSE 关闭缓冲 |
| HPA | 1 | worker 按 CPU 70% 扩到 10 |
| PDB | 2 | server / gateway 各 `minAvailable: 1` |
| NetworkPolicy | 7 | 默认拒绝 + 按需放行 |

## 关键设计

**worker 用 StatefulSet，不是 Deployment**
每个 Pod 有独立 PVC，PiG 的 Session JSONL 写在 `/data`。Pod 重建后 `Manager.Restore`
扫描 `sessions/*/agent` 重新打开，会话不丢。

**探针免认证**
`/healthz` 和 `/readyz` 在鉴权中间件之前放行。否则 `httpGet` 探针无法携带 Bearer token，
会一直 401。

**NetworkPolicy 默认拒绝**
worker 只能出站到 `sounder-gateway:9090` 和公网 443（模型 API），不能访问数据库或其他服务。
数据存储只接受来自 `sounder-server` 的连接。

**`terminationGracePeriodSeconds: 300`**
留足一轮工具执行的时间，避免 SIGKILL 丢掉 Session 尾部。

**HPA 依赖 metrics-server**
集群没装 metrics-server 时 HPA 不会生效，但不会影响 Pod 运行。

## 已知缺口

- Postgres 单副本、Redis 单副本，没有高可用；生产建议用托管服务。
- 没有 PodSecurityAdmission 标签（`restricted`），但容器已按该标准配置。
- 没有 ServiceMonitor / PrometheusRule，指标需要自行接入。
- 镜像未签名，供应链校验（cosign / SBOM）未配置。
