//go:build headless

package main

// refreshTray is a no-op: this process has no menu bar and no tray host. The runtime calls it on
// every state change (the count of running turns is a menu-bar fact), so the method has to exist —
// it just has nothing to repaint. See tray_readout_ui.go for the windowed half.
func (d *desktopApp) refreshTray() {}

// applyAwakeUI is a no-op here for the same reason refreshTray is: the 「保持唤醒」 switch exists
// in this process (awake.go), and the phone is its only surface — there is no menu to repaint.
func (d *desktopApp) applyAwakeUI(awakeState) {}
