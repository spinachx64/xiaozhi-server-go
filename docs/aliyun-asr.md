# 阿里云百炼 ASR（DashScope / Paraformer）

接入的是百炼 `paraformer-realtime-v2` 实时识别，不是传统智能语音交互 NLS（AppKey/AccessKey）接口。Go 直接调用 WebSocket，不依赖 Python 或 DashScope SDK。

## 配置

首次初始化时，在 `.config.yaml` 的 `ASR` 分组增加配置（公开 `config.yaml` 已提供模板）：

```yaml
ASR:
  AliyunASR:
    type: dashscope
    api_key: "" # 填百炼 API Key，或留空使用 DASHSCOPE_API_KEY
    asr_url: wss://dashscope.aliyuncs.com/api-ws/v1/inference
    model: paraformer-realtime-v2
    format: pcm
    sample_rate: 16000
    language_hints: [zh, en]
    semantic_punctuation_enabled: false
    max_sentence_silence: 300
    timeout_seconds: 30

selected_module:
  ASR: AliyunASR
```

请保留 `selected_module` 中其他 LLM、TTS 等选项。模板不会自动切换现有默认 ASR。

已经初始化数据库的实例，通过后台“模型供应商”添加 ASR →“阿里云百炼 Paraformer”，然后在“系统配置”选择 `AliyunASR`，保存后重启、重连设备。直接修改配置模板不会覆盖数据库。

环境变量方式：

```bash
export DASHSCOPE_API_KEY='你的百炼 API Key'
go run ./src/main.go
```

显式 `api_key` 优先于环境变量。国际站应同时使用对应区域密钥和 WebSocket 地址，例如 `wss://dashscope-intl.aliyuncs.com/api-ws/v1/inference`。

可通过高级 JSON 配置 `phrase_id`、`disfluency_removal_enabled`、`diarization_enabled`、`speaker_count`、`timestamp_alignment_enabled`、`special_word_filter`、`audio_event_detection_enabled`、`semantic_punctuation_enabled`、`max_sentence_silence`，这些字段原样传入 API 的 `parameters`。本地只输出识别文本，不输出说话人及词级时间戳。`language_hints` 在本实现中仅接受 `paraformer-realtime-v2`；使用其他模型请移除此字段。

## 音频与结果处理

- 输入是单声道、有符号 16 位小端 PCM。设备 Opus 先由现有连接层解码，不带 WAV 文件头，也不做 Base64。
- `sample_rate` 是独立调用或后台测试时的默认值。设备开始拾音时，按 hello 声明的实际采样率设置 API 参数；不偷偷重采样。默认模型 v2 支持不同采样率，其他模型需自行确认采样率要求。
- 等待 `task-started` 后才发送音频；输入完成发送 `finish-task`。
- `listen.start`、音频帧和 `listen.stop` 在同一个 FIFO 中处理；开始拾音时就预连接云端，结束信号不会越过排队的尾部音频。
- 默认 v2 使用 VAD 断句，静音阈值为 300ms（可配置 200–6000ms）。这是断句阈值，不是端到端延迟保证；说话停顿较长时可调高。官方默认 1300ms 更偏向完整收句，本项目为对话场景降低该值。
- 中间识别结果和 heartbeat 不提交给 LLM，最终句按 `sentence_id`（缺失时按 `begin_time`）去重。
- `auto/realtime`：第一条非空最终句结束本轮，发送 `finish-task` 后关闭连接并回调，避免等待完整任务结束增加响应延迟。
- 完成后的输入尾帧不会自动开启新任务，避免同一轮误触发重复识别。新拾音或重置后才恢复接收。
- `realtime` 模式一旦收到首个非空中间识别结果，就可打断播音和合成；此时不重置正在识别的任务，不把不稳定的中间文本提交给 LLM，仍等待最终句。
- `manual`：等待客户端 stop 和服务端 `task-finished`，合并所有最终句，只回调一次。
- `Transcribe(ctx, audio)` 接受配置采样率的原始 PCM，以 100ms 帧发送，等待完整任务结束并返回所有最终句；不是文件路径/WAV 解码接口。
- `timeout_seconds` 控制握手、启动等待、读写空闲超时（1–600 秒）。同步调用还受传入的 context 控制。重置/关闭会终止当前 socket，旧任务不会关闭新任务的连接。

## 验证

```bash
go test -race ./src/core/providers/asr/dashscope
```

测试使用本地 WebSocket 模拟服务验证鉴权头、请求顺序、PCM 二进制帧、增量去重、多句汇总、自动断句、错误及取消，不消耗云端额度。

配置真实凭据后，可在后台供应商卡片点击“测试”，用内置 16k PCM 测试音频验证真实服务，再用设备测试自动/手动拾音。

协议参考：[Paraformer 客户端事件](https://help.aliyun.com/zh/model-studio/paraformer-client-events)、[WebSocket 调用流程](https://help.aliyun.com/zh/model-studio/websocket-for-paraformer-real-time-service)。
