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

// printLocal sends raw ESC/POS bytes to a locally-installed printer by name via the Windows
// spooler (RAW datatype — the driver passes the bytes straight through, which is what thermal
// receipt printers expect).
func printLocal(name string, data []byte) error {
	p, err := winprinter.Open(name)
	if err != nil {
		return fmt.Errorf("open printer %q: %w", name, err)
	}
	defer p.Close()
	if err := p.StartRawDocument("POS receipt"); err != nil {
		return fmt.Errorf("start document: %w", err)
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
