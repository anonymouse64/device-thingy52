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
	"math"
	"math/big"
	"sync"
	"time"

	dsModels "github.com/edgexfoundry/device-sdk-go/pkg/models"
	"github.com/edgexfoundry/go-mod-core-contracts/clients/logger"
	contract "github.com/edgexfoundry/go-mod-core-contracts/models"
	"github.com/muka/go-bluetooth/api"
	"github.com/pkg/errors"
)

type thingy52Dev struct {
	bleDev        *api.Device
	readingsQueue []thingy52Heading
	readingCond   *sync.Cond
}

// Thingy52Driver is a driver for a thingy52
type Thingy52Driver struct {
	lc                  logger.LoggingClient
	devMap              map[string]*thingy52Dev
	errChan             chan error
	initChan            chan struct{}
	devRegistrationDone chan struct{}
}

// NewThingy52Driver creates a new ProtocolDriver with the associated
// error channel
func NewThingy52Driver(errChan chan error) *Thingy52Driver {
	devMap := make(map[string]*thingy52Dev)
	initChan := make(chan struct{})
	devRegistrationDone := make(chan struct{})
	return &Thingy52Driver{
		devMap:              devMap,
		errChan:             errChan,
		initChan:            initChan,
		devRegistrationDone: devRegistrationDone,
	}
}

// WaitForInitStarted will block until d.Initialize() is called
func (d *Thingy52Driver) WaitForInitStarted(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return errors.Wrap(ctx.Err(), "waiting for Initialize interrupted")
	case <-d.initChan:
		return nil
	}
}

// FinishedRegisteringDevices will send a signal that devices are done being
// registered with the driver and Initialize can finish
func (d *Thingy52Driver) FinishedRegisteringDevices(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return errors.Wrap(
			ctx.Err(),
			"finishing registering devices interrupted",
		)
	case d.devRegistrationDone <- struct{}{}:
		return nil
	}
}

// waitForDevicesRegistered waits until the registration is done
func (d *Thingy52Driver) waitForDevicesRegistered(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return errors.Wrap(
			ctx.Err(),
			"waiting for devices registered interrupted",
		)
	case <-d.devRegistrationDone:
		return nil
	}
}

// RegisterBLEDevice adds a device to the Thingy52Driver and starts running a
// background goroutine which gathers the data and pushes the data onto a
// queue which is then read from in the HandleReadCommands
// RegisterBLEDevice expects the device to already have been connected and
// ready to be read from
// must be called after Initialize has been started, but before
// FinishedRegisteringDevices is called
func (d *Thingy52Driver) RegisterBLEDevice(
	devName string,
	dev *api.Device,
) error {
	// TODO: fail here if this is called before Initialize has started or
	// after FinishedRegisteringDevices has been called

	// setup the caching queue for this device
	m := &sync.Mutex{}
	readingCond := sync.NewCond(m)
	readingsQueue := make([]thingy52Heading, 0)

	d.devMap[devName] = &thingy52Dev{
		bleDev:        dev,
		readingsQueue: readingsQueue,
		readingCond:   readingCond,
	}

	// get the motion characteristic UUID
	motionUUID := nordicUUID(motionHeadingCharUUID)

	// TODO: check that the device is actually connected before attempting to
	// register a notify callback

	// attempt to register for notifications for the heading characteristic
	// from the motion service
	errRes, err := RetryFunc(100, 100*time.Millisecond, func() error {
		return registerNotifyCallback(
			context.TODO(),
			d.errChan,
			dev,
			motionUUID,
			func(b []byte,
			) {
				// the heading is returned in fixed point Q16.16 (aka 16Q16 as
				// Nordic docs refer to it) format, so first create an exact
				// integer from the full bytes and make a rational number
				// dividing by 2^16
				// see https://en.wikipedia.org/wiki/Q_(number_format)#Q_to_float
				headingFloat, _ := big.NewRat(
					int64(binary.LittleEndian.Uint32(b)),
					1<<16,
				).Float32()

				d.lc.Debug(fmt.Sprintf("heading: %f", headingFloat))
				d.devMap[devName].readingCond.L.Lock()

				d.devMap[devName].readingsQueue = append(
					d.devMap[devName].readingsQueue,
					thingy52Heading{
						heading: headingFloat,
						timestamp: time.Now().UnixNano() /
							int64(time.Millisecond),
					},
				)

				d.devMap[devName].readingCond.Broadcast()
				d.devMap[devName].readingCond.L.Unlock()
			})
	})
	if err != nil {
		// return last error if there are multiple errors, or return the actual
		// operation err from retryFunc
		if len(errRes) > 0 {
			err = errRes[len(errRes)-1]
		}
		return errors.Wrapf(
			err,
			"registering notify callback for motion heading characteristic (uuid: %s) failed",
			motionUUID,
		)
	}

	// wait for the first reading before continuing to ensure that by the time
	// we get real requests (and hence are "started") we can actually service
	// them
	d.devMap[devName].readingCond.L.Lock()
	for len(d.devMap[devName].readingsQueue) == 0 {
		readingCond.Wait()
	}
	d.devMap[devName].readingCond.L.Unlock()
	return nil
}

type thingy52Heading struct {
	heading   float32
	timestamp int64
}

// Initialize currently saves the logging client and notifies the driver
// that the initialization sequence has begun, then waits to finish
// initialization until the driver is notified that all devices have been
// registered
// must be run in different goroutine than FinishedRegisteringDevices and a
// different goroutine than WaitForInitStarted (though those two functions can
// themselves be run in the sam goroutine as each other)
func (d *Thingy52Driver) Initialize(
	lc logger.LoggingClient,
	asyncCh chan<- *dsModels.AsyncValues,
) error {
	d.lc = lc
	// send a struct to inform that Initialize has been called
	d.initChan <- struct{}{}
	// now wait for all the devices to be registered
	d.waitForDevicesRegistered(context.TODO())
	return nil
}

// HandleReadCommands handles read commands from the device SDK
// Currently only supports a single request at a time
func (d *Thingy52Driver) HandleReadCommands(
	deviceName string,
	protocols map[string]contract.ProtocolProperties,
	reqs []dsModels.CommandRequest,
) (res []*dsModels.CommandValue, err error) {

	// get the device from the driver's map
	dev, ok := d.devMap[deviceName]
	if !ok {
		d.lc.Warn(fmt.Sprintf("unknown device %s", deviceName))
		err = fmt.Errorf("Thingy52Driver.HandleReadCommands; device %s does not exist in the BLE device map", deviceName)
		return
	}

	// check if the device is connected
	if !dev.bleDev.IsConnected() {
		d.lc.Warn(fmt.Sprintf("device %s is not connected", deviceName))
		err = fmt.Errorf("Thingy52Driver.HandleReadCommands; device is not connected")
		return
	}

	// only support a single read request for now
	if len(reqs) != 1 {
		err = fmt.Errorf("Thingy52Driver.HandleReadCommands; too many command requests; only one supported")
		return
	}

	// get the time at the start of the request
	// this is useful in debugging how long requests take before a value is
	// received from the sensor
	start := time.Now()

	// acquire the lock for reading from the queue and wait until the queue
	// has some values before processing them
	dev.readingCond.L.Lock()
	for len(dev.readingsQueue) == 0 {
		dev.readingCond.Wait()
	}
	defer dev.readingCond.L.Unlock()

	// for this single read command, get all of the values from the queue and
	// average them
	// this results in smoother data when the frequency of data being created
	// by the thingy52 is higher than EdgeX can read it (which is currently max of 1hz)
	res = make([]*dsModels.CommandValue, 1)
	numReadings := float64(len(dev.readingsQueue))
	lastReadingTime := dev.readingsQueue[len(dev.readingsQueue)-1].timestamp

	// we need to calculate the circular mean of the headings here, as if
	// we have 2 headings at 0 and 359, then the average is really around 359.5
	// but a normal arithmetic mean results in somewhere around 179.5 which is
	// wrong
	// so use the formula for circular mean from mean of angles from
	// https://en.wikipedia.org/wiki/Mean_of_circular_quantities#Mean_of_angles

	sinAngleSum := float64(0.0)
	cosAngleSum := float64(0.0)
	for _, reading := range dev.readingsQueue {
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
	circularMean := float32(math.Mod(
		math.Atan2(sinAngleMean, cosAngleMean)/math.Pi*180.0+360.0,
		360.0,
	))

	// reset the queue
	dev.readingsQueue = make([]thingy52Heading, 0)

	// create the command value for this request
	cv, _ := dsModels.NewFloat32Value(
		reqs[0].DeviceResourceName,
		lastReadingTime,
		circularMean,
	)
	res[0] = cv

	// turn the average unix time back into a real time.Time that we can print
	// off for debugging
	unixTime := float64(lastReadingTime) /
		float64(time.Second/time.Millisecond)
	unixTimeSec := math.Floor(unixTime)
	unixTimeNSec := int64((unixTime - unixTimeSec) *
		float64(time.Second/time.Nanosecond))
	readingTime := time.Unix(int64(unixTimeSec), unixTimeNSec)

	d.lc.Debug(fmt.Sprintf(
		"Thingy52Driver.HandleReadCommands: avg heading = %f @ t = %v took %v",
		circularMean,
		readingTime,
		time.Since(start)),
	)
	return res, nil
}

// HandleWriteCommands does nothing because there are no write commands for
// this device-service
func (d *Thingy52Driver) HandleWriteCommands(
	deviceName string,
	protocols map[string]contract.ProtocolProperties,
	reqs []dsModels.CommandRequest,
	params []*dsModels.CommandValue,
) error {
	d.lc.Info(fmt.Sprintf("Thingy52Driver.HandleWriteCommands: device-thingy52 handling write commands for device %s", deviceName))
	return nil
}

// Stop will kill the background process reading from the program
func (d *Thingy52Driver) Stop(force bool) (err error) {
	// Stop could get called before Initialize, when we receive our logger,
	// so check if it's nil before using it
	if d.lc != nil {
		d.lc.Info("Thingy52Driver.Stop: device-thingy52 driver is stopping...")
	}

	// try to disconnect the devices and then trigger cleanup
	for _, dev := range d.devMap {
		// TODO more elegantly handle the case where one device disconnect
		// fails but other devices didn't
		// currently if the first device fails to disconnect, but the last
		// one succeeds no error is returned
		err = dev.bleDev.Disconnect()
	}
	return
}
