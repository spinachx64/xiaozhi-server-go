# LLM / VLLLM API Key 使用环境变量

LLM 和 VLLLM Provider 在创建时解析密钥引用，覆盖默认模型、视觉服务、智能体所选模型与后台供应商测试入口。文件和数据库仍保存原始引用，不保存解析后的环境变量值。

## 写法

```yaml
LLM:
  ChatGLMLLM:
    type: openai
    url: https://open.bigmodel.cn/api/paas/v4/
    model_name: glm-4-flash
    api_key: "${QM_ENV_KEY}"
```

或使用独立字段：

```yaml
    env_key: QM_ENV_KEY
```

启动后端前设置变量：

```bash
export QM_ENV_KEY='你的实际API密钥'
go run ./src/main.go
```

项目启动入口已有 `godotenv.Load()`，也可以在工作目录的 `.env` 写入 `QM_ENV_KEY=...`。进程已有环境变量优先，`.env` 不会覆盖它。Docker 等部署方式应把变量注入实际运行后端的容器/进程。

## 规则

优先级：非空 `env_key` → `api_key` 中的 `${NAME}` → 字面 API Key。

- 显式引用在变量不存在、为空或名称非法时初始化失败，不会把引用文本发送给模型服务。
- 裸名称（如 `api_key: QM_ENV_KEY`）**不再**解析，按字面密钥处理。旧配置请改成 `"${QM_ENV_KEY}"` 或 `env_key: QM_ENV_KEY`。
- 只有配置文件和后台供应商配置（管理员维护）会被解析。智能体 Extra 中的 `api_key` 覆盖一律按字面密钥处理，并忽略该 Provider 的 `env_key`，避免通过智能体配置读取服务进程的环境变量。

## 生效时机

配置文件用于首次初始化；已初始化实例在后台修改供应商时，也可以在 API 密钥字段填 `${NAME}`，或在高级 JSON 中填写 `env_key`。

密钥在每次创建 Provider 时解析，不在每次请求时重新读取。更新环境变量或 `.env` 后需重启后端、重连设备。

这不是全局 YAML 环境变量替换：不会展开提示词、URL 或 Coze PAT 等其他字段。阿里云 ASR/TTS 在 `api_key` 为空时读取 `DASHSCOPE_API_KEY`。

```bash
go test ./src/configs ./src/core/providers/llm/openai ./src/httpsvr/webapi
```
