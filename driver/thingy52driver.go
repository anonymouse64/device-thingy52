// -*- Mode: Go; indent-tabs-mode: t -*-
//
// Copyright (C) 2019 Canonical Ltd
//
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os/exec"
	"sync"
	"time"

	dsModels "github.com/edgexfoundry/device-sdk-go/pkg/models"
	logger "github.com/edgexfoundry/edgex-go/pkg/clients/logging"
	"github.com/edgexfoundry/edgex-go/pkg/models"
)

// Thingy52Driver is a driver for a thingy52
type Thingy52Driver struct {
	lc            logger.LoggingClient
	readingsQueue []thingy52Heading
	readingCond   *sync.Cond
}

type thingy52Heading struct {
	heading   float32
	timestamp int64
}

// DisconnectDevice disconnects the device
func (d *Thingy52Driver) DisconnectDevice(addr *models.Addressable) error {
	d.lc.Info(fmt.Sprintf("Thingy52Driver.DisconnectDevice: device-thingy52 driver at %v is disconnecting", addr.Address))
	return nil
}

// Initialize will start running a background go routine which gathers the data
// and pushes the data onto a stack which is then read from in the HandleReadCommands
func (d *Thingy52Driver) Initialize(lc logger.LoggingClient, asyncCh chan<- *dsModels.AsyncValues) error {
	d.lc = lc
	m := &sync.Mutex{}
	d.readingCond = sync.NewCond(m)

	d.readingsQueue = make([]thingy52Heading, 0)

	// start the python process reading from the thingy52
	cmd := exec.Command("python3", "/home/ijohnson/git/bluepy/bluepy/thingy52-edgex.py", `-n=-1`, "-t", "1", "--heading", "c2:fe:6a:0f:91:15")
	cmdStdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmdStderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	cmd.Start()
	pid := cmd.Process.Pid

	// TODO: fix this to be able to check on the process after it was started
	// check that the process is still running after 1 second
	// time.Sleep(5 * time.Second)
	// if cmd.ProcessState.Exited() {
	// 	stdout, err := ioutil.ReadAll(cmdStdout)
	// 	if err != nil {
	// 		return err
	// 	}
	// 	stderr, err := ioutil.ReadAll(cmdStderr)
	// 	if err != nil {
	// 		return err
	// 	}
	// 	return fmt.Errorf("error starting python process (died with exit code %d): stdout: %s\nstderr: %s", cmd.ProcessState.ExitCode(), string(stdout), string(stderr))
	// }

	d.lc.Info(fmt.Sprintf("Thingy52Driver.Initialize: device-thingy52 reading process started with PID %v", pid))

	// in a background thread start listening for ble data and pushing it onto
	// the stack
	go func() {
		// in an infinte loop read data from the python process
		dec := json.NewDecoder(cmdStdout)
		var reading float32
		for {
			if err := dec.Decode(&reading); err == io.EOF {
				d.lc.Error(fmt.Sprintf("error decoding data from python process: stdout pipe closed: %s", err))
				stderr, err := ioutil.ReadAll(cmdStderr)
				if err != nil {
					d.lc.Error(fmt.Sprintf("error decoding data from python process: failed to read stderr: %s", err))
				}
				d.lc.Error(fmt.Sprintf("error decoding data from python process: stderr: %s", stderr))
				return
			} else if err != nil {
				d.lc.Error(fmt.Sprintf("error decoding data from python process: %s", err))
				return
			}

			// lock the condition variable, append a new reading on the
			// list and broadcast to all waiting go routines
			d.readingCond.L.Lock()
			d.readingsQueue = append(d.readingsQueue, thingy52Heading{
				heading:   reading,
				timestamp: time.Now().UnixNano() / int64(time.Millisecond),
			})

			d.readingCond.Broadcast()
			d.readingCond.L.Unlock()
		}
	}()

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
	addr *models.Addressable,
	reqs []dsModels.CommandRequest,
) (res []*dsModels.CommandValue, err error) {

	if len(reqs) != 1 {
		err = fmt.Errorf("Thingy52Driver.HandleReadCommands; too many command requests; only one supported")
		return
	}

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
	// ok := true
	// valid := true
	// var reading thingy52Heading
	var avgHeading float32
	var avgTime int64
	numReadings := int64(0)

	for _, reading := range d.readingsQueue {
		avgHeading *= float32(numReadings)
		avgHeading += reading.heading
		avgHeading /= float32(numReadings + 1)

		avgTime *= numReadings
		avgTime += reading.timestamp
		avgTime /= (numReadings + 1)
		numReadings++
	}
	d.readingsQueue = make([]thingy52Heading, 0)
	// for numReadings < 1 && ok && valid {
	// 	select {
	// 	case reading, valid = <-d.readingsQueue:
	// 		ok = true
	// 	default:
	// 		ok = false
	// 	}
	// 	// if we read a value off the queue successfully, then
	// 	// iteratively calculate the average
	// 	fmt.Println("avgHeading", avgHeading)
	// 	fmt.Println("reading.heading", reading.heading)
	// 	fmt.Println("numReadings", numReadings)
	// 	if ok && valid {
	// 		avgHeading *= float32(numReadings)
	// 		avgHeading += reading.heading
	// 		avgHeading /= float32(numReadings + 1)

	// 		avgTime *= numReadings
	// 		avgTime += reading.timestamp
	// 		avgTime /= (numReadings + 1)
	// 		numReadings++
	// 	}
	// }

	cv, _ := dsModels.NewFloat32Value(&reqs[0].RO, avgTime, avgHeading)
	res[0] = cv

	// turn the average unix time back into a real time.Time that we can print
	// off for debuggins
	unixTime := float64(avgTime) / float64(time.Second/time.Millisecond)
	unixTimeSec := math.Floor(unixTime)
	unixTimeNSec := int64((unixTime - unixTimeSec) * float64(time.Second/time.Nanosecond))
	readingTime := time.Unix(int64(unixTimeSec), unixTimeNSec)

	d.lc.Info(fmt.Sprintf("heading = %f @ t = %v", avgHeading, readingTime))
	d.lc.Info(fmt.Sprintf("Thingy52Driver.HandleReadCommands: device-thingy52 handling %d read commands for device at %v", len(res), addr.Address))

	return res, nil
}

// HandleWriteCommands does nothing becuase there are no write commands for
// this device-service
func (d *Thingy52Driver) HandleWriteCommands(addr *models.Addressable, params []dsModels.CommandRequest, val []*dsModels.CommandValue) error {
	d.lc.Info(fmt.Sprintf("Thingy52Driver.HandleWriteCommands: device-thingy52 handling read commands for device at %v", addr.Address))
	return nil
}

// Stop will kill the background process reading from the program
func (d *Thingy52Driver) Stop(force bool) error {
	d.lc.Info("Thingy52Driver.Stop: device-thingy52 driver is stopping...")
	return nil
}
