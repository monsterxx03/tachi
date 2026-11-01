package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/monsterxx03/tachi/config"
)

// 这台机器在手机那侧的稳定身份。
//
// 手机可以记住好几台电脑，所以一条推送说的「哪一段对话」必须配上「哪一台」。会话 id 只在
// 一台机器上有意义：拿同一个 id 去另一台机器上打开，运气好是「这段对话已经不在了」，
// 运气不好会打开一段同 id 的、完全不相干的对话。
//
// 为什么不是 hostname：用户会在手机上给设备改名（那只存在手机里），而 hostname 本身也可能
// 因为换网络、改机器名而变；两台机器同名也不是不可能。所以这里是一个随机值，第一次用到时
// 生成，之后一直不变。
//
// 它住在自己的文件里，不塞进 mobile.json：手机接口没开的时候推送仍然要用它，而那个文件
// 是凭据（0600、可一键作废），把身份和令牌搅在一起会让"作废令牌"顺手换掉一台机器的名字。
const machineIDFileName = "machine.json"

type machineIDFile struct {
	ID string `json:"id"`
}

// machineIDOnce makes the read-or-create happen once per process; the value cannot change
// while it runs.
var (
	machineIDOnce  sync.Once
	machineIDValue string
)

func machineIDPath() string { return filepath.Join(config.BaseDir(), machineIDFileName) }

// machineID returns this machine's stable id, generating it on first use.
//
// A failure returns "": a notification without a device is still worth sending (the app opens,
// it just cannot switch machines), and a push must never be the thing that fails because a
// file could not be written.
func machineID() string {
	machineIDOnce.Do(func() {
		if b, err := os.ReadFile(machineIDPath()); err == nil {
			var f machineIDFile
			if json.Unmarshal(b, &f) == nil && f.ID != "" {
				machineIDValue = f.ID
				return
			}
		}
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			log.Printf("machine id: %v", err)
			return
		}
		f := machineIDFile{ID: base64.RawURLEncoding.EncodeToString(raw)}
		if err := writeMachineIDFile(f); err != nil {
			log.Printf("machine id: %v", err)
			return
		}
		machineIDValue = f.ID
	})
	return machineIDValue
}

func writeMachineIDFile(f machineIDFile) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := config.BaseDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".machine-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeded
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, machineIDPath())
}
