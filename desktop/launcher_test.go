package main

import (
	"testing"
	"time"
)

// The launcher's ordering is the product: a launcher that puts Chrome above 微信 after 微 is typed
// is not a launcher. These cases pin the tiers, the pinyin keys and the two rules about usage.
func TestPinyinForms(t *testing.T) {
	cases := []struct {
		name     string
		initials string
		primary  string // the all-first-readings variant, which is variants[0]
		also     []string
	}{
		{name: "微信", initials: "wx", primary: "weixin"},
		{name: "哔哩哔哩", initials: "blbl", primary: "bilibili"},
		{name: "IINA", initials: "iina", primary: "iina"},
		{name: "Google Chrome", initials: "googlechrome", primary: "googlechrome"},
		// Mixed: the hanzi go through the dictionary, the Latin letters stay themselves.
		{name: "微信 WeChat", initials: "wxwechat", primary: "weixinwechat"},
		// 乐 is read liǎo/lè/yuè; the dictionary's first answer is not the one 音乐 uses, so the
		// alternate spellings have to be there or 「yinyue」 finds nothing.
		{name: "网易云音乐", initials: "wyyyl", primary: "wangyiyunyinle", also: []string{"wangyiyunyinyue"}},
	}
	for _, c := range cases {
		forms := pinyinFormsOf(c.name)
		if forms.initials != c.initials {
			t.Errorf("pinyinFormsOf(%q).initials = %q, want %q", c.name, forms.initials, c.initials)
		}
		if len(forms.variants) == 0 || forms.variants[0] != c.primary {
			t.Errorf("pinyinFormsOf(%q).variants = %v, want the first to be %q", c.name, forms.variants, c.primary)
		}
		for _, want := range c.also {
			found := false
			for _, variant := range forms.variants {
				if variant == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("pinyinFormsOf(%q).variants = %v, want it to include %q", c.name, forms.variants, want)
			}
		}
	}
}

func TestWordInitials(t *testing.T) {
	cases := map[string]string{
		"Google Chrome":          "gc",
		"Visual Studio Code":     "vsc",
		"IntelliJ IDEA Ultimate": "iiu",
		"iTerm2":                 "i",
		"1Password":              "1",
		"System Settings":        "ss",
		"Obsidian":               "o",
	}
	for name, want := range cases {
		if got := wordInitials(name); got != want {
			t.Errorf("wordInitials(%q) = %q, want %q", name, got, want)
		}
	}
}

// appFixture builds a prepared entry without touching the disk — the matching half is pure, and a
// test that had to read a real bundle could not say what it was testing.
func appFixture(name string, aliases ...string) *launcherApp {
	app := &launcherApp{Path: "/Applications/" + name + ".app", Name: name, Aliases: aliases}
	app.prepare()
	return app
}

func TestMatchTier(t *testing.T) {
	wechat := appFixture("微信", "WeChat")
	chrome := appFixture("Google Chrome")
	code := appFixture("Visual Studio Code")
	settings := appFixture("系统设置")

	cases := []struct {
		app   *launcherApp
		query string
		tier  int
	}{
		// The name itself, typed in Chinese — the reason the aliases and the dictionary exist.
		{wechat, "微信", tierExact},
		// An alias is the app's name too: the English one must not rank below the Chinese one.
		{wechat, "wechat", tierExact},
		{wechat, "wec", tierPrefix},
		// Pinyin: initials and the whole syllable string, from either a Latin or a hanzi name.
		{wechat, "wx", tierWord},
		{wechat, "weixin", tierWord},
		// A word inside a multi-word bundle name — the word a reader means for 「Google Chrome」.
		{chrome, "chr", tierWord},
		{chrome, "gc", tierWord},
		{code, "vsc", tierWord},
		{settings, "xtsz", tierWord},
		{settings, "系统", tierPrefix},
		// Inside a name, not at its start.
		{chrome, "rome", tierContains},
		// Letters in order but apart — the tier that lets a typo still land.
		{chrome, "gchrm", tierSubsequence},
		{chrome, "zzz", 0},
	}
	for _, c := range cases {
		tier, ok := c.app.matchTier(normalizeQuery(c.query))
		if !ok {
			tier = 0
		}
		if tier != c.tier {
			t.Errorf("%q against %q: tier %d, want %d", c.query, c.app.Name, tier, c.tier)
		}
	}
}

// One letter must not match everything: the loose tier is what would do it.
func TestSubsequenceNeedsTwoLetters(t *testing.T) {
	chrome := appFixture("Google Chrome")
	if _, ok := chrome.matchTier("z"); ok {
		t.Fatal("a single letter matched by subsequence")
	}
}

// A better tier beats a heavily used app: usage breaks ties inside a tier, never across one.
func TestListRanksTierBeforeUsage(t *testing.T) {
	l := newLauncher()
	l.apps = []*launcherApp{
		appFixture("Google Chrome"),
		appFixture("Chrome Remote Desktop"),
	}
	l.usage["/Applications/Chrome Remote Desktop.app"] = launcherUse{Count: 99, Last: time.Now()}

	items := l.list("chrome")
	if len(items) != 2 {
		t.Fatalf("got %d rows, want 2", len(items))
	}
	// 「Chrome Remote Desktop」 starts with the query, 「Google Chrome」 only contains it — even
	// though the second one is what this machine actually launches.
	if items[0].Name != "Chrome Remote Desktop" {
		t.Errorf("first row is %q, want Chrome Remote Desktop", items[0].Name)
	}

	// Inside one tier, usage decides.
	items = l.list("")
	if items[0].Name != "Chrome Remote Desktop" {
		t.Errorf("empty query: first row is %q, want the used app", items[0].Name)
	}
}

// The empty query is what the panel shows before anything is typed: usage first, then the
// alphabet — never the file-system order the scan happened to produce.
func TestListEmptyQueryFallsBackToTheAlphabet(t *testing.T) {
	l := newLauncher()
	l.apps = []*launcherApp{
		appFixture("Zed"),
		appFixture("Arc"),
		appFixture("IINA"),
	}
	items := l.list("   ")
	if len(items) != 3 {
		t.Fatalf("got %d rows, want 3", len(items))
	}
	for i, want := range []string{"Arc", "IINA", "Zed"} {
		if items[i].Name != want {
			t.Errorf("row %d is %q, want %q", i, items[i].Name, want)
		}
	}
}

// A query nothing answers is an empty list, not a list of everything.
func TestListDropsEverythingWhenNothingMatches(t *testing.T) {
	l := newLauncher()
	l.apps = []*launcherApp{appFixture("Zed")}
	if items := l.list("微信"); len(items) != 0 {
		t.Fatalf("got %d rows, want none", len(items))
	}
}
