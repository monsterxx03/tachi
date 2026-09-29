// 小窗（HUD）：一轮跑完、人又不在窗口前时，屏幕上多出来的那一块面板。
//
// 它是**同一个页面**的另一条路由（main.tsx 按 ?hud=1 分叉），不是第二套应用：主题、字体、
// 样式变量与主窗口共用，而这里只画两样东西——会话名和这条回复的正文。
//
// 主机那边（desktop/hud.go）负责「什么时候弹、弹什么」；这里只负责画出来、以及把两个用户动作
// 说回去：关掉（× 或 Esc）和上下翻（设备的那两个键走的就是 hud:scroll）。
import { useEffect, useRef, useState } from 'react'
import { Events } from '@wailsio/runtime'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { MarkdownBlock } from './markdown'

// HudPayload 与 Go 侧的 hudPayload 一一对应（desktop/hud.go）。
type HudPayload = {
  sessionId: string
  title: string
  text: string
}

// 一次 hud:scroll 走多远（px）。
//
// 由主机决定「该翻一屏了」，而一屏有多高是这里的排版事实——所以定在这里，主机只送方向。
// 数是这样来的：正文行高约 25px（15.5px × 1.62），面板正文区高约 250px，一次翻 112px ≈ 4-5 行，
// 按五下看完一屏。设备那边是**按一下翻一段**（手在按键上，不在滚轮上），一行一行挪要按十几次。
const SCROLL_STEP = 112

export function Hud() {
  const [content, setContent] = useState<HudPayload | null>(null)
  const [workDir, setWorkDir] = useState('')
  const body = useRef<HTMLDivElement | null>(null)

  useEffect(() => {
    const offContent = Events.On('agent:hud', (event) => {
      setContent(event.data as HudPayload)
      // 新的一条从头上看起：上一轮读到哪儿和这一轮无关，而停在半截上会让人以为漏了开头。
      requestAnimationFrame(() => {
        if (body.current) body.current.scrollTop = 0
      })
    })
    const offScroll = Events.On('hud:scroll', (event) => {
      const delta = Number((event.data as { delta?: number } | undefined)?.delta ?? 0)
      body.current?.scrollBy({ top: Math.sign(delta) * SCROLL_STEP })
    })
    // 页面就绪：主机手里那条内容现在能送过来了。启动后第一轮如果跑在页面挂载之前，
    // 那次载荷是发进一个还没有监听者的页面的——这一句是它的兜底。
    void Events.Emit('hud:ready')
    return () => {
      offContent?.()
      offScroll?.()
    }
  }, [])

  // 正文里的图片按**这个会话的工作目录**解析（transcript 用的是同一个 binding：同一张图在两处
  // 的路径必须由同一处算出来，否则「窗口里能看到、小窗里是个破图」）。
  //
  // 一次载荷问一次（切换会话时才重问）：图片路径是相对路径，而小窗显示的是那一条回复所属的会话。
  useEffect(() => {
    const id = content?.sessionId
    if (!id) {
      setWorkDir('')
      return
    }
    let alive = true
    void Promise.resolve((AgentService as any).GetSessionWorkingDir?.(id))
      .then((dir: string | undefined) => {
        if (alive) setWorkDir(dir || '')
      })
      .catch(() => {
        if (alive) setWorkDir('')
      })
    return () => {
      alive = false
    }
  }, [content?.sessionId])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') closeHud()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  return (
    <div className="hud">
      <header className="hud-head">
        <span className="hud-title" title={content?.title || ''}>
          {content?.title || 'Tachi'}
        </span>
        <button className="hud-close" onClick={closeHud} title="关闭（Esc）" aria-label="关闭小窗">
          ✕
        </button>
      </header>
      <div className="hud-body" ref={body}>
        {content ? (
          // 和窗口里那份 transcript 共用同一个渲染管线（react-markdown + gfm + 代码高亮，
          // ```mermaid 走 markdown.tsx 的懒加载，见 docs/2026-09-29-desktop-hud-design.md §2）。
          //
          // 唯一的差别是 zoomDiagrams={false}：那张图的「点击全屏」是把覆盖层开在这块面板里，
          // 而面板只有 560×300——挤得没法看，所以这个面板上不要那个手势（图本身照常画）。
          <MarkdownBlock text={content.text} workDir={workDir} zoomDiagrams={false} />
        ) : (
          <div className="hud-empty">还没有要看的东西</div>
        )}
      </div>
    </div>
  )
}

// closeHud 让小窗消失。真正的隐藏由主机做——它是「小窗开着没有」的唯一所有者（设备那边据此
// 决定底栏写什么、上下键发不发），这里只说一声，不自己动窗口。
function closeHud() {
  void Events.Emit('hud:close')
}
