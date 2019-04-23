package main

import (
	devicethingy52 "github.com/anonymouse64/device-thingy52"
	"github.com/anonymouse64/device-thingy52/driver"
	"github.com/edgexfoundry/device-sdk-go/pkg/startup"
)

const (
	version     string = devicethingy52.Version
	serviceName string = "device-thingy52"
)

func main() {
	d := driver.Thingy52Driver{}
	startup.Bootstrap(serviceName, version, &d)
}
