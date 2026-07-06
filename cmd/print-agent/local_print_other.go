//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// localPrinters lists CUPS printer queues (`lpstat -e`).
func localPrinters() ([]string, error) {
	out, err := exec.Command("lpstat", "-e").Output()
	if err != nil {
		return nil, fmt.Errorf("lpstat: %w", err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// printLocal sends raw ESC/POS bytes to a CUPS queue by name (`lp -d <name> -o raw`).
func printLocal(name string, data []byte) error {
	tmp, err := os.CreateTemp("", "pos-print-*.bin")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if out, err := exec.Command("lp", "-d", name, "-o", "raw", tmp.Name()).CombinedOutput(); err != nil {
		return fmt.Errorf("lp: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
