//go:build headless

package main

// The badge link and the desktop microphone are both DESKTOP capabilities: a Bluetooth device
// paired with the machine, and a microphone in the room. A headless host has neither, so badge.go
// and voice.go are not compiled here. The runtime still calls these four methods on every state
// change and every parked ask, so they exist — as silence.
//
// Keeping the call sites identical is deliberate: the phone is meant to be the screen that sees
// what the badge would have seen, and when that lands it should land in the same places the badge
// already uses, not in a parallel set of hooks.

import "github.com/monsterxx03/tachi/config"

// notifyLink would repaint the device's screen. There is no device.
func (d *desktopApp) notifyLink() {}

// enqueueAlert would queue a beep for the device. There is no device to beep.
func (d *desktopApp) enqueueAlert(kind string) {}

// startBadge and stopBadge would own the BLE listener's lifecycle. Nothing to start.
func (d *desktopApp) startBadge(cfg *config.Config) {}
func (d *desktopApp) stopBadge()                    {}

// applyBadgeLink is the settings-save hook for the device link. There is no link to re-point, so
// a saved setting is simply accepted.
func applyBadgeLink(d *desktopApp, cfg *config.Config) error { return nil }
