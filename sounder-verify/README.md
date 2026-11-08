# PiG Sprint 0 验证包（V2 / V3）

验证「1 Pod N Session」与「会话可恢复」这两条架构前提是否成立。
**代码未在本机编译**（项目规则禁止构建），所有 `API-CHECK` 注释处需按你 pin 的 PiG 版本确认签名。

## 前提

- Go 1.27+
- 能访问 `github.com/MichaelKinsy/PiG`
- 可选：一个可用的模型 provider（`PIG_VERIFY_MODEL=provider/model`）
  没有模型也能跑 V2 的路径隔离与 V3 的 JSONL 完整性

## 初始化

```bash
cd sounder-verify
go mod tidy          # 自动拉取 PiG
go build ./...
```

## V2：多 Session 隔离

```bash
# 只验证目录/配置布局（无需模型）
go run ./cmd/v2-isolation

# 再验证 Agent 真实写入行为（需要模型）
PIG_VERIFY_MODEL=openai/gpt-5 go run ./cmd/v2-isolation
```

**预期结果**

| 检查 | 期望 |
|---|---|
| 两个 Services 的 CWD / AgentDir 不同 | PASS |
| A 的 workspace 文件不出现在 B | PASS |
| A 的 settings.json 不出现在 B | PASS |
| `DefaultAgentDir()` 未被显式参数污染 | PASS |
| Agent 写入的文件只落在各自 workspace | PASS / WARN |

**如果失败**

- `DefaultAgentDir` 被污染 → PiG 有全局状态，1 Pod N Session 不成立，降级为 1 Pod 1 Session。
- Agent 写入串目录 → 检查 `SessionStartOptions` 是否复用了同一个 `SessionManager` 或 `CWD`。
- 注意：本验证**不能**证明 `bash` 的越界访问被阻止——这是已知且接受的（同 Pod = 信任域）。

## V3：kill -9 后恢复

```bash
go build -o bin/v3-recovery ./cmd/v3-recovery

# 1. 建会话并挂起
./bin/v3-recovery -mode=create -out /tmp/pig-v3 -model=openai/gpt-5 &

# 2. 等它进入 idle（日志出现 session file 路径）后强杀
kill -9 %1

# 3. 纯文件层面检查日志完整性（无需模型）
./bin/v3-recovery -mode=inspect -session /tmp/pig-v3/session.jsonl

# 4. 尝试 resume 并继续一轮（需要模型）
./bin/v3-recovery -mode=resume -session /tmp/pig-v3/session.jsonl -model=openai/gpt-5
```

**预期结果**

- `inspect`：行数 > 0、无重复 `id`、无断链（每个 `parentId` 都能找到）、root 只有 1 个。
- `resume`：打开后消息数与中断前一致，新增一轮后 `totalMessages` 递增。

**如果失败**

- `inspect` 断链 → PiG 的 append 不是原子的，需要 Worker 侧写 WAL 或加锁。
- `resume` 打不开 → 记录实际错误，按 pin 的 commit 查 `Runtime.Open` 的真实签名；必要时改用文件型 `SessionManager`。

## 已知局限

1. 未编译验证，`API-CHECK` 处的签名可能随 0.x 变化。
2. V2 只验证显式路径隔离，不验证 extension 进程间的全局状态（需读 PiG 源码确认）。
3. V3 的 `kill -9` 是进程级，不覆盖节点断电；后者要靠 PVC/对象存储的 fsync 语义。
