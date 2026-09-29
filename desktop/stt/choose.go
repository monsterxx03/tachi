package stt

import (
	"encoding/json"
	"fmt"
	"unicode"
)

// Transcription 是两个转写模块对**同一段音频**各自给出的结果。
//
// 为什么要两个：中文句子里嵌的英文词只有 zh-CN 读得出来（en-US 读中文是垃圾），而纯英文
// 的句子 zh-CN 会给出音译垃圾。所以两边各自跑完再**选**一个——注意不是「融合」，那做不到：
// SpeechDetector 只报有语音/无语音，不判语种（设计文档 §10）。
type Transcription struct {
	ZH     string  `json:"zh"`
	ZHConf float64 `json:"zhConf"`
	EN     string  `json:"en"`
	ENConf float64 `json:"enConf"`

	// Missing 列的是这一次**没跑成**的模块（按 locale 报，例如 "en-US"），不管什么原因。
	Missing []string `json:"missing"`
	// Installing 是 Missing 里那些**系统说缺离线资产**的（子集），垫片已经发起安装。
	// 与 Missing 分开是必须的：缺资产会自己好，别的不会——屏幕上要对用户说的是不同的话。
	Installing []string `json:"installing"`
	// Error 是「一门都没跑成」时垫片带回来的原因原文，例如
	// `SFSpeechErrorDomain 4: No GeneralASR asset for language en`。
	Error string `json:"error"`
}

// Result 是一次转写的结果。
//
// 文本和「哪几门没用上」是两件事，所以不压进一个字符串：缺资产的降级必须能被看见
// （desktop/badge.go 会把它写进日志、并据此决定跟用户说什么），而不能悄悄给一句
// 质量更差的文本。
type Result struct {
	// Text 是转写出来的那句话。空文本不是错误——那多半是「按下了但没说话」。
	Text string
	// Missing 列的是这一次没用上的模块（按 locale 报）：它们没跑成。非空时 Text 只
	// 来自剩下的那门语言。
	Missing []string
	// Installing 是 Missing 里那些缺离线资产的——它们已经在后台装，下一次说话可能
	// 就好了。它决定屏幕上那句「为什么没了」怎么说（见 desktop/badge.go）。
	Installing []string
}

// Choose 是设计文档 §10 那条判据：**zh-CN 的结果里有汉字就用它**（覆盖绝大多数输入），
// 一个汉字都没有时，才跟 en-US 比置信度。
//
// 为什么不是单比置信度：置信度说的是「我说得对不对」，不是「我听全了没有」。实测里出现过
// en-US 只抓到半句却给出 0.67 的情形（zh-CN 0.80，只差 0.13），而那半句是
// 「Hi, R S S I, her battery level. Hi」——残缺却自信。汉字那条硬规则不靠裕度。
//
// 反过来说，第二层是**脆的**：纯英文句上两个置信度可能几乎并列，而 zh-CN 那串是音译垃圾
// 却险胜（探针第 5 轮复现过一次，见 choose_test.go 里那条被标出来的用例）。设备上大概率的
// 输入是中文，这个代价可以接受，但别把第二层当成可靠判据。
func Choose(t Transcription) string {
	if hasHan(t.ZH) {
		return t.ZH
	}
	if t.ENConf > t.ZHConf {
		return t.EN
	}
	return t.ZH
}

// hasHan 判断一段文本里有没有汉字。
//
// 按码点范围判，而不是「有没有非 ASCII」：标点、全角字母、日文假名都不是汉字，而它们
// 出现在 zh-CN 的输出里并不说明它听对了。只有汉字本身是那条硬规则的依据。
func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// ParseTranscription 解析垫片交回来的那行 JSON（见 desktop/stt/shim.swift）。
//
// 垫片认得两种「没成」：整次失败（error 非空）和降级（少了一门，见 Missing）。前者在这里
// 变成 error，后者不是——降级的结果照样能用，只是得让调用方知道。
func ParseTranscription(raw string) (Transcription, error) {
	var t Transcription
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return Transcription{}, fmt.Errorf("stt: 解析转写结果: %w", err)
	}
	if t.Error != "" {
		// 原文照搬，不再压成一句「转写失败」：这句话的用处就是让「哪门语言缺资产」和
		// 「引擎坏了」分得开（后者不会自己好）。
		return t, fmt.Errorf("stt: %s", t.Error)
	}
	return t, nil
}
