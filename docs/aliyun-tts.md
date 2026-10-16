# 阿里云百炼 TTS（Qwen3-TTS Realtime）

本实现对应 `qwen3-tts-flash-realtime`，使用 Qwen Realtime WebSocket 事件协议，与 ASR 的 Paraformer `run-task` 协议不同，也不是 CosyVoice 接口。

## 配置与启用

公开 `config.yaml` 已添加 `TTS.AliyunTTS` 完整模板及九个音色。首次启动填写 `.config.yaml`：

```yaml
TTS:
  AliyunTTS:
    type: dashscope
    api_key: "" # 留空使用 DASHSCOPE_API_KEY，显式配置优先
    url: wss://dashscope.aliyuncs.com/api-ws/v1/realtime
    model: qwen3-tts-flash-realtime
    voice: Cherry
    format: wav
    sample_rate: 24000
    timeout_seconds: 60
    output_dir: tmp/

selected_module:
  TTS: AliyunTTS
```

上面省略的 `supported_voices` 对默认模型会自动补齐；公开模板中已显式列出。请保留其他 ASR、LLM 等配置。

已运行的实例优先读取数据库：后台“模型供应商”添加 TTS →“阿里云百炼 Qwen3-TTS”，填写密钥、模型、地址并选择音色；然后在“系统配置”选择该 TTS，保存后重启、重连设备。密钥也可以通过进程环境变量 `DASHSCOPE_API_KEY` 提供，与 ASR 共用。

`url` 使用 `/api-ws/v1/realtime`，模型自动写入 URL 的 `model` 查询参数。切区域时自行填写对应地址并使用对应区域密钥，例如 `wss://dashscope-intl.aliyuncs.com/api-ws/v1/realtime`。

## 音色配置

`voice` 是大小写敏感的 API ID。`supported_voices` 中 `name` 也是 API ID，`display_name` 是后台显示和语音选择名称。中文显示名会转换为 API ID 后发送。

| voice / name | display_name | 主语言标签 |
| --- | --- | --- |
| Cherry | 芊悦 | 普通话 |
| Serena | 苏瑶 | 普通话 |
| Ethan | 晨煦 | 普通话 |
| Chelsie | 千雪 | 普通话 |
| Dylan | 北京-晓东 | 北京话 |
| Jada | 上海-阿珍 | 上海话 |
| Sunny | 四川-晴儿 | 四川话 |
| Nofish | 不吃鱼 | 普通话 |
| Katerina | 卡捷琳娜 | 普通话 |

这是参考脚本验证过的初始列表，不是云端所有音色的全集，也不是所有阿里云模型通用的列表。语言标签主要供后台分组，不会强制设置云端合成语言。

可在后台高级 JSON 中扩充或替换 `supported_voices`，例如：

```json
{
  "type": "dashscope",
  "model": "qwen3-tts-flash-realtime",
  "voice": "Serena",
  "supported_voices": [
    {"name": "Cherry", "display_name": "芊悦", "language": "普通话", "sex": "女", "description": "日常交流"},
    {"name": "Serena", "display_name": "苏瑶", "language": "普通话", "sex": "女", "description": "柔和女声"}
  ]
}
```

该片段需与现有凭据、地址等字段合并。修改高级 JSON 后离开输入框会刷新音色下拉选项。列表中的 API ID 必须唯一；配置或切换的音色必须在列表中，非法音色不会悄悄回退成另一个声音。添加云端新音色后可通过后台试听确认账户、模型及区域支持情况。

切换为其他 Qwen Realtime TTS 模型时，必须显式配置该模型支持的 `supported_voices` 和默认 `voice`，本实现不为未知模型套用这九个音色。`Raymond` 不在初始列表中，参考脚本实测该默认模型不支持它。

智能体可将 `Voice` 设置为例如 `Serena`。默认 TTS 不是阿里云时，现有路由会根据配置中的 `supported_voices` 寻找匹配 Provider；跨 Provider 使用的 ID 应避免重名歧义。

若要通过语音切换音色，选 `AliyunTTS` 为全局默认，并在现有 `local_mcp_fun` 列表中启用已有工具（已初始化实例通过配置管理接口保存）：

```yaml
local_mcp_fun:
  - name: change_voice
    description: 切换当前合成音色
    enabled: true
```

保留列表中其他工具。重启、重连后可询问“换成苏瑶的声音”，模型应调用 `local_change_voice`，将 `Serena` 或中文显示名传给 Provider。该工具切换当前 Provider 的音色，不负责跨 Provider 切换。

## 合成与播放

连接后等待 `session.created`，发送带音色的 `session.update` 并等待 `session.updated`，随后追加文本、发送 `session.finish`。读取 `response.audio.delta` 的 Base64 PCM，直到 `session.finished`。错误、异常断连、空音频、非法 Base64 或未完成 PCM 均返回失败，不返回部分文件。

云端输出为单声道 24kHz PCM16，小端裸流。目前只接受 `format: wav` 和 `sample_rate: 24000`：这是文件接口/录音的格式；设备协商的下行编码仍是 PCM 或 Opus。

设备对话优先使用可选 `PCMStreamingTTS.StreamPCM` 接口：第一批云端音频到达后即可分帧下发，不等待整句合成或 WAV 落盘。沿用已有 `tts.start`、`sentence_start`、二进制音频帧、`sentence_end`、`tts.stop` 消息；codec 元数据与协商编码一致。跨云端分片保留未满一帧的样本，只在真正 EOF 时补齐尾帧；Opus 在整句中复用同一个编码器，不为每个 delta 重建编码器。

音频发送仍串行处理句子，按协商帧时长控制发送节奏，最多预送三帧，PCM 分片队列容量为四个云端 delta，避免无限缓冲。WAV 结果并行保留给录音、快速回复缓存和原有文件接口；Opus 抓包功能继续支持。命中缓存时走原文件路径，其他未实现流式接口的 Provider 保持文件合成模式。

流式模式中，后续云端错误无法撤回已经播放的声音；会停止该音频任务，不把未完成结果保存成成功文件。后台试听 `ToTTSWithContext` 仍等待完整 WAV 后返回。

文本生成和音频播放协调结束：首句播完但 LLM 仍在输出时，不会提前发送整轮 `tts.stop` 或重置 ASR。

`timeout_seconds` 是整次合成的最长时间（1–600 秒）。设备打断、新轮次或断开连接会取消流式/支持 context 的合成；当前正在等待云端的旧请求不再占住下一句 TTS。后台试听也传递请求 context。音色变更只影响后续合成，正在进行的合成使用开始时的音色快照。

## 验证

```bash
go test -race ./src/core ./src/core/utils ./src/core/providers/asr/dashscope ./src/core/providers/tts/dashscope
```

本地模拟 WebSocket 测试涵盖生产工厂配置映射、鉴权、音色、事件顺序、Unicode 分段、WAV 播放、服务错误、取消和清理；另将云端 `session.finished` 故意阻塞，验证设备已经收到可解码的 PCM/Opus 首帧。还验证打断后下一轮能继续、ASR 开始/停止 FIFO、首句结束不会提前停止整轮。这些测试验证本地处理顺序，不代表真实网络或设备的端到端耗时。

配置真实密钥后，在后台供应商卡片点击“测试”试听，再测试设备播报和音色切换。

参考：[官方音色表](https://help.aliyun.com/zh/model-studio/qwen-tts-voice-list)、[客户端事件](https://help.aliyun.com/zh/model-studio/qwen-tts-realtime-client-events)、[服务端事件](https://help.aliyun.com/zh/model-studio/qwen-tts-realtime-server-events)。
