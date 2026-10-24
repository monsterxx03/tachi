package main

import (
	"os"
	"strconv"
	"strings"

	"github.com/skip2/go-qrcode"
)

// pairingQRCode renders the pairing payload as a terminal QR code.
//
// The payload is the same JSON the desktop card hands its frontend and the phone's scanner expects
// ({"v":1,"url":…,"urls":[…],"token":…}, see mobile_token.go) — one shape, three presentations: a
// QR image in the window, these block characters here, and the scanner on the phone. "urls" is the
// whole set of addresses, so one scan teaches the phone every way to reach this machine: on the
// home network it takes the 4ms LAN one, from elsewhere it takes the tailnet one.
//
// The one thing that has to be right is CONTRAST, and which direction that is depends on the
// terminal's background. ToSmallString's inverse flag swaps black and white; printing the dark
// modules as █ reads correctly on a light background, and comes out inverted on a dark one (a phone
// can usually still read it, but "usually" is not what a pairing screen should be). So the choice
// follows COLORFGBG when the terminal sets it, and falls back to dark — the common case for a
// terminal someone is running a server from.
//
// It lives in this file (no build tag) so it can be tested without the headless tag.
func pairingQRCode(payload string) (string, error) {
	code, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		return "", err
	}
	out := code.ToSmallString(terminalBackgroundIsLight())
	// A code wider than a normal terminal is folded by the terminal itself, and a folded QR code
	// cannot be scanned. Better to print nothing than a picture that looks scannable and is not.
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if len([]rune(line)) > 78 {
			return "", nil
		}
	}
	return out, nil
}

// terminalBackgroundIsLight reports whether the terminal is drawn on a light background.
//
// COLORFGBG is set by several terminals (Terminal.app, iTerm2, konsole, …) as "<fg>;<bg>" and
// sometimes with more segments; 0–6 are dark colours and 7–15 light, which is the only part of it
// that matters here. An unset or unparsable value answers false (dark), because guessing wrong there
// still produces a scannable inverted code, while guessing wrong the other way puts a mostly-black
// block on a black background.
func terminalBackgroundIsLight() bool {
	v := os.Getenv("COLORFGBG")
	if i := strings.LastIndex(v, ";"); i >= 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(v[i+1:])); err == nil {
			return n >= 7
		}
	}
	return false
}
