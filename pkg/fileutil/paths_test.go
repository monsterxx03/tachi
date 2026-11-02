package fileutil

import (
	"os"
	"path/filepath"
	"testing"
)

// ExpandHome 是「`~` 是 shell 语法，不是 Go 的」这件事唯一的实现：一个 "~/workspace" 直接交给
// os.MkdirAll 会在工作目录里造一个名叫 "~" 的目录，看起来还挺像故意的。
//
// 断言的是**规则**，不是某个人的家目录：能解析就以 os.UserHomeDir 为参照比较。
func TestExpandHomeExpandsOnlyALeadingTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("这台机器解析不出家目录（%v），规则里的另一半没法断言", err)
	}

	cases := []struct{ in, want string }{
		{"~", home},
		{"~/workspace", filepath.Join(home, "workspace")},
		{"~/.tachi/config.yaml", filepath.Join(home, ".tachi", "config.yaml")},
		// 以下四个都**不该**被展开：中间的 ~ 是普通字符，`~user` 是另一套 shell 语义。
		{"", ""},
		{"a/~/b", "a/~/b"},
		{"~user/x", "~user/x"},
		{"/abs/~/x", "/abs/~/x"},
	}
	for _, c := range cases {
		if got := ExpandHome(c.in); got != c.want {
			t.Errorf("ExpandHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// IsSinglePathElement 是「一个名字来自外部（webview、磁盘上的记录）时，能不能拼进本程序拥有的
// 目录」这条规则：`../../other` 拼上去就逃出去了。
func TestIsSinglePathElementRefusesTraversal(t *testing.T) {
	ok := []string{"a", "config.yaml", "2026-11-01-220739-84ce3a86", "带空格 的名字", "中文名"}
	for _, name := range ok {
		if !IsSinglePathElement(name) {
			t.Errorf("IsSinglePathElement(%q) = false, want true", name)
		}
	}
	bad := []string{
		"", ".", "..", // 目录自身与父目录
		"a/b", "a\\b", // 分隔符（两个都算：Windows 的也能出现）
		"./a", "../a", "/abs", // 相对/绝对路径
		"a/", "/", // 尾随分隔符与根
	}
	for _, name := range bad {
		if IsSinglePathElement(name) {
			t.Errorf("IsSinglePathElement(%q) = true, want false", name)
		}
	}
}

// CanonicalPath 的承诺是「同一个文件永远得到同一个字符串，哪怕它还不在」。两条分支都要钉：
// 存在（含符号链接，要落到真实路径）与不存在（退到最近的存在的祖先之上拼回去）。
func TestCanonicalPathResolvesWhatExistsAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real")
	if err := os.MkdirAll(realPath, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(realPath, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// 期望值也要把「真实路径」算一遍，而不是直接拿 t.TempDir() 拼出来的字符串：
	// 临时目录自己就可能在一个符号链接下面（macOS 上 /var → /private/var），而
	// CanonicalPath 解析的是所有存在的链接。Linux 上这两者恰好相同，所以只比原始路径
	// 的话，这条用例在 Linux 上绿、在 macOS 上红。
	real, err := filepath.EvalSymlinks(realPath)
	if err != nil {
		t.Fatalf("EvalSymlinks(real): %v", err)
	}

	// 存在的东西：符号链接落到真实路径，且一个点、两个点都被清理掉。
	got, err := CanonicalPath(filepath.Join(link, ".", "file.txt"))
	if err != nil {
		t.Fatalf("CanonicalPath(existing): %v", err)
	}
	want := filepath.Join(real, "file.txt")
	if got != want {
		t.Errorf("CanonicalPath = %q, want %q（符号链接要落到真实路径）", got, want)
	}

	// 不存在的东西：仍然给出一个稳定的答案 —— 这里那个文件确实还不存在。
	if Exists(got) {
		t.Fatalf("测试前提不成立：%q 不该存在", got)
	}
	again, err := CanonicalPath(filepath.Join(link, "file.txt"))
	if err != nil {
		t.Fatalf("CanonicalPath(missing): %v", err)
	}
	if again != got {
		t.Errorf("同一个不存在路径两次得到 %q / %q，应当相同", got, again)
	}

	// 相对路径先变绝对：调用方拿它做 key，两种写法必须落在一起。
	abs, err := CanonicalPath(filepath.Join(dir, "..", filepath.Base(dir), "real"))
	if err != nil {
		t.Fatalf("CanonicalPath(relative-ish): %v", err)
	}
	if abs != real {
		t.Errorf("CanonicalPath = %q, want %q", abs, real)
	}
}
