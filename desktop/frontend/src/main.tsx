import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import { Hud } from './hud'

// 同一个页面有两条路由，因为两个窗口用的是同一份资源（同一个 index.html、同一套样式变量与
// 主题解析）：主窗口是应用本身，小窗（?hud=1）是那块只显示一条回复的置顶面板。分叉放在这里
// 而不是 Go 侧用两份 HTML，是为了让主题、字体、CSS 变量这些「两个窗口必须一致」的东西天然只有
// 一份。小窗的窗口参数见 desktop/hud.go。
const isHud = new URLSearchParams(window.location.search).has('hud')

ReactDOM.createRoot(document.getElementById('root') as HTMLElement).render(
  <React.StrictMode>{isHud ? <Hud /> : <App />}</React.StrictMode>,
)
