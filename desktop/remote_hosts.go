package main

// 远端机器表：桌面作为另一个宿主（一台无头 tachi）的客户端时，它要记住的那些机器。
//
// 配对不扫码：无头宿主启动时本来就打印一段配对信息（地址、每条可达地址、令牌），
// 这里做的就是把它粘进来、试一遍、记下来。地址和手机那一侧的规则是同一份
// （docs/agents/mobile-protocol.md 不变量 12）：一台机器可能有好几条地址，
// 连的时候一起试，谁先应答用谁。
//
// 令牌是凭据，和手机那个一样单独放、0600：拿到它就能在那台机器上执行命令。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

const (
	remoteHostsFileName = "remote_hosts.json"
	remoteHostsVersion  = 1
	// remoteProbeTimeout 是一次配对的全部预算：几条地址一起试，一条都不应答就让读者
	// 知道「地址或网络不对」，而不是让他等满一个 HTTP 超时。
	remoteProbeTimeout = 6 * time.Second
	// remoteRequestTimeout 是普通命令（发一条消息、答一个询问）的预算。它比探测宽，
	// 因为对面可能正在跑一轮、要等锁。
	remoteRequestTimeout = 20 * time.Second
)

// remoteHost 是一台被记住的机器。ID 优先用对端自报的 deviceId —— 那是它稳定的身份
// （手机也用它），地址会变、名字会改，这个值不变。
type remoteHost struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	URLs     []string `json:"urls"`
	Token    string   `json:"token"`
	DeviceID string   `json:"deviceId,omitempty"`
	Version  string   `json:"version,omitempty"`
}

type remoteHostsFile struct {
	Version int          `json:"version"`
	Hosts   []remoteHost `json:"hosts"`
}

// RemoteHostVO 是机器表给界面看的那一份。**没有 token**：界面不需要它，而一个进过
// webview 的凭据就多了一处要擦的地方。
type RemoteHostVO struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	URLs      []string `json:"urls"`
	DeviceID  string   `json:"deviceId"`
	Reachable bool     `json:"reachable"`
	Error     string   `json:"error"`
}

func remoteHostsPath() string {
	return filepath.Join(config.BaseDir(), remoteHostsFileName)
}

func (h remoteHost) vo() RemoteHostVO {
	urls := h.URLs
	if urls == nil {
		urls = []string{}
	}
	return RemoteHostVO{
		ID:       h.ID,
		Name:     h.Name,
		Version:  h.Version,
		URLs:     urls,
		DeviceID: h.DeviceID,
	}
}

func (d *desktopApp) loadRemoteHosts() ([]remoteHost, error) {
	b, err := os.ReadFile(remoteHostsPath())
	if errors.Is(err, os.ErrNotExist) {
		return []remoteHost{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f remoteHostsFile
	if err := json.Unmarshal(b, &f); err != nil {
		// 读不懂的文件绝不覆盖：里面有令牌，重建一次等于让所有配对失效。
		return nil, fmt.Errorf("远端机器表 %s 读不出来：%w", remoteHostsPath(), err)
	}
	if f.Hosts == nil {
		f.Hosts = []remoteHost{}
	}
	return f.Hosts, nil
}

func (d *desktopApp) saveRemoteHosts(hosts []remoteHost) error {
	if hosts == nil {
		hosts = []remoteHost{}
	}
	b, err := json.MarshalIndent(remoteHostsFile{Version: remoteHostsVersion, Hosts: hosts}, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.AtomicWriteFilePrivate(remoteHostsPath(), b)
}

func (d *desktopApp) remoteHostByID(id string) (remoteHost, error) {
	hosts, err := d.loadRemoteHosts()
	if err != nil {
		return remoteHost{}, err
	}
	for _, h := range hosts {
		if h.ID == id {
			return h, nil
		}
	}
	return remoteHost{}, fmt.Errorf("没有这台远端机器：%s", id)
}

// ListRemoteHosts 只回答「记住了哪些机器」，一次网络都不碰：它是页面打开时画列表用的，
// 一台离线的机器不该让列表等一个超时。可达性是另一次读取的事（RemoteSessions/RemoteOpen）。
func (s *AgentService) ListRemoteHosts() []RemoteHostVO {
	d := s.desk
	d.remoteHostsMu.Lock()
	defer d.remoteHostsMu.Unlock()
	hosts, err := d.loadRemoteHosts()
	if err != nil {
		return []RemoteHostVO{}
	}
	out := make([]RemoteHostVO, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.vo())
	}
	return out
}

type remotePairingPayload struct {
	V        int      `json:"v"`
	URL      string   `json:"url"`
	URLs     []string `json:"urls"`
	Token    string   `json:"token"`
	DeviceID string   `json:"deviceId"`
	// Hostname is not part of the printed payload; it is filled in by the probe.
	Hostname string `json:"hostname,omitempty"`
}

// remoteURLPattern picks addresses out of prose: the headless host prints them in a sentence
// ("…fill this address in the app: http://x:8787 (局域网)"), and under systemd that line is the
// only thing the reader has to copy. Parentheses and quotes end an address.
var remoteURLPattern = regexp.MustCompile(`https?://[^\s"'<>()（）]+`)

// remoteTokenPattern is what a bare credential looks like: `writeNewMobileToken` generates 32
// bytes, base64url, so 43 characters of [A-Za-z0-9-_]. A lower bound keeps it from matching an
// ordinary word in the surrounding text.
var remoteTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)

// textPairingToken finds the credential in copied text. It prefers an explicitly labelled one
// ("token: …" / "令牌 …"), and otherwise takes the first bare string that is shaped like the
// generated token. Labels matter: "tachi:" and "token:" are right there in the same paragraph,
// and picking the first field after the first colon is exactly how a paste like that fails.
func textPairingToken(s string) string {
	fields := strings.Fields(s)
	for i, f := range fields {
		label := strings.ToLower(strings.Trim(f, "：:"))
		if label == "token" || f == "令牌" || f == "令牌：" || f == "令牌:" {
			if i+1 < len(fields) {
				return strings.Trim(fields[i+1], "，,。.；;\"'()（）")
			}
		}
	}
	for _, f := range fields {
		c := strings.Trim(f, "，,。.；;\"'()（）")
		if remoteTokenPattern.MatchString(c) {
			return c
		}
	}
	return ""
}

// parseRemotePairing 接受两种形状：无头宿主打印的那段 JSON（原样粘），或者**照屏幕抄下来的
// 那几行文字**。后者是 NAS / VPS 上的常情：systemd 的 journal 里没有二维码，只有
// 「… address: http://nas:8787 (局域网)」和「token: …」两行，所以带前缀、带标签、带括号
// 的整段文字都必须能读出来 —— 让读者自己去抠 URL 和令牌是没必要的门槛。
func parseRemotePairing(raw string) (remotePairingPayload, error) {
	var p remotePairingPayload
	s := strings.TrimSpace(raw)
	if s == "" {
		return p, errors.New("把远端主机打印的配对信息粘进来")
	}
	if strings.HasPrefix(s, "{") {
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			return p, fmt.Errorf("这段配对信息读不出来：%v", err)
		}
	} else {
		p.URLs = remoteURLPattern.FindAllString(s, -1)
		p.Token = textPairingToken(s)
		if len(p.URLs) > 0 {
			p.URL = p.URLs[0]
		}
	}
	// 一台机器可能有好几条地址（局域网、Tailscale）。顺序就是配对信息里的顺序，
	// 去重之后原样保存：连接时按这个顺序试。
	seen := map[string]bool{}
	urls := make([]string, 0, len(p.URLs)+1)
	add := func(u string) {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		urls = append(urls, u)
	}
	add(p.URL)
	for _, u := range p.URLs {
		add(u)
	}
	if len(urls) == 0 {
		return p, errors.New("配对信息里没有地址")
	}
	if strings.TrimSpace(p.Token) == "" {
		return p, errors.New("配对信息里没有令牌")
	}
	p.URLs = urls
	p.URL = urls[0]
	return p, nil
}

// probeRemotePing 只读 ping（这个端点不要令牌，它回答的是「这个地址上是不是 tachi」）。
func probeRemotePing(ctx context.Context, base string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/ping", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: remoteProbeTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// remoteChecksToken 用一次带令牌的读来验证令牌。ping 故意不要令牌（配对页先要能碰到东西），
// 所以「地址对」和「令牌对」是两次确认。
func remoteChecksToken(ctx context.Context, base, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/sessions", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: remoteProbeTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("令牌不对（在远端机器上用 `tachi-headless` 打印的那段信息里找）")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("远端回了 HTTP %d", resp.StatusCode)
	}
	return nil
}

func dedupeURLs(groups ...[]string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		for _, u := range g {
			u = strings.TrimRight(strings.TrimSpace(u), "/")
			if u == "" || seen[u] {
				continue
			}
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

func remoteHostID(deviceID, firstURL string) string {
	if deviceID != "" {
		return deviceID
	}
	sum := sha256.Sum256([]byte(firstURL))
	return hex.EncodeToString(sum[:8])
}

func displayHostName(hostname, firstURL string) string {
	name := strings.TrimSuffix(strings.TrimSpace(hostname), ".local")
	if name != "" {
		return name
	}
	if u, err := url.Parse(firstURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return firstURL
}

// pairRemoteHost 是配对的那一半（不带锁）：试地址、验令牌、得出要保存的那条记录。
func pairRemoteHost(ctx context.Context, payload string) (remoteHost, error) {
	p, err := parseRemotePairing(payload)
	if err != nil {
		return remoteHost{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, remoteProbeTimeout)
	defer cancel()
	type answer struct {
		url  string
		ping map[string]any
		err  error
	}
	ch := make(chan answer, len(p.URLs))
	for _, u := range p.URLs {
		go func(u string) {
			ping, err := probeRemotePing(ctx, u)
			ch <- answer{url: u, ping: ping, err: err}
		}(u)
	}
	var first answer
	ok := false
	for range p.URLs {
		a := <-ch
		if a.err == nil {
			first = a
			ok = true
			break // 后面的地址不用再试了；defer 的 cancel 会把它们收掉
		}
	}
	if !ok {
		return remoteHost{}, fmt.Errorf("这些地址都没有应答：%s", strings.Join(p.URLs, "、"))
	}
	if err := remoteChecksToken(ctx, first.url, p.Token); err != nil {
		return remoteHost{}, err
	}
	hostname := stringField(first.ping, "hostname")
	deviceID := stringField(first.ping, "deviceId")
	if deviceID == "" {
		deviceID = p.DeviceID
	}
	// 对端自己报的地址排在前面（它知道自己现在在哪），然后是配对信息里带的。
	urls := dedupeURLs(p.URLs, []string{first.url}, stringSlice(first.ping["urls"]))
	return remoteHost{
		ID:       remoteHostID(deviceID, urls[0]),
		Name:     displayHostName(hostname, urls[0]),
		URLs:     urls,
		Token:    strings.TrimSpace(p.Token),
		DeviceID: deviceID,
		Version:  stringField(first.ping, "version"),
	}, nil
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func stringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, it := range raw {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// AddRemoteHost pairs one machine and remembers it. 同一台机器再配对一次就是更新
// （按 deviceId 认），不会在列表里多出一行。
func (s *AgentService) AddRemoteHost(payload string) (RemoteHostVO, string) {
	d := s.desk
	host, err := pairRemoteHost(context.Background(), payload)
	if err != nil {
		return RemoteHostVO{}, err.Error()
	}
	d.remoteHostsMu.Lock()
	defer d.remoteHostsMu.Unlock()
	hosts, err := d.loadRemoteHosts()
	if err != nil {
		return RemoteHostVO{}, err.Error()
	}
	replaced := false
	for i := range hosts {
		if hosts[i].ID == host.ID {
			// 再配对一次是「刷新地址和令牌」，不是把读者起过的名字改回机器自称的那个。
			if hosts[i].Name != "" {
				host.Name = hosts[i].Name
			}
			hosts[i] = host
			replaced = true
			break
		}
	}
	if !replaced {
		hosts = append(hosts, host)
	}
	if err := d.saveRemoteHosts(hosts); err != nil {
		return RemoteHostVO{}, "没记住这台机器：" + err.Error()
	}
	vo := host.vo()
	vo.Reachable = true
	return vo, ""
}

// RenameRemoteHost gives a machine the name the reader chose. The name lives HERE, not on the
// other machine — the phone's rule too: a probe reports the machine's hostname, and there is no
// endpoint that writes a label back, so 「我管这台叫 NAS」 is this side's business.
func (s *AgentService) RenameRemoteHost(id, name string) (RemoteHostVO, string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return RemoteHostVO{}, "名字不能是空的"
	}
	d := s.desk
	d.remoteHostsMu.Lock()
	defer d.remoteHostsMu.Unlock()
	hosts, err := d.loadRemoteHosts()
	if err != nil {
		return RemoteHostVO{}, err.Error()
	}
	found := false
	for i := range hosts {
		if hosts[i].ID == id {
			hosts[i].Name = name
			found = true
			break
		}
	}
	if !found {
		return RemoteHostVO{}, "没有这台远端机器"
	}
	if err := d.saveRemoteHosts(hosts); err != nil {
		return RemoteHostVO{}, "没存下来：" + err.Error()
	}
	for _, h := range hosts {
		if h.ID == id {
			return h.vo(), ""
		}
	}
	return RemoteHostVO{}, "没有这台远端机器"
}

// RemoveRemoteHost forgets one machine. 它同时断掉那条连接：一个已经不该被信任的
// 地址上留着的长连接，比没有连接更糟。
func (s *AgentService) RemoveRemoteHost(id string) string {
	d := s.desk
	d.remoteHostsMu.Lock()
	hosts, err := d.loadRemoteHosts()
	if err != nil {
		d.remoteHostsMu.Unlock()
		return err.Error()
	}
	kept := make([]remoteHost, 0, len(hosts))
	found := false
	for _, h := range hosts {
		if h.ID == id {
			found = true
			continue
		}
		kept = append(kept, h)
	}
	if !found {
		d.remoteHostsMu.Unlock()
		return "没有这台远端机器"
	}
	if err := d.saveRemoteHosts(kept); err != nil {
		d.remoteHostsMu.Unlock()
		return "没删掉：" + err.Error()
	}
	d.remoteHostsMu.Unlock()
	d.closeRemoteConn(id)
	return "ok"
}
