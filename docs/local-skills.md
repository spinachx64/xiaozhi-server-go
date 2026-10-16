# 本地 Skill 注册与渐进式加载

Skill 是维护者编写的任务说明，MCP/本地工具负责实际操作。本实现复用现有 function calling 链路，不会执行 Skill 中的脚本。

## 目录与注册

在服务**启动工作目录**下创建 `skills/<目录>/SKILL.md`。不是相对于可执行文件定位。

```text
skills/
  time-assistant/
    SKILL.md
    references/
      format.md
```

```markdown
---
name: time-assistant
description: 查询当前日期、时间或星期，整理成简短语音回答。
---

# 时间助手

查询实时信息前先调用 local_get_time，不要猜测。
只有需要详细格式时才读取 references/format.md。
```

- `name` 必填、全局唯一，最多 64 个 ASCII 字符，使用小写字母、数字和单个连接符。
- `description` 必填，最多 1024 个字符，说明任务触发条件。
- YAML front matter 后的正文不能为空，单文件最多 64 KiB。
- 只扫描一级子目录。缺少 skills 目录或没有有效 Skill 时，不增加目录提示或加载工具。
- 无效文件、重名会记录警告，不影响其他有效技能；重名保留按目录名排序的第一个。
- 每个连接的本地 MCP 客户端启动时注册一次。修改正文/元数据后重新连接，必要时重启服务；参考文件按调用读取。

## 三层加载

1. **目录**：每次模型请求在 system 消息中附加名称和描述，不附加正文或参考资料。目录不写入短期记忆，切换角色后仍可重新组合。
2. **正文**：模型调用 `local_load_skill({"name":"time-assistant"})`，正文作为 tool 结果回传，再请求模型继续处理。
3. **参考资料**：模型在确有需要时调用 `local_read_skill_resource({"name":"time-assistant","path":"references/format.md"})`。

这里的渐进加载指**按需进入模型上下文**。注册时解析并缓存 SKILL.md，参考文件在调用时才读取。已加载内容经过现有短期历史裁剪后可能被移除，需要时可重新加载。不维护额外的永久“已激活技能”状态。

参考资料必须是技能目录内的普通 UTF-8 文本文件，最多 64 KiB。绝对路径、目录越界（含指向外部的符号链接）、二进制和过大文件会返回加载错误。技能名通过注册表查找，不作为路径拼接。

## 使用示例

仓库附带 `time-assistant`：启用已有 `local_mcp_fun` 中的 `time` 工具，连接设备后询问“使用时间助手告诉我今天星期几”。预期工具顺序为：

```text
local_load_skill(name="time-assistant")
→ local_get_time()
→ LLM 生成简短回答
```

当用户进一步询问完整播报格式时，模型可以按需加载 `references/format.md`。工具选择由模型决定，以上是预期行为，不是硬编码路由。

## 范围与限制

- 需要支持并传递 system 消息、tool calls 和 tool results 的模型适配器。DSH 当前忽略本地工具参数，不能直接使用该机制。
- 技能由服务器维护者安装，当前全连接可见；尚未实现按智能体授权或管理界面。
- 当前仅解释 `name`、`description`，其他 front matter 字段不具有授权效果。不要把 `allowed_tools` 等声明当作执行权限。
- 注册技能不会自动注册正文提到的业务工具，相关 MCP 必须另外配置。
- 没有脚本执行、联网安装、热更新或自主重试。正文应简短，把长资料放到 references 中。
- 继续复用现有 Agent Loop；多工具调用聚合、最大执行步数与端到端取消仍属于后续工作。

验证：`go test ./src/core/skill ./src/core/mcp ./src/core/chat ./src/core`。

## 远程 MCP 连接

运行环境必须能在 PATH 中找到 stdio 配置的 `command`（例如 `mcp-proxy`）；也可直接使用原生 Streamable HTTP，省去代理进程：

```json
{
  "mcpServers": {
    "zh-data": {
      "transport": "streamable_http",
      "url": "https://infra-data.xmkanshu.cn/mcp"
    }
  }
}
```

以上配置放在启动工作目录的 `.mcp_server_settings.json`。旧 SSE 服务使用 `transport: "sse"`；原 stdio 的 `command`、`args`、`env` 配置继续支持。URL 未指定 transport 时默认 Streamable HTTP。客户端会遍历工具列表分页，并将多段结果转换为对话循环可消费的文本。

远程工具在本项目中的名称带 `mcp_` 前缀，例如 `mcp_list_metrics`、`mcp_query_sensors_data`、`mcp_resolve_book`。Skill 中提到的原始工具名需与模型实际收到的工具定义对应。`agents/openai.yaml` 当前不会自动安装或连接依赖。

选择 `type: dsh` 时，仅最新用户文本发送给外部桥接，本项目的 Skill 目录、正文和 MCP 工具定义不会传过去。需要在 DSH 侧单独配置相应能力。
