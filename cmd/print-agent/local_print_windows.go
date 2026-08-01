//go:build windows

package main

import (
	"fmt"

	winprinter "github.com/alexbrainman/printer"
)

// localPrinters lists the printers installed in Windows (what the operator sees in Settings >
// Printers). USB receipt printers appear here once their driver is installed.
func localPrinters() ([]string, error) {
	names, err := winprinter.ReadNames()
	if err != nil {
		return nil, fmt.Errorf("enumerate printers: %w", err)
	}
	return names, nil
}

// printLocal sends raw bytes (ESC/POS, TSPL, ZPL, …) to a locally-installed printer by name via
// the Windows spooler (RAW datatype — the driver passes the bytes straight through, which is
// what thermal receipt/label printers expect).
//
// Deliberately does NOT use winprinter.(*Printer).StartRawDocument: that helper first calls
// DriverInfo() (GetPrinterDriver at info level 8) to decide between "RAW" and "XPS_PASS" per
// Microsoft KB2779300's v4-driver guidance, and aborts the whole print if THAT probe call fails.
// Confirmed against a real Xprinter XP-330B (a classic RAW-passthrough label-printer driver):
// GetPrinterDriver(level 8) itself returns ERROR_INVALID_DATA ("The data is invalid") for this
// driver — not because RAW printing doesn't work, but because the driver doesn't support that
// particular info level — so StartRawDocument fails before ever attempting the real print, even
// though a direct StartDocument(name, "RAW") on the same printer handle succeeds immediately.
// Going straight to "RAW" (the overwhelmingly common case for receipt/label printers) and only
// falling back to "XPS_PASS" if that itself fails avoids depending on a driver-info query that
// isn't universally supported.
func printLocal(name string, data []byte) error {
	p, err := winprinter.Open(name)
	if err != nil {
		return fmt.Errorf("open printer %q: %w", name, err)
	}
	defer p.Close()
	if err := p.StartDocument("print job", "RAW"); err != nil {
		if xpsErr := p.StartDocument("print job", "XPS_PASS"); xpsErr != nil {
			return fmt.Errorf("start document (RAW: %v, XPS_PASS: %v)", err, xpsErr)
		}
	}
	defer p.EndDocument()
	if err := p.StartPage(); err != nil {
		return fmt.Errorf("start page: %w", err)
	}
	if _, err := p.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return p.EndPage()
}
