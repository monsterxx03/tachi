package stt

import (
	"slices"
	"strings"
	"testing"
)

// 这 18 条不是编出来的：它们是探针第 5 轮
// （ai-passport tools/stt-probe/round5_language_choice.swift，日志 logs/round5.log）
// 在真机上跑出来的原文与置信度——六句话 × 三种音频。这条判据只被真机数据检验过这一次，
// 所以逐条钉在这里，**包括那条错的**。
//
// 为什么值得把一整张表抄进来：判据的两层各有一个非平凡的边界。
//
//   - 「汉字优先」不是保险而是主要依据：第 9 条里 en-US 的置信度（0.35）比 zh-CN（0.09）
//     高得多，单比置信度就会选错，而 zh-CN 那句是听得对的。
//   - 第二层是脆的：第 14 条（纯英文句）两个置信度只差 0.01，zh-CN 那串是音译垃圾却险胜。
//     它在这里不是「待修」的标记，而是这条判据边界的证据——要修它得换一套判据（比如英文
//     词典），而设备上大概率的输入是中文。
var chooseCases = []struct {
	name string
	got  Transcription
	want string
}{
	{
		name: "中文句 / 16k 无损",
		got: Transcription{
			ZH: "帮我把这个改动提交一下 ，顺便写一句 commitmessage。", ZHConf: 0.90,
			EN: ", commute message.", ENConf: 0.01,
		},
		want: "帮我把这个改动提交一下 ，顺便写一句 commitmessage。",
	},
	{
		name: "中文句 / 16k ADPCM",
		got: Transcription{
			ZH: "帮我把这个改动提交一下 ，顺便写一句 commutmessage。", ZHConf: 0.92,
			EN: ", commute message.", ENConf: 0.01,
		},
		want: "帮我把这个改动提交一下 ，顺便写一句 commutmessage。",
	},
	{
		name: "中文句 / 8k ADPCM",
		got: Transcription{
			ZH: "帮我把这个改动提交一下 ，顺便写一句 commitmessage。", ZHConf: 0.85,
			EN: "Commit message.", ENConf: 0.06,
		},
		want: "帮我把这个改动提交一下 ，顺便写一句 commitmessage。",
	},
	{
		name: "中英混说 / 16k 无损",
		got: Transcription{
			ZH: "把这个 commit revert掉 ，然后 reround一下测试最后 pushop man。", ZHConf: 0.58,
			EN: "Magic, commit river, yang ho, rerong, yang, sushi, tui ho, tu, shao, main.", ENConf: 0.01,
		},
		want: "把这个 commit revert掉 ，然后 reround一下测试最后 pushop man。",
	},
	{
		name: "中英混说 / 16k ADPCM",
		got: Transcription{
			ZH: "把这个 commit rivert掉 ，然后 rerom一下测试最后 pushop main。", ZHConf: 0.65,
			EN: "Magic, commit, river, yao, yang, ho, ri, yang, ho.", ENConf: 0.02,
		},
		want: "把这个 commit rivert掉 ，然后 rerom一下测试最后 pushop main。",
	},
	{
		name: "中英混说 / 8k ADPCM",
		got: Transcription{
			ZH: "把这个 commit reverte掉 ，然后 ream一下测试最后通时到内。", ZHConf: 0.25,
			EN: "", ENConf: 0.00,
		},
		want: "把这个 commit reverte掉 ，然后 ream一下测试最后通时到内。",
	},
	{
		name: "技术词 / 16k 无损",
		got: Transcription{
			ZH: "看一下 Bec的 RSSI和 Bentrylevel还有什么连接间隔。", ZHConf: 0.35,
			EN: "Hi Yisian, Ben, the R S S I, her battery level. Hi, you, family.", ENConf: 0.06,
		},
		want: "看一下 Bec的 RSSI和 Bentrylevel还有什么连接间隔。",
	},
	{
		name: "技术词 / 16k ADPCM",
		got: Transcription{
			ZH: "看一下 Bec的 RSSI和 Benterylevel还有连接间隔。", ZHConf: 0.44,
			EN: "Hi Yi Xia, Ben, R S S I, her battery level. Hi, you, Lian Jian Jiang.", ENConf: 0.07,
		},
		want: "看一下 Bec的 RSSI和 Benterylevel还有连接间隔。",
	},
	{
		// 这一条是「汉字优先」为什么必需：en-US 的置信度比 zh-CN 高得多（0.35 对 0.09），
		// 但 zh-CN 那句是听得对的，而 en-US 只抓到半句。
		name: "技术词 / 8k ADPCM（置信度会选错的那条）",
		got: Transcription{
			ZH: "看一下单 RSSI和 betarylevel还有连接间隔。", ZHConf: 0.09,
			EN: "Hi, R S S I, her battery level. Hi", ENConf: 0.35,
		},
		want: "看一下单 RSSI和 betarylevel还有连接间隔。",
	},
	{
		name: "纯中文 / 16k 无损",
		got: Transcription{
			ZH: "先跑一下 Lint，然后 make trest确认没有回归。", ZHConf: 0.66,
			EN: "Xian pao yi Xian ling, Yang ho make chest.", ENConf: 0.16,
		},
		want: "先跑一下 Lint，然后 make trest确认没有回归。",
	},
	{
		name: "纯中文 / 16k ADPCM",
		got: Transcription{
			ZH: "先跑一下 Lint，然后 make trest确认没有回归。", ZHConf: 0.76,
			EN: "Xian pao yi Xian ling, Yang ho make chest chi meo Hui Gui.", ENConf: 0.04,
		},
		want: "先跑一下 Lint，然后 make trest确认没有回归。",
	},
	{
		name: "纯中文 / 8k ADPCM",
		got: Transcription{
			ZH: "先跑一下 Lint，然后 maketes确认没有回归。", ZHConf: 0.79,
			EN: "Xian pao yi Xian ling, Yang ho make.", ENConf: 0.11,
		},
		want: "先跑一下 Lint，然后 maketes确认没有回归。",
	},
	{
		name: "英文句 / 16k 无损",
		got: Transcription{
			ZH: "Pleaer tha comitenyron the test.", ZHConf: 0.54,
			EN: "Please revert that commitment and rerun the tests.", ENConf: 0.70,
		},
		want: "Please revert that commitment and rerun the tests.",
	},
	{
		// ⚠ 已知的失败：两个置信度只差 0.01，而 zh-CN 那串是音译垃圾。判据的第二层在纯
		// 英文句上就是脆的——这里断言的是**当前行为**（选 zh），不是为了让它永远错，而是
		// 让「有朝一日换了判据」这件事**看得见**。
		name: "英文句 / 16k ADPCM（判据险胜错的那条）",
		got: Transcription{
			ZH: "Plaser that comitenyroun the test.", ZHConf: 0.56,
			EN: "Please revert that commit, then rerun the tests.", ENConf: 0.55,
		},
		want: "Plaser that comitenyroun the test.",
	},
	{
		name: "英文句 / 8k ADPCM",
		got: Transcription{
			ZH: "Please revert tht commitenyoneher test.", ZHConf: 0.34,
			EN: "Please revert that commitment and rerun the test.", ENConf: 0.57,
		},
		want: "Please revert that commitment and rerun the test.",
	},
	{
		name: "英文句 2 / 16k 无损",
		got: Transcription{
			ZH: "List te change files and show me the dif", ZHConf: 0.58,
			EN: "List the change files and show me the div.", ENConf: 0.85,
		},
		want: "List the change files and show me the div.",
	},
	{
		name: "英文句 2 / 16k ADPCM",
		got: Transcription{
			ZH: "Liste change files and chol me the dif", ZHConf: 0.55,
			EN: "List the change files and show me the div.", ENConf: 0.78,
		},
		want: "List the change files and show me the div.",
	},
	{
		name: "英文句 2 / 8k ADPCM",
		got: Transcription{
			ZH: "Liste change files and show me the de.", ZHConf: 0.57,
			EN: "List the change files and show me the diff.", ENConf: 0.78,
		},
		want: "List the change files and show me the diff.",
	},
}

func TestChooseAgainstTheProbeData(t *testing.T) {
	for _, c := range chooseCases {
		if got := Choose(c.got); got != c.want {
			t.Errorf("%s:\n  选出了 %q\n  期望是 %q", c.name, got, c.want)
		}
	}
}

// 垫片返回的那行 JSON 必须能解开（两边字段名对不上时，这里会立刻红，而不是等到真机上
// 「转写永远返回空」）。
func TestParseTranscription(t *testing.T) {
	raw := `{"zh":"看一下 badge 的 RSSI","zhConf":0.8,"en":"Hi, R S S I","enConf":0.67}`

	got, err := ParseTranscription(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.ZH != "看一下 badge 的 RSSI" || got.ZHConf != 0.8 {
		t.Errorf("中文那半解错了: %+v", got)
	}
	if got.EN != "Hi, R S S I" || got.ENConf != 0.67 {
		t.Errorf("英文那半解错了: %+v", got)
	}

	// 坏数据要报错，而不是给一个全零的结构体——那看起来像「人没说话」。
	if _, err := ParseTranscription("{oops"); err == nil {
		t.Error("坏 JSON 没有被拒绝")
	}
}

// 汉字判据的边界：标点、全角字母、假名都不算汉字。把它们当汉字会让 zh-CN 的垃圾输出
// （比如一整串音译）被选中。
// ParseTranscription 的**结果形状**：正常、降级（少了一门，但话还能用）、整次失败带原文。
// 字段名与坏 JSON 那两条属于上一个测试（TestParseTranscription）的事，这里不重复。
//
// 第二条最容易写错：降级**不是** error。把它当 error，一句能用的转写就被丢掉了——而那
// 句话恰恰是垫片逐门兜底才换回来的（见 shim.swift 的 transcribe）。
func TestParseTranscriptionOutcomes(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantText string
		wantMiss []string
		wantInst []string
		wantErr  string // 空串表示期望没有错误
	}{
		{
			name:     "两个模块都跑成",
			raw:      `{"zh":"提交一下","zhConf":0.9,"en":"commit","enConf":0.2,"missing":[],"installing":[]}`,
			wantText: "提交一下",
		},
		{
			name:     "降级：英文那门没跑成，中文的话照旧能用",
			raw:      `{"zh":"提交一下","zhConf":0.9,"en":"","enConf":0,"missing":["en-US"],"installing":["en-US"]}`,
			wantText: "提交一下",
			wantMiss: []string{"en-US"},
			wantInst: []string{"en-US"},
		},
		{
			name:     "听清了但没人说话：空文本不是错误",
			raw:      `{"zh":"","zhConf":0,"en":"","enConf":0,"missing":[],"installing":[]}`,
			wantText: "",
		},
		{
			name: "整次失败：原文原样带出来，同时留着「哪几门没跑成」",
			raw: `{"zh":"","zhConf":0,"en":"","enConf":0,"missing":["zh-CN","en-US"],` +
				`"installing":["en-US"],"error":"SFSpeechErrorDomain 4: No GeneralASR asset for language en"}`,
			wantErr:  "No GeneralASR asset for language en",
			wantMiss: []string{"zh-CN", "en-US"},
			wantInst: []string{"en-US"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTranscription(tc.raw)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("不该有错误: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v，期望包含 %q", err, tc.wantErr)
			}
			if text := Choose(got); text != tc.wantText {
				t.Errorf("Choose = %q，期望 %q", text, tc.wantText)
			}
			if !slices.Equal(got.Missing, tc.wantMiss) {
				t.Errorf("Missing = %v，期望 %v", got.Missing, tc.wantMiss)
			}
			if !slices.Equal(got.Installing, tc.wantInst) {
				t.Errorf("Installing = %v，期望 %v", got.Installing, tc.wantInst)
			}
		})
	}
}

func TestHasHan(t *testing.T) {
	yes := []string{"中文", "commit 提交", "汉字"}
	no := []string{"", "hello", "！？。，", "ＡＢＣ", "ひらがな", "please revert that commit"}

	for _, s := range yes {
		if !hasHan(s) {
			t.Errorf("hasHan(%q) = false，期望 true", s)
		}
	}
	for _, s := range no {
		if hasHan(s) {
			t.Errorf("hasHan(%q) = true，期望 false", s)
		}
	}
}
