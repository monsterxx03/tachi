package main

// sessionTitle returns the session's display title ("" when unknown). It is the notifier's
// subtitle callback (see notifyTarget): naming the conversation is what makes a notification
// about one the reader is NOT looking at actionable rather than puzzling.
//
// It lives here rather than in notify.go because it is a fact about a SESSION, not about
// notifications: the window title bar, a failure list and the headless host all ask it, and the
// headless build has no notification centre to ask anything of.
func (d *desktopApp) sessionTitle(id string) string {
	d.mu.Lock()
	r := d.ownerOfLocked(id)
	d.mu.Unlock()
	if r == nil || r.sm == nil {
		return ""
	}
	if cur := r.sm.Current(); cur != nil {
		return cur.Title
	}
	return ""
}
