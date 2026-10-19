import { useCallback, useEffect, useState } from 'react'
import QRCode from 'qrcode'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { MobilePairingVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { copyText } from './lib'

// 手机配对卡 —— 设置页上第二块不是 config leaf 的内容（第一块是 MCP 编辑器）。
//
// 它为什么不走 SaveSettings：地址和令牌不是偏好。令牌住在 ~/.tachi/mobile.json（0600），
// 由后端生成与作废；这张卡只是把它显示出来，并提供一个「重新生成」。
//
// 二维码的内容就是后端给的 payload（{"v":1,"url":…,"token":…}）—— 手机端的解析器读的是
// 同一个形状（mobile/src/api/qr.ts），这里绝不再拼第二份。
export function MobilePairingCard() {
  const [pair, setPair] = useState<MobilePairingVO | null>(null)
  const [qr, setQr] = useState('')
  const [err, setErr] = useState('')
  const [reveal, setReveal] = useState(false)
  const [confirming, setConfirming] = useState(false)
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    try {
      const p = await AgentService.GetMobilePairing()
      setPair(p)
      setErr('')
      if (p?.payload) {
        // 离线渲染：这张卡不该为了画一个码去请求网络。
        setQr(await QRCode.toDataURL(p.payload, { margin: 1, width: 336, errorCorrectionLevel: 'M' }))
      } else {
        setQr('')
      }
    } catch (e) {
      setErr(String(e))
    }
  }, [])

  useEffect(() => { void load() }, [load])

  const regenerate = useCallback(async () => {
    setBusy(true)
    setNotice('')
    try {
      const ret = await AgentService.RegenerateMobileToken()
      if (ret && ret !== 'ok') {
        setNotice(ret)
      } else {
        setNotice('令牌已换新；已配对的手机都被断开了，需要重新扫码。')
      }
    } catch (e) {
      setNotice(String(e))
    } finally {
      setBusy(false)
      setConfirming(false)
      await load()
    }
  }, [load])

  return (
    <div className="set-section mobile-pair-block">
      <div className="set-section-head">
        手机配对
        <span className="mcp-servers-file">~/.tachi/mobile.json</span>
      </div>
      {/* 这条链路取舍必须写在用它的地方：它不是 HTTPS，而拿到令牌的人可以让这台电脑执行命令。 */}
      <div className="mobile-pair-warn">
        局域网明文 HTTP：拿到令牌的人就能让这台电脑执行命令。只在自己家的网络里用，并让手机和电脑待在同一个 Wi-Fi 下。
      </div>

      {err ? <div className="settings-error">读不到配对信息：{err}</div> : null}
      {pair?.error ? <div className="settings-error">{pair.error}</div> : null}

      {pair && !pair.enabled ? (
        <div className="set-note">移动接口现在是关闭的：打开上面的 enabled 并重启一次，这张卡才会变成可连的状态。</div>
      ) : null}

      {pair ? (
        <div className="mobile-pair">
          <div className="mobile-pair-row">
            <span className="mobile-pair-label">地址</span>
            <span className="mobile-pair-value" title={pair.url || pair.listen}>
              {pair.url || (pair.listen ? `${pair.listen}（没有可用的局域网地址）` : '—')}
            </span>
            <span className="mobile-pair-actions">
              <button className="btn ghost" disabled={!pair.url} onClick={() => copyText(pair.url)}>复制</button>
            </span>
          </div>

          <div className="mobile-pair-row">
            <span className="mobile-pair-label">令牌</span>
            <span className="mobile-pair-value" title={reveal ? pair.token : ''}>
              {reveal ? pair.token : '•'.repeat(Math.min(pair.token?.length || 0, 24))}
            </span>
            <span className="mobile-pair-actions">
              <button className="btn ghost" onClick={() => setReveal((v) => !v)}>{reveal ? '隐藏' : '显示'}</button>
              <button className="btn ghost" disabled={!pair.token} onClick={() => copyText(pair.token)}>复制</button>
            </span>
          </div>

          {qr ? (
            <div className="mobile-pair-qr">
              <img src={qr} alt="配对二维码" />
              <div className="mobile-pair-qr-hint">
                用手机里的 Tachi 扫这个码，地址和令牌会一起填好。<br />
                扫不了就手输上面两行：地址要手机能访问到（同一局域网），令牌是凭据、别发给别人。
              </div>
            </div>
          ) : null}

          <div className="mobile-pair-actions">
            {confirming ? (
              <>
                <span className="set-note">换新令牌会断开所有已配对的手机。</span>
                <button className="btn" disabled={busy} onClick={() => void regenerate()}>确认重新生成</button>
                <button className="btn ghost" disabled={busy} onClick={() => setConfirming(false)}>取消</button>
              </>
            ) : (
              <button className="btn ghost" disabled={busy} onClick={() => setConfirming(true)}>重新生成令牌</button>
            )}
          </div>
          {notice ? <div className="mobile-pair-ok">{notice}</div> : null}
        </div>
      ) : null}

      <div className="set-note">
        「重新生成」之后旧令牌立刻失效：活着的连接会被断开，已配对的手机需要重新扫码。
      </div>
    </div>
  )
}
