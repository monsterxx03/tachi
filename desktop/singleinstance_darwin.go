//go:build darwin

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// singleInstanceOptions returns nothing on macOS: the platform already sees to this.
// LaunchServices activates the running app instead of starting a second one, and `open -n` — a
// deliberate second instance, which the dev flow (`make -C desktop run`) and the smoke suite use
// — must keep working. Wails' own mechanism would break exactly that, on the platform that does
// not need it.
func singleInstanceOptions(_ func()) *application.SingleInstanceOptions { return nil }
