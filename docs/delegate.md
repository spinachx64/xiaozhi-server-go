# 长任务委派（delegate → agent-bridge）

语音主线程只适合快速对话。联网检索、多步操作、读写文件、写代码这类耗时任务，交给独立的
[agent-bridge](https://github.com/spinachx64/agent-bridge) 服务，由它调度本机的 agent harness（目前是 Codex）执行。

```
设备 ──语音──▶ xiaozhi-server ──LLM 调用 local_delegate_task──▶ 立即回复“交给后台处理了”
                    │
                    │ WebSocket 协议 v1（task / progress / result / cancel / fetch / ack）
                    ▼
               agent-bridge ── adapter ──▶ Codex SDK（codex exec）
                    │   每设备一个会话，追问自动续接；结果落盘直到确认送达
                    ▼
xiaozhi-server 等设备空闲 ──▶ 主动开启新一轮播报结果，并写入对话历史
```

协议规范见 agent-bridge 仓库的 `docs/protocol.md`。

## 启用

1. 启动 agent-bridge（详见其 README）：

   ```bash
   git clone https://github.com/spinachx64/agent-bridge.git
   cd agent-bridge && npm install
   export AGENT_BRIDGE_TOKEN=$(openssl rand -hex 24)
   npm start
   ```

   本机 `codex` 需要已登录。默认沙箱为 `workspace-write`、审批策略 `never`，工作目录按设备隔离在 `data/devices/<id>/workspace`。

2. 在 xiaozhi-server 的运行环境里设置同一个 `AGENT_BRIDGE_TOKEN`，确认 `config.yaml`：

   ```yaml
   local_mcp_fun:
     - name: "delegate"
       enabled: true

   delegate:
     url: "ws://127.0.0.1:8787/ws"
     token: "${AGENT_BRIDGE_TOKEN}"
     timeout_seconds: 600
     max_concurrent: 2
   ```

   数据库里的 server_configs 会覆盖 `.config.yaml`。库里已有配置时，要在库里同步打开 `delegate`。

3. 给智能体加载 `skills/route-to-agent` Skill，它规定了哪些请求转交、哪些直接回答。

## 行为

- 提交后立刻播报“好的，这个任务我交给后台处理了，完成后告诉你”，本轮不再生成回答。
- 用户可以问进度（`local_task_status`）或取消（`local_cancel_task`）。
- 结果完成后，等设备连续空闲约 1 秒再播报；用户此时开口会打断播报，结果仍在对话历史里，可以追问。
- 同一设备的后续任务续接上一次的 Codex 会话（默认 12 小时内）。
- 结果在 bridge 落盘，播报后才确认删除。xiaozhi-server 重启或任务连接断开后，设备重连时会补取并播报（保留 24 小时）。

## 风险与限制

- 服务端主动播报（设备空闲或正在聆听时）尚未在官方固件上实测。
- 播报内容是截断后的原文（最多 600 字，去掉代码块），没有经过主 LLM 转述。
- bridge 与 xiaozhi-server 之间没有 TLS，bridge 默认只监听 127.0.0.1。跨机器部署时请自行加 TLS 或走内网隧道。
- 无头模式下没人能审批，需要审批的操作会直接失败。不建议把沙箱改成 `danger-full-access`：语音可能被误识别，也没有人能当场拦截。
- 每个任务都会启动一次 `codex exec`，冷启动多 1～3 秒。
