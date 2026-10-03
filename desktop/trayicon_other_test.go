//go:build !darwin

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// TestTintTrayIconKeepsAlphaAppliesColour：Linux 上托盘图标是「保留形状、换颜色」。
//
// 形状全在 alpha 通道里（menuicon/ 那些 PNG 是黑色单色图，见 trayicon_other.go 的说明），
// 所以染色必须只动 RGB、原样留下 alpha——这一条把这两件事钉住，别的一概不管。
func TestTintTrayIconKeepsAlphaAppliesColour(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.SetNRGBA(0, 0, color.NRGBA{A: 0xff})          // 不透明的「笔画」
	src.SetNRGBA(1, 0, color.NRGBA{R: 0, G: 0, B: 0}) // 全透明（形状之外）
	var raw bytes.Buffer
	if err := png.Encode(&raw, src); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}

	want := color.NRGBA{R: 0x4c, G: 0x9a, B: 0xf0, A: 0xff}
	out, err := png.Decode(bytes.NewReader(tintTrayIcon(raw.Bytes(), want)))
	if err != nil {
		t.Fatalf("decode tinted: %v", err)
	}

	r, g, b, a := out.At(0, 0).RGBA()
	if a>>8 != 0xff {
		t.Errorf("不透明像素的 alpha 丢了：%v", a>>8)
	}
	if r>>8 != uint32(want.R) || g>>8 != uint32(want.G) || b>>8 != uint32(want.B) {
		t.Errorf("像素 = #%02x%02x%02x，想要 #%02x%02x%02x",
			r>>8, g>>8, b>>8, want.R, want.G, want.B)
	}
	if _, _, _, a := out.At(1, 0).RGBA(); a != 0 {
		t.Errorf("透明像素被染色了：alpha=%v", a)
	}
}

// TestTintTrayIconFallsBackOnGarbage：解不开的输入原样返回。
//
// 这不是「防御性编程」而是取舍：调用方（applyTrayIcon）没有别的地方兜底，一个黑色的图标
// 也好过托盘里什么都没有。
func TestTintTrayIconFallsBackOnGarbage(t *testing.T) {
	in := []byte("not a png")
	if got := tintTrayIcon(in, color.NRGBA{R: 1, A: 0xff}); !bytes.Equal(got, in) {
		t.Error("解不开的输入应原样返回")
	}
}
