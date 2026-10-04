//go:build darwin

package main

// framelessWindow: macOS keeps its native title bar, and NOT for symmetry's sake — the traffic
// lights ARE the window's own controls (close hides the window, see main.go's close hook), and
// removing the frame would take them away with nothing to put in their place.
//
// The app's own bar beside them is a CONTENT column's toolbar (see the frontend's layout.css),
// not window chrome, which is why the two can coexist there.
const framelessWindow = false
