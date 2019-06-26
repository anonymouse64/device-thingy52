package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	devicethingy52 "github.com/anonymouse64/device-thingy52"
	"github.com/anonymouse64/device-thingy52/internal/driver"
	"github.com/edgexfoundry/device-sdk-go/pkg/startup"
	"github.com/muka/go-bluetooth/api"
	"github.com/muka/go-bluetooth/emitter"
	"github.com/pkg/errors"
)

const (
	version     = devicethingy52.Version
	serviceName = "device-thingy52"
	// TODO: make these things configurable
	debug                   = true
	defaultBluetoothAdapter = "hci0"
	thingy52BLEAddress      = "EB:B8:28:5B:6A:38"
)

// run will performs the main actions with the device, setting up backgrond
// goroutines that continue running after run() returns
func run(cleanupDone chan struct{}, signalChan chan os.Signal) (*api.Device, error) {

	// setup an interrupt handler to disconnect from the device properly
	// if we get interrupted
	devCleanUpChan := make(chan *api.Device, 1)
	go func() {
		log.Println("waiting for os signal")
		<-signalChan
		log.Println("Received an interrupt, disconnecting device...")
		// try an immediate read on the channel, if it's empty or closed
		// do nothing
		select {
		case dev, gotValue := <-devCleanUpChan:
			if gotValue {
				dev.Disconnect()
			}
		default:
		}

		close(cleanupDone)
	}()

	if debug {
		log.Println("setup cleanup handler")
	}

	// make sure the adapter exists
	ok, err := api.AdapterExists(defaultBluetoothAdapter)
	if err != nil {
		return nil, errors.Wrapf(err, "checking adapter %s failed", defaultBluetoothAdapter)
	} else if !ok {
		return nil, fmt.Errorf("adapter %s doesn't exist", defaultBluetoothAdapter)
	}

	// try to turn on the adapter
	adapter, err := api.GetAdapter(defaultBluetoothAdapter)
	if err != nil {
		return nil, errors.Wrapf(err, "getting adapter %s failed", defaultBluetoothAdapter)
	} else if adapter == nil {
		return nil, fmt.Errorf("adapter %s is nil", defaultBluetoothAdapter)
	}

	if debug {
		log.Println("got adapter", defaultBluetoothAdapter)
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
			return nil, errors.Wrapf(err, "turning adapter %s on failed", defaultBluetoothAdapter)
		}
	}

	if debug {
		log.Println("ensured adapter is powered on")
	}

	// start discovery on the adapter
	if err := adapter.StartDiscovery(); err != nil {
		return nil, errors.Wrap(err, "starting discovery on adapter %s on failed")
	}

	if debug {
		log.Println("started discovery on adapter")
	}

	// Lookup the device with the specified address
	devChan := make(chan *api.Device, 1)
	errRes, err := driver.RetryFunc(100, 100*time.Millisecond, func() error {
		var err error
		dev, err := api.GetDeviceByAddress(thingy52BLEAddress)
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
		return nil, errors.Wrapf(err, "connecting to device with address %s failed", thingy52BLEAddress)
	}

	// get the device from the buffered send on the channel from the retry
	// function
	dev := <-devChan

	// send the device to the cleanup channel in the background
	devCleanUpChan <- dev

	if debug {
		log.Printf("created device handle at %p\n", dev)
	}

	// useful lambda function to call later
	var connect = func() error {
		if !dev.IsConnected() {
			err := dev.Connect()
			if err != nil {
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
	// try to connect to the device before starting the device service proper

	// setup channels for handling interrupts and safely exiting,
	// disconnecting from the device before exiting
	cleanupDone := make(chan struct{})
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)

	go func() {
		bleDev, err := run(cleanupDone, signalChan)
		if err != nil {
			// if we failed to run properly, log the error and trigger cleanup
			log.Print(err)
			signalChan <- os.Interrupt
			return
		}
		d := driver.Thingy52Driver{
			Dev:           bleDev,
			InterruptChan: signalChan,
		}
		// start the device service in the background
		startup.Bootstrap(serviceName, version, &d)
	}()

	// wait for the cleanup to finish
	<-cleanupDone
}
