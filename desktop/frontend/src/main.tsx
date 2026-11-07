import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import Launcher from './launcher'

// One bundle, two windows. The main window loads "/" and gets the conversation; the launcher panel
// (desktop/launcher.go) loads "/?window=launcher" and gets the launcher. The query string is what
// tells them apart — see launcher.go's launcherURL.
const isLauncher = new URLSearchParams(window.location.search).get('window') === 'launcher'
if (isLauncher) {
  // The panel floats over the desktop, so the page underneath it must be see-through; the panel's
  // own sheet draws the only opaque surface (public/launcher.css).
  document.documentElement.classList.add('is-launcher')
}

ReactDOM.createRoot(document.getElementById('root') as HTMLElement).render(
  <React.StrictMode>{isLauncher ? <Launcher /> : <App />}</React.StrictMode>,
)
