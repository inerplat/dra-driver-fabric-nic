package sysfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// VPD is what prep uses from a function's PCI Vital Product Data.
type VPD struct {
	Serial string // the SN keyword of the read-only section
	// NVLinkManagement is set when the data carries SW_MNG, the marker
	// NVIDIA programs into the ConnectX bridges that manage NVLink switches.
	NVLinkManagement bool
}

// ReadVPD reads and parses <dev>/vpd. Reading VPD goes to the device and
// can stall, so it gives up after timeout.
func ReadVPD(dev string, timeout time.Duration) (VPD, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := os.Open(dev + "/vpd")
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, 32<<10))
		ch <- result{b, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return VPD{}, r.err
		}
		return ParseVPD(r.b)
	case <-time.After(timeout):
		return VPD{}, fmt.Errorf("reading %s/vpd timed out after %s", dev, timeout)
	}
}

// ParseVPD walks the resource tags of PCI VPD (PCI Local Bus 3.0, section
// 6.4) and returns the serial number from the read-only section.
func ParseVPD(b []byte) (VPD, error) {
	v := VPD{NVLinkManagement: bytes.Contains(b, []byte("SW_MNG"))}
	for i := 0; i < len(b); {
		tag := b[i]
		if tag&0x80 == 0 { // small resource
			if tag>>3&0x0f == 0x0f { // end tag
				break
			}
			i += 1 + int(tag&0x07)
			continue
		}
		if i+3 > len(b) {
			return v, errors.New("vpd: truncated large resource header")
		}
		n := int(b[i+1]) | int(b[i+2])<<8
		data := b[i+3:]
		if n > len(data) {
			return v, errors.New("vpd: truncated large resource")
		}
		data = data[:n]
		if tag == 0x90 { // VPD-R
			for j := 0; j+3 <= len(data); {
				kw, l := string(data[j:j+2]), int(data[j+2])
				if j+3+l > len(data) {
					return v, errors.New("vpd: truncated keyword")
				}
				if kw == "SN" {
					v.Serial = string(bytes.Trim(data[j+3:j+3+l], " \x00"))
				}
				j += 3 + l
			}
		}
		i += 3 + n
	}
	return v, nil
}
