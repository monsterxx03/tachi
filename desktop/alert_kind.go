package main

// The two alert kinds the runtime can queue for the device (see alertIfUnwatched).
//
// They are spelled here rather than taken from desktop/link because that package is the BLE
// transport, and a headless build must not compile it at all (it needs CoreBluetooth / serial
// cgo). The values are part of the LINK PROTOCOL, so they must stay equal to link.AlertKindAsk /
// link.AlertKindDone — the badge's own tests pin the wire spelling, and these two pin the
// runtime's. If a third kind ever appears, it belongs in both places for the same reason.
const (
	alertKindAsk  = "ask"
	alertKindDone = "done"
)
