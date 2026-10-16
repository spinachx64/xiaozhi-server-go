# LLM API Key 使用环境变量

LLM Provider 创建时解析密钥引用，覆盖默认模型、智能体模型与后台供应商测试入口。文件和数据库仍保存原始引用，不保存解析后的环境变量值。

## 直接填写环境变量名

```yaml
LLM:
  ChatGLMLLM:
    type: openai
    url: https://token-hub.qiseek.com/v1
    model_name: 你的模型名称
    api_key: QM_ENV_KEY
```

启动后端前设置变量：

```bash
export QM_ENV_KEY='你的实际API密钥'
go run ./src/main.go
```

项目启动入口已有 `godotenv.Load()`，因此也可以在工作目录的 `.env` 文件写入：

```dotenv
QM_ENV_KEY=你的实际API密钥
```

进程已有环境变量优先，`.env` 不会覆盖它。Docker 等部署方式应把变量注入实际运行后端的容器/进程。

裸变量名的规则：同名环境变量**存在**时读取其值；存在但为空时报错。变量不存在时继续把 `api_key` 当作字面密钥，以兼容旧配置。若希望变量缺失时立即报错，使用下面的显式形式。

## 显式引用与优先级

```yaml
api_key: "${QM_ENV_KEY}"
```

或：

```yaml
env_key: QM_ENV_KEY
```

优先级：非空 `env_key` → `api_key` 中的 `${NAME}` → 已存在的同名环境变量 → 字面 API Key。显式引用在变量不存在、为空或名称非法时初始化失败，不会把引用文本发送给模型服务。

真实密钥直接填入 `api_key: sk-...` 的用法保持兼容。智能体 Extra 中的 `api_key` 覆盖也支持相同解析规则，并优先于该 Provider 的全局 `env_key`。

## 生效时机

配置文件用于首次初始化；已初始化实例通过后台修改供应商，仍可把环境变量名或 `${NAME}` 写进 API 密钥字段。`env_key` 可在高级 JSON 中填写。

密钥在每次创建 Provider 时解析，不在每次请求时重新读取。更新服务环境变量或 `.env` 后重启后端、重连设备；已经创建的 Provider 保留创建时的密钥快照。已有短期记忆不包含解析后的密钥。

本兼容针对 LLM 的 `api_key`，不是全局 YAML 环境变量替换；不会扩展任意提示词、URL 或 Coze 的 PAT 等独立认证字段。VLLLM 尚未接入此解析入口。

本地 HTTP 测试会验证实际 Authorization 头、环境变量更新后的新 Provider、字面密钥兼容，以及原始配置未被替换：

```bash
go test ./src/configs ./src/core/providers/llm/openai ./src/httpsvr/webapi
```
