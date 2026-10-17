package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// provider 定义放在 config.yaml 旁边的一组 JSON 文件里（provider.<任意>.json），不在这份
// yaml 里。一个文件可以放好几个 provider，形状和 mcp.json 一样是一个 `providers` 数组：
//
//	{"providers": [{"name": "deepseek-flash", "type": "anthropic", ...}]}
//
// 为什么不继续放在 config.yaml：provider 是「跟哪台机器、哪个账号说话」这件事，和 tachi
// 自己的偏好（语言、权限、日志）混在同一份文件里，谁要接一个新线路就得改整份配置；而
// config.yaml 是设置页整份重写的那份文件，两个用途挤在一处的代价是每次保存都在重写别人的
// 密钥。拆开之后，一份 provider 文件就是一个线路的身份证，可以单独拿出来、单独换、单独
// 备份。
//
// 装载是「按文件名排序、逐个读、按 name 合并」；同名跨文件是错误而不是覆盖——两个文件都
// 在说「我叫 main」，哪一个赢都不是用户能预料的事（mcp.json 同一套规则）。
const (
	providerFilePrefix = "provider."
	providerFileSuffix = ".json"
)

// providerFile 是一个 provider.*.json 的内容: `{"providers": [...]}`。
//
// 和 mcp.json 的 "servers" 同一种写法——文件是一份带字段的文档，不是一个裸数组，所以以后要加
// 「这份文件属于哪个环境」这类顶层键，不用换文件的形状。
type providerFile struct {
	Providers []ProviderConfig `json:"providers"`
}

// ProviderConfigDir 是 provider.*.json 所在的目录：config.yaml 旁边。
func ProviderConfigDir() string { return BaseDir() }

// ProviderConfigPaths 列出 dir 里全部 provider.*.json，按文件名排序（也就是合并顺序）。
func ProviderConfigPaths(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isProviderFileName(e.Name()) {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// isProviderFileName reports whether a file name is one of the provider files: `provider.` +
// 非空的名字 + `.json`，所以 `provider.json`（没有名字）不算。
func isProviderFileName(name string) bool {
	if !strings.HasPrefix(name, providerFilePrefix) || !strings.HasSuffix(name, providerFileSuffix) {
		return false
	}
	return len(name) > len(providerFilePrefix)+len(providerFileSuffix)
}

// LoadProviders 读取并合并 dir 里的 provider 文件。没有文件是正常状态（返回 nil）。
//
// 每个条目的 SourceFile 被填成它来自的文件——设置页靠它决定「保存时写回哪一份」，所以它
// 是装载的一部分，不是调用方自己拼的。
func LoadProviders(dir string) ([]ProviderConfig, error) {
	paths, err := ProviderConfigPaths(dir)
	if err != nil {
		return nil, err
	}
	var out []ProviderConfig
	seen := make(map[string]string, len(paths)) // name → 定义它的文件
	for _, path := range paths {
		entries, err := readProviderFile(path)
		if err != nil {
			return nil, err
		}
		for _, p := range entries {
			if p.Name == "" {
				return nil, fmt.Errorf("%s: 有一个 provider 没有 name", path)
			}
			if prev, ok := seen[p.Name]; ok {
				return nil, fmt.Errorf("%s: provider %q 在 %s 里也定义过（同名只能出现一次）", path, p.Name, prev)
			}
			seen[p.Name] = path
			p.SourceFile = path
			out = append(out, p)
		}
	}
	return out, nil
}

// readProviderFile 读一个 provider 文件。文件里的 SourceFile 由装载器盖，不接受文件自己写。
func readProviderFile(path string) ([]ProviderConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f providerFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i := range f.Providers {
		f.Providers[i].SourceFile = ""
	}
	return f.Providers, nil
}

// ProviderFilePlan 是一次保存要写下去的东西：哪些文件写什么，哪些文件该消失（里面的 provider
// 全被删了）。
//
// 它先算后写，因为设置页要先把「保存会写什么」拿给人看，再写；同一个函数算出来的内容，预览
// 和保存看到的是同一份（两处各写一遍，就是两处会不一致的地方）。
type ProviderFilePlan struct {
	// Files 是路径 → 文件内容，全部在计划时就渲染好。
	Files map[string][]byte
	// Deletes 是内容已经空掉、该删掉的文件。
	Deletes []string
}

// Empty reports whether this plan touches anything at all.
func (p ProviderFilePlan) Empty() bool { return len(p.Files) == 0 && len(p.Deletes) == 0 }

// Paths 列出所有会被动到的文件，写和删都在里面，按路径排序（错误信息与预览的顺序稳定）。
func (p ProviderFilePlan) Paths() []string {
	out := make([]string, 0, len(p.Files)+len(p.Deletes))
	for path := range p.Files {
		out = append(out, path)
	}
	out = append(out, p.Deletes...)
	sort.Strings(out)
	return out
}

// PlanProviderFiles 把这份 provider 列表按各自的来源分组，算出每个文件该写什么。
//
// 分组的键是 SourceFile：一个条目写回它被读出来的那份文件——这是「编辑保存回原文件」的全部
// 实现。没有 SourceFile 的条目是设置页新加的，按名字落到 provider.<name>.json（那个文件已
// 存在就并进去，用户自己的命名习惯优先）。
//
// 一个文件里的条目全被删掉时，文件也被删掉：留一个 `{"providers": []}` 在那儿，下次看到它
// 的人不知道是该往里加东西还是删掉它。
func PlanProviderFiles(dir string, providers []ProviderConfig) (ProviderFilePlan, error) {
	existing, err := ProviderConfigPaths(dir)
	if err != nil {
		return ProviderFilePlan{}, err
	}
	groups := make(map[string][]ProviderConfig, len(existing))
	names := make(map[string]string, len(providers))
	for _, p := range providers {
		if p.Name == "" {
			return ProviderFilePlan{}, fmt.Errorf("有个 provider 没有 name，存不下去")
		}
		path := p.SourceFile
		if path == "" {
			path = filepath.Join(dir, providerFileName(p.Name))
		}
		if prev, ok := names[p.Name]; ok {
			return ProviderFilePlan{}, fmt.Errorf("provider %q 出现了两次（%s 与 %s）", p.Name, prev, path)
		}
		names[p.Name] = path
		p.SourceFile = ""
		groups[path] = append(groups[path], p)
	}

	plan := ProviderFilePlan{Files: make(map[string][]byte, len(groups))}
	for path, list := range groups {
		data, err := fileutil.MarshalJSON(providerFile{Providers: list})
		if err != nil {
			return ProviderFilePlan{}, fmt.Errorf("%s: %w", path, err)
		}
		plan.Files[path] = append(data, '\n')
	}
	for _, path := range existing {
		if _, kept := groups[path]; !kept {
			plan.Deletes = append(plan.Deletes, path)
		}
	}
	sort.Strings(plan.Deletes)
	return plan, nil
}

// providerFileName 是新条目落到哪个文件：provider.<name>.json。名字里不能进文件名的字符换成
// 短横线——一个叫 `goat/deepseek` 的 provider 仍然得有一个文件能装它。
func providerFileName(name string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, name)
	if safe == "" {
		safe = "provider"
	}
	return providerFilePrefix + safe + providerFileSuffix
}

// Apply 执行这份计划，并返回一个把磁盘恢复原样的函数。
//
// 多份文件不是一次原子写，所以它先记下每个会被动到的文件的原始字节：写到一半失败时把自己
// 已经写的全部撤回，再报错。调用方（设置页保存）拿到的 undo 是给「后面那一步失败」用的——
// provider 文件写成功、config.yaml 写失败时，两边都得回到起点。
func (p ProviderFilePlan) Apply() (undo func(), err error) {
	if p.Empty() {
		return func() {}, nil
	}
	before, err := p.snapshot()
	if err != nil {
		return nil, err
	}
	restore := func() { restoreProviderFiles(before) }
	for _, path := range p.deletesOrder() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			restore()
			return nil, fmt.Errorf("删除 %s: %w", path, err)
		}
	}
	for _, path := range p.filesOrder() {
		if err := writeProviderFile(path, p.Files[path]); err != nil {
			restore()
			return nil, err
		}
	}
	return restore, nil
}

// filesOrder / deletesOrder 让写入顺序稳定（map 的遍历顺序是随机的，而一次失败要能复现）。
func (p ProviderFilePlan) filesOrder() []string {
	out := make([]string, 0, len(p.Files))
	for path := range p.Files {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func (p ProviderFilePlan) deletesOrder() []string { return p.Deletes }

// providerFileSnapshot 是一个文件在被改动之前的样子。exists=false 意味着它原本不在（undo 要
// 把它删掉，而不是写回空内容）。
type providerFileSnapshot struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func (p ProviderFilePlan) snapshot() ([]providerFileSnapshot, error) {
	out := make([]providerFileSnapshot, 0, len(p.Files)+len(p.Deletes))
	for _, path := range p.Paths() {
		snap := providerFileSnapshot{path: path}
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			snap.data, snap.exists = data, true
			if st, err := os.Stat(path); err == nil {
				snap.mode = st.Mode().Perm()
			}
		case !os.IsNotExist(err):
			return nil, fmt.Errorf("读取 %s: %w", path, err)
		}
		out = append(out, snap)
	}
	return out, nil
}

// restoreProviderFiles 把快照写回磁盘，尽力而为：保存失败那条路上，能撤多少撤多少。
func restoreProviderFiles(snaps []providerFileSnapshot) {
	for _, s := range snaps {
		if !s.exists {
			_ = os.Remove(s.path)
			continue
		}
		mode := s.mode
		if mode == 0 {
			mode = 0o600
		}
		_ = fileutil.AtomicWriteFile(s.path, s.data, providerFileDirPerm, mode)
	}
}

// writeProviderFile 写一份 provider 文件，保留它原本的权限——里面是 api_key，一个用户收成
// 0600 的文件不该因为保存一次就变回 0644。
func writeProviderFile(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := fileutil.AtomicWriteFile(path, data, providerFileDirPerm, mode); err != nil {
		return fmt.Errorf("写入 %s: %w", path, err)
	}
	return nil
}

// SyncProviderFiles 是 PlanProviderFiles + Apply 的常用组合：把当前的 provider 列表整份写回
// 它该在的文件里。
func SyncProviderFiles(dir string, providers []ProviderConfig) (undo func(), err error) {
	plan, err := PlanProviderFiles(dir, providers)
	if err != nil {
		return nil, err
	}
	return plan.Apply()
}

// ProviderFiles 是 dir 里现有 provider 文件的相对路径（相对 dir），给预览和错误信息用。
func ProviderFiles(dir string) ([]string, error) {
	paths, err := ProviderConfigPaths(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(paths))
	for i, path := range paths {
		out[i] = filepath.Base(path)
	}
	return out, nil
}

// providerFileDirPerm 是新目录的权限：provider 文件里是密钥，所以目录只给本人。
const providerFileDirPerm os.FileMode = 0o700
