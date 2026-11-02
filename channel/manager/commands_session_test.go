package manager

import (
	"os"
	"path/filepath"
	"testing"
)

// /cd 的 `~` 规则：`~` 与 `~/…` 展开成家目录，`~user` **不展开**。
//
// 后者是这条用例存在的理由：以前任何以 `~` 开头的参数都会被当成「家目录 + 剩下的部分」，
// 于是 `/cd ~bob/work` 变成 `$HOME/bob/work` —— 一个看起来像用户要去的、却几乎肯定不存在的
// 路径。`~user` 是 shell 的用户展开，Go 不知道别的用户的家目录，留着它当相对路径反而更诚实。
func TestExpandCDDirOnlyExpandsTheUsersOwnHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("这台机器解析不出家目录（%v），展开那一半没法断言", err)
	}

	expanded := []struct{ in, want string }{
		{"~", home},
		{"~/work", filepath.Join(home, "work")},
	}
	for _, c := range expanded {
		got, err := expandCDDir(c.in)
		if err != nil {
			t.Fatalf("expandCDDir(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("expandCDDir(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	unchanged := []string{"~bob/work", "~bob", "/abs/path", "relative/dir", ""}
	for _, in := range unchanged {
		got, err := expandCDDir(in)
		if err != nil {
			t.Fatalf("expandCDDir(%q): %v", in, err)
		}
		if got != in {
			t.Errorf("expandCDDir(%q) = %q, want it left alone", in, got)
		}
	}
}
