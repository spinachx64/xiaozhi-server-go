package configs

// DashScopeVoices is a conservative starter catalog for qwen3-tts-flash-realtime,
// matching the nine voices verified by the integration reference script.
// Name is the case-sensitive API ID; DisplayName is for selection and speech.
func DashScopeVoices() []VoiceInfo {
	return []VoiceInfo{
		{Name: "Cherry", DisplayName: "芊悦", Language: "普通话", Sex: "女", Description: "明快自然，适合日常聊天"},
		{Name: "Serena", DisplayName: "苏瑶", Language: "普通话", Sex: "女", Description: "柔和亲切，适合轻声交流"},
		{Name: "Ethan", DisplayName: "晨煦", Language: "普通话", Sex: "男", Description: "有活力的男声，略带北方口音"},
		{Name: "Chelsie", DisplayName: "千雪", Language: "普通话", Sex: "女", Description: "偏二次元风格的年轻女声"},
		{Name: "Dylan", DisplayName: "北京-晓东", Language: "北京话", Sex: "男", Description: "带北京口音的少年男声"},
		{Name: "Jada", DisplayName: "上海-阿珍", Language: "上海话", Sex: "女", Description: "爽快的上海方言女声"},
		{Name: "Sunny", DisplayName: "四川-晴儿", Language: "四川话", Sex: "女", Description: "亲切甜美的四川方言女声"},
		{Name: "Nofish", DisplayName: "不吃鱼", Language: "普通话", Sex: "男", Description: "带平舌特点的个性男声"},
		{Name: "Katerina", DisplayName: "卡捷琳娜", Language: "普通话", Sex: "女", Description: "成熟有韵律的女声"},
	}
}

func DefaultDashScopeTTS() TTSConfig {
	return TTSConfig{Type: "dashscope", URL: "wss://dashscope.aliyuncs.com/api-ws/v1/realtime",
		Model: "qwen3-tts-flash-realtime", Voice: "Cherry", Format: "wav", SampleRate: 24000,
		TimeoutSeconds: 60, OutputDir: "tmp/", SupportedVoices: DashScopeVoices()}
}

// WithTTSDefaults exposes the same default voice catalog to runtime routing as
// to the provider itself. A different model requires an explicit voice catalog.
func WithTTSDefaults(cfg TTSConfig) TTSConfig {
	if cfg.Type != "dashscope" {
		return cfg
	}
	defaults := DefaultDashScopeTTS()
	if cfg.Model == "" {
		cfg.Model = defaults.Model
	}
	if cfg.URL == "" {
		cfg.URL = defaults.URL
	}
	if cfg.Voice == "" {
		cfg.Voice = defaults.Voice
	}
	if cfg.OutputDir == "" {
		cfg.OutputDir = defaults.OutputDir
	}
	if cfg.Format == "" {
		cfg.Format = defaults.Format
	}
	if cfg.SampleRate == 0 {
		cfg.SampleRate = defaults.SampleRate
	}
	if cfg.TimeoutSeconds == 0 {
		cfg.TimeoutSeconds = defaults.TimeoutSeconds
	}
	if len(cfg.SupportedVoices) == 0 && cfg.Model == defaults.Model {
		cfg.SupportedVoices = DashScopeVoices()
	}
	return cfg
}
