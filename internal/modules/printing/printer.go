package printing

import (
	"fmt"
	"net"
	"time"
)

// PrinterProfile mirrors the printer_profiles JSON array stored in OutletSetting.
type PrinterProfile struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	PrinterType string   `json:"printer_type"` // "network" | "thermal" | "bluetooth" | "browser" | "none"
	PrinterIP   string   `json:"printer_ip"`
	PaperWidth  string   `json:"paper_width"` // "58mm" | "80mm"
	AutoPrint   bool     `json:"auto_print"`
	Categories  []string `json:"categories"` // empty = all categories
}

// NetworkPrinter sends raw ESC/POS bytes to a network printer via TCP on port 9100.
type NetworkPrinter struct {
	IP      string
	Port    int
	Timeout time.Duration
}

func NewNetworkPrinter(ip string) *NetworkPrinter {
	return &NetworkPrinter{IP: ip, Port: 9100, Timeout: 5 * time.Second}
}

// Print dials the printer and writes the raw bytes.
func (p *NetworkPrinter) Print(data []byte) error {
	addr := fmt.Sprintf("%s:%d", p.IP, p.Port)
	conn, err := net.DialTimeout("tcp", addr, p.Timeout)
	if err != nil {
		return fmt.Errorf("printing: dial %s: %w", addr, err)
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(p.Timeout)); err != nil {
		return err
	}
	_, err = conn.Write(data)
	return err
}

// FindProfileByID returns the printer profile with the given ID from a slice.
func FindProfileByID(profiles []PrinterProfile, id string) *PrinterProfile {
	for i := range profiles {
		if profiles[i].ID == id {
			return &profiles[i]
		}
	}
	return nil
}
