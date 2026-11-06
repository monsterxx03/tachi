package main

// 远端的文件：两边的字节怎么过去、怎么回来。
//
// 手机那套接口本来就是这么做的（协议里「附件」那一节）：上行是两步 —— 先把字节传到**那段
// 会话自己的目录**（uploads/，删会话一起删），拿回一个名字；发送时递的是那个**名字**，绝不
// 是路径。下行只有一种：`SendFile` 交付过的附件按那次工具调用的 id 取字节。桌面照抄这两条，
// 不发明第三种传输。
//
// 一条要守住的边界：**上传只认名字、下载只认 id**。令牌不等于「读那台机器上任意文件」——
// 所以这里没有「按路径取文件」的端点，也不该有。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	// remoteFileMaxBytes 是这一侧读写一个文件的硬上限。服务端还有自己的上限（ping 里的
	// maxUploadMb），超过那边的会带着它自己的理由拒绝；这里只是不让一个手滑选中的大文件
	// 先把本进程的内存吃光。
	remoteFileMaxBytes = 256 << 20
	// remoteDownloadsDir is where a downloaded attachment lands: the usual place a reader looks,
	// and never inside the workspace (a file from another machine is not part of this tree).
	remoteDownloadsDir = "Downloads"
)

// RemoteUploadVO is one upload's answer — the name the OTHER machine stored the file under, which
// is what the send request cites (never a path).
type RemoteUploadVO struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Kind string `json:"kind"`
}

// callBytes is call's binary twin: same address fallback and the same error decoding, but the
// 2xx body is returned as bytes instead of being decoded into a struct.
func (e *remoteEndpoint) callBytes(ctx context.Context, method, path string, payload []byte, contentType string) ([]byte, error) {
	var lastErr error
	for _, base := range e.urls {
		req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+e.token)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := e.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, remoteFileMaxBytes))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			var env struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(body, &env)
			if env.Error == "" {
				env.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
			return nil, &remoteAPIError{Status: resp.StatusCode, Code: env.Error}
		}
		if readErr != nil {
			return nil, readErr
		}
		return body, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用地址")
	}
	return nil, lastErr
}

// RemoteUploadFile sends one local file to the other machine's session and returns the name that
// machine stored it under. `path` names a file on THIS machine — it is read here, and only its
// bytes travel.
func (s *AgentService) RemoteUploadFile(hostID, sessionID, path string) (RemoteUploadVO, string) {
	var empty RemoteUploadVO
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return empty, why
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return empty, "这个文件读不了"
	}
	st, err := os.Stat(abs)
	if err != nil || !st.Mode().IsRegular() {
		return empty, "这个文件读不了"
	}
	if st.Size() > remoteFileMaxBytes {
		return empty, fmt.Sprintf("文件太大（%d MB，上限 %d MB）", st.Size()>>20, remoteFileMaxBytes>>20)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return empty, "这个文件读不了"
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filepath.Base(abs))
	if err != nil {
		return empty, "这个文件读不了"
	}
	if _, err := part.Write(data); err != nil {
		return empty, "这个文件读不了"
	}
	if err := mw.Close(); err != nil {
		return empty, "这个文件读不了"
	}
	body, err := e.callBytes(context.Background(), http.MethodPost, sessionPath(sessionID, "/files"), buf.Bytes(), mw.FormDataContentType())
	if err != nil {
		return empty, remoteReason(err)
	}
	// The protocol wraps it: {"file":{"name":…,"size":…,"kind":…}}.
	var env struct {
		File RemoteUploadVO `json:"file"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.File.Name == "" {
		return empty, "上传没成功"
	}
	return env.File, ""
}

// RemoteDownloadFile fetches one attachment the other machine's agent delivered (a SendFile call)
// and writes it under this machine's Downloads. The id is that tool call's id — the only handle
// the protocol offers, and the reason a token cannot be turned into "read any file over there".
func (s *AgentService) RemoteDownloadFile(hostID, sessionID, attachmentID, name string) (string, string) {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return "", why
	}
	if attachmentID == "" {
		return "", "这个文件没有可下载的标识"
	}
	path := sessionPath(sessionID, "/files/"+url.PathEscape(attachmentID))
	data, err := e.callBytes(context.Background(), http.MethodGet, path, nil, "")
	if err != nil {
		return "", remoteReason(err)
	}
	dest, err := saveRemoteDownload(name, data)
	if err != nil {
		return "", "没存下来：" + err.Error()
	}
	return dest, ""
}

// saveRemoteDownload writes the bytes somewhere a reader can find them, with a name that cannot
// escape that directory: `name` comes from the other machine, so it is reduced to a base name and
// de-duplicated like any other download.
func saveRemoteDownload(name string, data []byte) (string, error) {
	dir, err := os.UserHomeDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	if dl := filepath.Join(dir, remoteDownloadsDir); isDir(dl) {
		dir = dl
	}
	base := filepath.Base(strings.TrimSpace(name))
	if base == "" || base == "." || base == ".." || base == string(filepath.Separator) {
		base = "download"
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	dest := filepath.Join(dir, base)
	for i := 1; ; i++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			break
		}
		dest = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", err
	}
	return dest, nil
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
