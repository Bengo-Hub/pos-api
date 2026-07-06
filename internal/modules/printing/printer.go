package printing

import (
	"fmt"
	"net"
	"strconv"
	"time"
)

// PrinterProfile mirrors the printer_profiles JSON array stored in OutletSetting.
type PrinterProfile struct {
	ID          string   `json:"id"` // "customer" | "waiter" | a KDS station UUID
	Label       string   `json:"label"`
	PrinterType string   `json:"printer_type"` // "network" | "usb" | "os" | "bluetooth" | "thermal" | "browser" | "none"
	PrinterIP   string   `json:"printer_ip"`
	PrinterPort int      `json:"printer_port"` // raw-socket port, default 9100
	PrinterName string   `json:"printer_name"` // OS spooler name for usb/os/bluetooth targets
	PaperSize   string   `json:"paper_size"`   // "58mm" | "76mm" | "80mm" | "A6" | "A5" | "A4" | "Letter"
	PaperWidth  string   `json:"paper_width"`  // legacy field, same values as paper_size
	AutoPrint   bool     `json:"auto_print"`
	Categories  []string `json:"categories"` // empty = all categories
	StationID   string   `json:"station_id"`
	StationType string   `json:"station_type"` // kitchen | bar | expo | all
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
	addr := net.JoinHostPort(p.IP, strconv.Itoa(p.Port))
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
