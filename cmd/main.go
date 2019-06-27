package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	devicethingy52 "github.com/anonymouse64/device-thingy52"
	"github.com/anonymouse64/device-thingy52/internal/driver"
	device "github.com/edgexfoundry/device-sdk-go"
	"github.com/muka/go-bluetooth/api"
	"github.com/muka/go-bluetooth/emitter"
	"github.com/pkg/errors"
)

const (
	version                 = devicethingy52.Version
	serviceName             = "device-thingy52"
	defaultBluetoothAdapter = "hci0"
)

var (
	confProfile string
	confDir     string
	useRegistry string
	debug       bool
)

// run will performs the main actions with the device, setting up backgrond
// goroutines that continue running after run() returns
func run(addr, adapterName string) (*api.Device, error) {

	if debug {
		log.Println("setup cleanup handler")
	}

	// make sure the adapter exists
	ok, err := api.AdapterExists(adapterName)
	if err != nil {
		return nil, errors.Wrapf(err, "checking adapter %s failed", adapterName)
	} else if !ok {
		return nil, fmt.Errorf("adapter %s doesn't exist", adapterName)
	}

	// try to turn on the adapter
	adapter, err := api.GetAdapter(adapterName)
	if err != nil {
		return nil, errors.Wrapf(err, "getting adapter %s failed", adapterName)
	} else if adapter == nil {
		return nil, fmt.Errorf("adapter for %s is nil", adapterName)
	}

	if debug {
		log.Println("got adapter for", adapterName)
	}

	// ensure the adapter is powered
	if !adapter.Properties.Powered {
		errRes, err := driver.RetryFunc(10, 100*time.Millisecond, func() error {
			return adapter.SetProperty("Powered", true)
		})
		if err != nil {
			// return last error if there are multiple errors, or return the actual
			// operation err from retryFunc
			if len(errRes) > 0 {
				err = errRes[len(errRes)-1]
			}
			return nil, errors.Wrapf(err, "turning adapter %s on failed", adapterName)
		}
	}

	if debug {
		log.Printf("ensured adapter %s is powered on\n", adapterName)
	}

	// start discovery on the adapter
	if err := adapter.StartDiscovery(); err != nil {
		return nil, errors.Wrapf(err, "starting discovery on adapter %s on failed", adapterName)
	}

	if debug {
		log.Println("started discovery on adapter")
	}

	// Lookup the device with the specified address
	devChan := make(chan *api.Device, 1)
	errRes, err := driver.RetryFunc(100, 100*time.Millisecond, func() error {
		var err error
		dev, err := api.GetDeviceByAddress(addr)
		if err != nil {
			return err
		} else if dev == nil {
			return errors.New("device is nil")
		}
		devChan <- dev
		return nil
	})
	if err != nil {
		// return last error if there are multiple errors, or return the actual
		// operation err from retryFunc
		if len(errRes) > 0 {
			err = errRes[len(errRes)-1]
		}
		return nil, errors.Wrapf(err, "connecting to device with address %s failed", addr)
	}

	// get the device from the buffered send on the channel from the retry
	// function
	dev := <-devChan

	if debug {
		log.Printf("created device handle at %p\n", dev)
	}

	// useful lambda function to call later
	var connect = func() error {
		if !dev.IsConnected() {
			err := dev.Connect()
			if err != nil {
				log.Println("failed to connect", err)
				return err
			}
		}
		return nil
	}

	// create an event handler for the device changing so we can handle
	// disconnects and try to do reconnections if the
	// device was disconnected
	err = dev.On("changed", emitter.NewCallback(func(ev emitter.Event) {
		changed, ok := ev.GetData().(api.PropertyChangedEvent)
		if !ok {
			log.Println("invalid type for event data, can't cast to PropertyChangedEvent")
			return
		}
		if changed.Field == "Connected" {
			conn, ok := changed.Value.(bool)
			if ok && !conn {
				log.Printf("[CALLBACK] Device disconnected...")
				// try to re-connect every second indefinitely in a background
				// go routine
				go driver.RetryFunc(0, time.Second, connect)
			} else if !ok {
				log.Println("[CALLBACK] invalid changed value type for connected field")
			} else {
				log.Println("[CALLBACK] Device connected!")
				// for initial connections, we want to stop discovery after
				// we were connected, so check if the adapter is discovering
				if adapter.Properties.Discovering {
					if err := adapter.StopDiscovery(); err != nil {
						log.Println(errors.Wrap(err, "stopping discovery on adapter %s failed"))
					}
				}
			}
		}
	}))

	if err != nil {
		return nil, errors.Wrap(err, "event handler setup failed")
	}

	if debug {
		log.Println("setup connection event handler")
	}

	// try to connect
	errRes, err = driver.RetryFunc(100, 100*time.Millisecond, connect)
	if err != nil {
		// return last error if there are multiple errors, or return the actual
		// operation err from retryFunc
		if len(errRes) > 0 {
			err = errRes[len(errRes)-1]
		}
		return nil, errors.Wrap(err, "device connection failed")
	}

	if debug {
		log.Println("device connected successfully!")
	}

	return dev, nil
}

func main() {
	flag.StringVar(&useRegistry, "registry", "", "Indicates the service should use the registry and provide the registry url.")
	flag.StringVar(&useRegistry, "r", "", "Indicates the service should use registry and provide the registry path.")
	flag.StringVar(&confProfile, "profile", "", "Specify a profile other than default.")
	flag.StringVar(&confProfile, "p", "", "Specify a profile other than default.")
	flag.StringVar(&confDir, "confdir", "", "Specify an alternate configuration directory.")
	flag.StringVar(&confDir, "c", "", "Specify an alternate configuration directory.")
	flag.Parse()

	// setup channels for handling interrupts and safely exiting,
	// disconnecting from the device before exiting
	cleanupDone := make(chan struct{})
	errChan := make(chan error)
	go func() {
		c := make(chan os.Signal)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
		errChan <- fmt.Errorf("%s", <-c)
	}()

	// setup an interrupt handler to disconnect from the device properly
	// if we get interrupted
	svcCleanUpChan := make(chan *device.Service, 1)
	go func() {
		// wait for an error
		err := <-errChan
		// try an immediate read on the service cleanup channel, if it's empty
		// or closed then do nothing
		select {
		case svc, gotValue := <-svcCleanUpChan:
			if gotValue {
				// stop the service in a safe way
				svc.Stop(false)
			}
		default:
		}

		// print the error message and close the cleanup channel
		// TODO: use edgex logging facilities here
		log.Println(err)

		// close the cleanupDone channel so the main goroutine exits
		close(cleanupDone)
	}()

	go func() {
		// make a new driver and a dev service out of the driver
		d := driver.NewThingy52Driver(errChan)
		svc, err := device.NewService(serviceName, version, confProfile, confDir, useRegistry, d)
		if err != nil {
			// don't need to send the svc before here, because the error
			// goroutine doesn't block waiting for the service and will
			// finish without receiving a service to stop
			errChan <- err
			return
		}

		// send the service on the buffered service cleanup channel so it will
		// be stopped if hit any critical errors on the errChan
		svcCleanUpChan <- svc

		// handle debug configuration
		config := device.DriverConfigs()
		if debugConfig, ok := config["Debug"]; ok {
			debug, err = strconv.ParseBool(debugConfig)
			if err != nil {
				errChan <- errors.Wrap(err, "\"Debug\" setting in configuration.toml is invalid")
				return
			}
		}

		// start the device service in order to import the devices from the
		// configuration.toml file
		// do this in the background because we need Start to load the devices
		// from the configuration.toml file, but we also want it to be blocked
		// until we have a chance to actually setup the connections for
		// those devices
		go func() {
			err := svc.Start(errChan)
			if err != nil {
				errChan <- err
				return
			}
		}()

		// wait for the driver to have it's initialization started
		err = d.WaitForInitStarted(context.TODO())
		if err != nil {
			errChan <- err
			return
		}

		// get all the devices from the service and register them all with
		// the driver
		for _, dev := range svc.Devices() {
			if debug {
				log.Println("setting up dev", dev.Name)
			}

			// for each device, get the protocols and the BLE address
			// in order to actually make a BLE connection to the device
			devProto := dev.Protocols
			if devProto == nil {
				errChan <- errors.Errorf("no \"Protocols\" specified in configuration.toml for device %s", dev.Name)
				return
			}
			devProps, ok := devProto["BLE"]
			if !ok {
				errChan <- errors.Errorf("no \"BLE\" key in \"Protocols\" map in configuration.toml for device %s", dev.Name)
				return
			}
			addr, ok := devProps["Address"]
			if !ok {
				errChan <- errors.Errorf("no \"Address\" key in \"BLE\" map in configuration.toml for device %s", dev.Name)
				return
			}

			// if no adapter was specified, use the default
			adapterName, ok := devProps["Adapter"]
			if !ok {
				adapterName = defaultBluetoothAdapter
			}

			// try to setup the adapter and connect to the device at the
			// address
			bleDev, err := run(addr, adapterName)
			if err != nil {
				// if we failed to run properly, log the error and trigger cleanup
				errChan <- err
				return
			}

			// register the device with the driver
			err = d.RegisterBLEDevice(dev.Name, bleDev)
			if err != nil {
				errChan <- err
				return
			}
		}

		// inform the driver we are done registering devices and it can
		// continue with initialization and starting
		err = d.FinishedRegisteringDevices(context.TODO())
		if err != nil {
			errChan <- err
			return
		}

		// at this point the goroutine for svc.Start should return
		// and this routine should return leaving just the error checking
		// goroutine, the driver's goroutines, the device SDK's goroutines
		// and the main goroutine here left running
	}()

	// wait for the cleanup to finish
	<-cleanupDone
}
