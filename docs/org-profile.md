<p align="center"><img src="https://raw.githubusercontent.com/getsounder/sounder/main/docs/assets/logo.svg" width="92" alt="Sounder"></p>
<h1 align="center">Sounder</h1>
<p align="center"><strong>把 Agent 跑在设备与云端之间</strong><br><sub>Agent cloud, on every device.</sub></p>

<p align="center">
  <a href="https://github.com/getsounder/sounder/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/getsounder/sounder/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/getsounder/sounder/blob/main/LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-4f7cff"></a>
  <a href="https://getsounder.github.io/sounder/"><img alt="Docs" src="https://img.shields.io/badge/docs-live-4f7cff"></a>
</p>

## 我们在做什么

Sounder 是一个开源的 Agent 云平台，把**控制面**和**执行面**分开：

- **控制面**负责用户、设备、会话、审批与审计
- **执行面**是 Worker，跑在 Docker、Kubernetes，或者你自己的笔记本上
- **一个 Worker = 一台设备**；桌面、云端、手机共享同一份会话

客户端不执行 Agent，所有工具调用都发生在可审计的 Worker 里。

## 能力

- 多用户：本地账号 / Google / GitHub 登录，设备与会话按用户隔离
- 设备：Computer（关机保留镜像）/ Sandbox（挂载目录），云端按配额 provision
- 人在回路：工具审批、批量允许/拒绝、`ask_user` 多问题提问
- 三端同源：Web / Desktop / iOS / Android 共用同一份 UI
- 本地模型：Ollama 开箱即用，兼容 LM Studio、llama.cpp 等 OpenAI 端点
- 自托管：Docker Compose / Kustomize，Prometheus 指标与审计日志

## 快速开始

```bash
git clone https://github.com/getsounder/sounder.git
cd sounder
cp .env.example .env
# 填模型 key，或使用本地 Ollama：
#   MODEL=ollama/qwen2.5:3b
#   OLLAMA_HOST=http://host.docker.internal:11434/v1
make up
open http://localhost:3000
```

## 仓库

| 仓库 | 说明 |
|---|---|
| [sounder](https://github.com/getsounder/sounder) | 控制面、网关、Worker、Web / 桌面 / 移动端 |
| [.github](https://github.com/getsounder/.github) | 组织主页与社区配置 |

## 链接

- 官网与文档：https://getsounder.github.io/sounder/
- 架构说明：[pig-agent-cloud-architecture.md](https://github.com/getsounder/sounder/blob/main/pig-agent-cloud-architecture.md)
- 设备模型：[docs/devices.md](https://github.com/getsounder/sounder/blob/main/docs/devices.md)
- 备份恢复：[docs/backup.md](https://github.com/getsounder/sounder/blob/main/docs/backup.md)
- 发布记录：https://github.com/getsounder/sounder/releases

<details>
<summary>English</summary>

Sounder is an open-source agent cloud platform built on [PiG](https://pi-in-go.dev).
It separates the control plane (users, devices, sessions, approvals, audit) from
execution (workers in Docker, Kubernetes, or on your laptop). One worker is one
device; desktop, cloud, and mobile share the same session.

- Multi-user with local, Google, and GitHub login
- Computer / Sandbox devices with quota-based cloud provisioning
- Human-in-the-loop tool approvals and multi-question `ask_user`
- Shared UI across web, desktop, iOS, and Android
- Ollama and any OpenAI-compatible endpoint
- Self-host with Docker Compose or Kustomize

```bash
git clone https://github.com/getsounder/sounder.git
cd sounder && cp .env.example .env && make up
```

</details>

<p align="center"><sub>MIT License · Built on <a href="https://pi-in-go.dev">PiG</a></sub></p>
