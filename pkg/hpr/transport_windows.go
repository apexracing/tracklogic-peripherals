//go:build windows

package hpr

import (
	"github.com/tracklogic/tracklogic-peripherals/internal/hidtransport"
)

// hidScanner adapts hidtransport.Scanner to hpr.DeviceScanner.
// It lives here (not in hidtransport) to avoid an import cycle:
// hidtransport must not depend on hpr.
type hidScanner struct{}

func (hidScanner) ScanDevices() ([]DeviceInfo, error) {
	raw, err := hidtransport.NewScanner().Scan()
	if err != nil {
		return nil, err
	}
	out := make([]DeviceInfo, 0, len(raw))
	for _, d := range raw {
		out = append(out, deviceDescriptorToInfo(d))
	}
	return out, nil
}

// hidOpener adapts hidtransport.Open to hpr.TransportOpener.
func hidOpener(info DeviceInfo) (Transport, error) {
	return hidtransport.Open(infoToDescriptor(info))
}

func defaultDeviceScanner() DeviceScanner { return hidScanner{} }

func defaultTransportOpener() TransportOpener { return hidOpener }

// deviceDescriptorToInfo lifts a platform descriptor to the
// universal hpr.DeviceInfo. DriverName and Model are filled in
// later by Manager.decorate.
func deviceDescriptorToInfo(d hidtransport.DeviceDescriptor) DeviceInfo {
	return DeviceInfo{
		DevicePath:    d.DevicePath,
		FriendlyName:  d.FriendlyName,
		Manufacturer:  d.Manufacturer,
		Product:       d.Product,
		VendorID:      d.VendorID,
		ProductID:     d.ProductID,
		VersionNumber: d.VersionNumber,
		UsagePage:     d.UsagePage,
		Usage:         d.Usage,
	}
}

func infoToDescriptor(info DeviceInfo) hidtransport.DeviceDescriptor {
	return hidtransport.DeviceDescriptor{
		DevicePath:    info.DevicePath,
		FriendlyName:  info.FriendlyName,
		Manufacturer:  info.Manufacturer,
		Product:       info.Product,
		VendorID:      info.VendorID,
		ProductID:     info.ProductID,
		VersionNumber: info.VersionNumber,
		UsagePage:     info.UsagePage,
		Usage:         info.Usage,
	}
}
