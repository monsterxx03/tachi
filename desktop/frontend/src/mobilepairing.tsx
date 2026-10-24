import { useCallback, useEffect, useState } from 'react'
import QRCode from 'qrcode'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { MobilePairingVO, MobilePairingCandidate } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { copyText } from './lib'

/** 一个候选地址怎么写给人看：种类 + 网卡 + IP。种类是后端算好的词，这里不再自己判断。 */
function candLabel(c: MobilePairingCandidate): string {
  return `${c.label} · ${c.interface} · ${c.address}`
}

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
        明文 HTTP：拿到令牌的人就能让这台电脑执行命令。配对码里带着这台电脑的全部地址——家里局域网的，和
        Tailscale 的——所以手机在同一个 Wi-Fi 下能连，在 Tailscale 网络里也能连。两者都不是加密的，只在自己
        的网络里用。
      </div>

      {err ? <div className="settings-error">读不到配对信息：{err}</div> : null}
      {pair?.error ? <div className="settings-error">{pair.error}</div> : null}

      {pair && !pair.enabled ? (
        <div className="set-note">移动接口现在是关闭的：打开上面的 enabled 并重启一次，这张卡才会变成可连的状态。</div>
      ) : null}

      {pair ? (
        <div className="mobile-pair">
          {pair.candidates && pair.candidates.length > 1 ? (
            <div className="mobile-pair-row">
              <span className="mobile-pair-label">优先</span>
              <select
                className="set-input set-select"
                value={pair.interface || ''}
                disabled={busy}
                aria-label="配对优先使用的网络地址"
                onChange={async (e) => {
                  setBusy(true)
                  setNotice('')
                  try {
                    const ret = await AgentService.SetMobilePairingHost(e.target.value)
                    if (ret && ret !== 'ok') setNotice(ret)
                  } catch (err) {
                    setNotice(String(err))
                  } finally {
                    setBusy(false)
                    await load()
                  }
                }}
              >
                <option value="">自动（{candLabel(pair.candidates[0])}）</option>
                {pair.candidates.map((c) => (
                  <option key={c.interface} value={c.interface}>{candLabel(c)}</option>
                ))}
              </select>
              <span className="mobile-pair-actions" />
            </div>
          ) : null}

          <div className="mobile-pair-row">
            <span className="mobile-pair-label">地址</span>
            <span className="mobile-pair-value" title={pair.url || pair.listen}>
              {pair.url || (pair.listen ? `${pair.listen}（没有可用的局域网地址）` : '—')}
            </span>
            <span className="mobile-pair-actions">
              <button className="btn ghost" disabled={!pair.url} onClick={() => copyText(pair.url)}>复制</button>
            </span>
          </div>

          {/* 二维码里带着全部地址，所以「选中哪一个」只决定谁排在第一位、哪一行被复制走。
              把它们列出来是为了让读者知道手机不止一条路能进来 —— 不然 Tailscale 掉了会以为
              是这张卡没配上。 */}
          {(pair.urls ?? []).length > 1 ? (
            <div className="mobile-pair-row">
              <span className="mobile-pair-label">备用</span>
              <span className="mobile-pair-value">
                {(pair.urls ?? []).filter((u) => u !== pair.url).join('　')}
              </span>
              <span className="mobile-pair-actions" />
            </div>
          ) : null}

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
                码里带着这台电脑的每一条地址，手机连的时候会自己挑能用的那条（在家走近的，在外面走 Tailscale）。<br />
                扫不了就手输上面两行：地址填手机当下能访问到的任意一条，令牌是凭据、别发给别人。
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
