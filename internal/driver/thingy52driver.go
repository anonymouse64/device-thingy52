// -*- Mode: Go; indent-tabs-mode: t -*-
//
// Copyright (C) 2019 Canonical Ltd
//
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"math/big"
	"os"
	"sync"
	"time"

	dsModels "github.com/edgexfoundry/device-sdk-go/pkg/models"
	"github.com/edgexfoundry/go-mod-core-contracts/clients/logger"
	contract "github.com/edgexfoundry/go-mod-core-contracts/models"
	"github.com/muka/go-bluetooth/api"
	"github.com/pkg/errors"
)

// Thingy52Driver is a driver for a thingy52
type Thingy52Driver struct {
	lc            logger.LoggingClient
	readingsQueue []thingy52Heading
	readingCond   *sync.Cond
	Dev           *api.Device
	InterruptChan chan os.Signal
}

type thingy52Heading struct {
	heading   float32
	timestamp int64
}

// DisconnectDevice disconnects the device
func (d *Thingy52Driver) DisconnectDevice(addr *contract.Addressable) error {
	d.lc.Info(fmt.Sprintf("Thingy52Driver.DisconnectDevice: device-thingy52 driver at %v is disconnecting", addr.Address))
	return d.Dev.Disconnect()
}

// Initialize will start running a background go routine which gathers the data
// and pushes the data onto a queue which is then read from in the HandleReadCommands
// initialize expects the device in the driver to already be connected
func (d *Thingy52Driver) Initialize(lc logger.LoggingClient, asyncCh chan<- *dsModels.AsyncValues) error {
	d.lc = lc
	m := &sync.Mutex{}
	d.readingCond = sync.NewCond(m)
	d.readingsQueue = make([]thingy52Heading, 0)

	motionUUID := nordicUUID(motionHeadingCharUUID)

	// attempt to register for notifications for the heading characteristic
	// from the motion service
	errRes, err := RetryFunc(100, 100*time.Millisecond, func() error {
		return registerNotifyCallback(context.TODO(), d.Dev, motionUUID, func(b []byte) {
			// the heading is returned in fixed point Q16.16 (aka 16Q16 as
			// Nordic docs refer to ir) format, so first create an exact
			// integer from the full bytes and make a rational number dividing
			// by 2^16
			// see https://en.wikipedia.org/wiki/Q_(number_format)#Q_to_float
			headingFloat, _ := big.NewRat(int64(binary.LittleEndian.Uint32(b)), 1<<16).Float32()

			log.Println("heading:", headingFloat)
			d.readingCond.L.Lock()
			d.readingsQueue = append(d.readingsQueue, thingy52Heading{
				heading:   headingFloat,
				timestamp: time.Now().UnixNano() / int64(time.Millisecond),
			})

			d.readingCond.Broadcast()
			d.readingCond.L.Unlock()
		})
	})
	if err != nil {
		// return last error if there are multiple errors, or return the actual
		// operation err from retryFunc
		if len(errRes) > 0 {
			err = errRes[len(errRes)-1]
		}
		return errors.Wrapf(err, "registering notify callback for motion heading characteristic (uuid: %s) failed", motionUUID)
	}

	// wait for the first reading before continuing to ensure that by the time
	// we get real requests (and hence are "started") we can actually service
	// them
	d.readingCond.L.Lock()
	for len(d.readingsQueue) == 0 {
		d.readingCond.Wait()
	}
	d.readingCond.L.Unlock()

	return nil
}

// HandleReadCommands handles read commands from the device SDK
func (d *Thingy52Driver) HandleReadCommands(
	deviceName string,
	protocols map[string]contract.ProtocolProperties,
	reqs []dsModels.CommandRequest,
) (res []*dsModels.CommandValue, err error) {
	// if the device isn't connected fail immediately
	if !d.Dev.IsConnected() {
		err = fmt.Errorf("Thingy52Driver.HandleReadCommands; device is not connected")
		return
	}

	if len(reqs) != 1 {
		err = fmt.Errorf("Thingy52Driver.HandleReadCommands; too many command requests; only one supported")
		return
	}

	// get the time at the start of the request
	// this is useful in debugging how long requests take before a value is
	// recieved from the sensor
	start := time.Now()

	// acquire the lock for reading from the queue and wait until the queue
	// has some values before processing them
	d.readingCond.L.Lock()
	for len(d.readingsQueue) == 0 {
		d.readingCond.Wait()
	}
	defer d.readingCond.L.Unlock()

	// for this single read command, get all of the values from the queue and
	// average them
	// this results in smoother data when the frequency of data being created
	// by the thingy52 is higher than EdgeX can read it (which is currently max of 1hz)
	res = make([]*dsModels.CommandValue, 1)
	numReadings := float64(len(d.readingsQueue))
	lastReadingTime := d.readingsQueue[len(d.readingsQueue)-1].timestamp

	// we need to calculate the circular mean of the headings here, as if
	// we have 2 headings at 0 and 359, then the average is really around 359.5
	// but a normal arithmetic mean results in somewhere around 179.5 which is
	// wrong
	// so use the formula for circular mean from mean of angles from
	// https://en.wikipedia.org/wiki/Mean_of_circular_quantities#Mean_of_angles

	sinAngleSum := float64(0.0)
	cosAngleSum := float64(0.0)
	for _, reading := range d.readingsQueue {
		// turn the heading into a radian angle
		angle := float64(reading.heading) * math.Pi / 180.0
		// get the sine and cosine and add those to the running sums
		sin, cos := math.Sincos(angle)
		sinAngleSum += sin
		cosAngleSum += cos
	}

	// get the average by dividing by the number of readings
	sinAngleMean := sinAngleSum / numReadings
	cosAngleMean := cosAngleSum / numReadings

	// take the arctangent of the sin and cos means, convert to degrees and
	// then convert the degree in the range [-180,180] to [0,360] by adding
	// 360 and taking the mod of 360 so for example mod(-90+360,360) => 270,
	// and mod(100+360,360) == 100
	circularMean := float32(math.Mod(math.Atan2(sinAngleMean, cosAngleMean)/math.Pi*180.0+360.0, 360.0))

	// reset the queue
	d.readingsQueue = make([]thingy52Heading, 0)

	// create the command value for this request
	cv, _ := dsModels.NewFloat32Value(reqs[0].DeviceResourceName, lastReadingTime, circularMean)
	res[0] = cv

	// turn the average unix time back into a real time.Time that we can print
	// off for debugging
	unixTime := float64(lastReadingTime) / float64(time.Second/time.Millisecond)
	unixTimeSec := math.Floor(unixTime)
	unixTimeNSec := int64((unixTime - unixTimeSec) * float64(time.Second/time.Nanosecond))
	readingTime := time.Unix(int64(unixTimeSec), unixTimeNSec)

	d.lc.Debug(fmt.Sprintf("Thingy52Driver.HandleReadCommands: avg heading = %f @ t = %v took %v", circularMean, readingTime, time.Since(start)))
	return res, nil
}

// HandleWriteCommands does nothing becuase there are no write commands for
// this device-service
func (d *Thingy52Driver) HandleWriteCommands(deviceName string, protocols map[string]contract.ProtocolProperties, reqs []dsModels.CommandRequest, params []*dsModels.CommandValue) error {
	d.lc.Info(fmt.Sprintf("Thingy52Driver.HandleWriteCommands: device-thingy52 handling write commands for device %s", deviceName))
	return nil
}

// Stop will kill the background process reading from the program
func (d *Thingy52Driver) Stop(force bool) error {
	d.lc.Info("Thingy52Driver.Stop: device-thingy52 driver is stopping...")
	// try to disconnect the device and then trigger cleanup
	err := d.Dev.Disconnect()
	d.InterruptChan <- os.Interrupt
	return err
}
