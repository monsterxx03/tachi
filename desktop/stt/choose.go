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
func ParseTranscription(raw string) (Transcription, error) {
	var t Transcription
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return Transcription{}, fmt.Errorf("stt: 解析转写结果: %w", err)
	}
	return t, nil
}
