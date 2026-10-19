package sysfs

import (
	"testing"

	"github.com/inerplat/dra-driver-fabric-nic/internal/sysfstest"
)

func TestParseVPD(t *testing.T) {
	for _, tc := range []struct {
		name   string
		b      []byte
		serial string
		mgmt   bool
	}{
		{"data NIC", sysfstest.VPD("MT2530J01EYY", ""), "MT2530J01EYY", false},
		{"NVLink bridge", sysfstest.VPD("MT25406000TD", "MLX:MN=MLNX:SMDL=SW_MNG:MODL=C7010Z"), "MT25406000TD", true},
		{"padded serial", sysfstest.VPD("SN123  ", ""), "SN123", false},
	} {
		v, err := ParseVPD(tc.b)
		if err != nil || v.Serial != tc.serial || v.NVLinkManagement != tc.mgmt {
			t.Errorf("%s: %+v %v", tc.name, v, err)
		}
	}
	if _, err := ParseVPD([]byte{0x90, 0x10, 0x00, 'S'}); err == nil {
		t.Error("truncated VPD parsed")
	}
}
