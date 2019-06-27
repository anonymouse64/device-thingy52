package driver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/godbus/dbus"
	"github.com/muka/go-bluetooth/api"
	"github.com/muka/go-bluetooth/bluez"
	"github.com/pkg/errors"
)

// Definition of all UUIDs used by Thingy
// see documentation from nordic:
// https://nordicsemiconductor.github.io/Nordic-Thingy52-FW/documentatiosn
const (
	// TODO: is the CCCD necessary when using bluez dbus?
	// cccdUUID = 0x2902

	// battery level service
	// TODO: currently bluez 5.48+ doesn't work with this properly...
	// batteryServiceUUID = 0x180F
	// batteryLevelUUID   = 0x2A19

	// thingy52 overall configuration
	configurationServiceUUID = 0x0100
	deviceNameUUID           = 0x0101
	advertisingParamUUID     = 0x0102
	connectionParamUUID      = 0x0104
	eddystoneURLUUID         = 0x0105
	cloudTokenUUID           = 0x0106
	firmwareVersionUUID      = 0x0107
	mtuRequestUUID           = 0x0108
	nfcTagContentUUID        = 0x0109

	// environmental service
	environmentServiceUUID         = 0x0200
	environmentTemperatureCharUUID = 0x0201
	environmentPressureCharUUID    = 0x0202
	environmentHumidityCharUUID    = 0x0203
	environmentGasCharUUID         = 0x0204
	environmentColorCharUUID       = 0x0205
	environmentConfigCharUUID      = 0x0206

	// user interface service
	uiServiceUUID         = 0x0300
	uiLEDCharUUID         = 0x0301
	uiButtonCharUUID      = 0x0302
	uiExternalPinCharUUID = 0x0303

	// motion service
	motionServiceUUID            = 0x0400
	motionConfigCharUUID         = 0x0401
	motionTapCharUUID            = 0x0402
	motionOrientationCharUUID    = 0x0403
	motionQuaternionCharUUID     = 0x0404
	motionStepForwardCharUUID    = 0x0405
	motionRawDataCharUUID        = 0x0406
	motionEulerCharUUID          = 0x0407
	motionRotationCharUUID       = 0x0408
	motionHeadingCharUUID        = 0x0409
	motiongGravityVectorCharUUID = 0x040A

	// sound service
	soundServiceUUID           = 0x0500
	soundConfigCharUUID        = 0x0501
	soundSpeakerDataCharUUID   = 0x0502
	soundSpeakerStatusCharUUID = 0x0503
	soundMicrophoneCharUUID    = 0x0504
)

// helper function to turn the numeric UUID specifier into a full 128-bit
// BLE UUID
func nordicUUID(id int64) string {
	return fmt.Sprintf("EF68%04X-9B35-4933-9B10-52FFA9740042", id)
}

// helper function for generic ID
func genericUUID(id int64) string {
	return fmt.Sprintf("0000%04X-0000-1000-8000-00805f9b34fb", id)
}

// RetryFunc will simply run the provided function in a loop checking if the
// function returns an error and retrying it if it does
// if the function fails after numTimes tries, all of the errors are returned
// in the first result and the second result is non-nil
// if the function eventually succeeds before running out of tries, the second
// result will be nil and the first result will contain the previous errors
// Note: if the number of times is specified as 0, this function will loop
// forever
func RetryFunc(numTimes uint, waitTime time.Duration, theFunc func() error) ([]error, error) {
	errRes := make([]error, 0)

	// if numTimes is 0, loop forever
	for try := uint(0); numTimes == 0 || try < numTimes; try++ {
		err := theFunc()
		if err != nil {
			errRes = append(errRes, err)
			if try == numTimes-1 {
				return errRes, fmt.Errorf("function failed after %d tries", numTimes)
			}
		} else {
			break
		}
		time.Sleep(waitTime)
	}

	return errRes, nil
}

// register callback takes a characteristic uuid and a callback function, and
// sets up notifications for the characteristic uuid to call the provided
// cb with the data returned
func registerNotifyCallback(ctx context.Context, errorChanel chan error, dev *api.Device, charuuid string, cb func(data []byte)) error {
	// get the heading data characteristic
	data, err := dev.GetCharByUUID(charuuid)
	if err != nil {
		return errors.Wrap(err, "getting characteristic for motion heading failed")
	}

	// look for the bluez dbus service path for the heading characteristic
	svcs, err := dev.GetAllServicesAndUUID()
	if err != nil {
		return errors.Wrap(err, "error getting services")
	}
	var uuidAndService string
	for _, svcName := range svcs {
		val := strings.Split(svcName, ":")
		if len(val) > 1 && val[0] == charuuid {
			uuidAndService = val[1]
			break
		}
	}
	if uuidAndService == "" {
		return fmt.Errorf("failed to find bluez service path for characteristic for %s", charuuid)
	}

	// start notifications for this characteristic
	if err := data.StartNotify(); err != nil {
		return errors.Wrap(err, "starting notifications failed")
	}

	// TODO: implement checking for the characteristic notifying
	// check to make sure that it is now notifying
	// n, err := data.GetProperty("Notifying")
	// if err != nil {
	// 	return errors.Wrap(err, "getting notifying status failed")
	// }
	// if !n.(bool) {

	// }

	// register a notifier for this characteristic and get a channel for events
	dataChannel, err := data.Register()
	if err != nil {
		return errors.Wrap(err, "registering callback for motion heading characteristic failed")
	}

	// in the background listen for events on the characteristic data channel
	go handleEvents(ctx, dataChannel, errorChanel, uuidAndService, cb)

	return nil
}

func handleEvents(
	ctx context.Context,
	dataChannel chan *dbus.Signal,
	errChannel chan error,
	uuidAndService string,
	cb func(data []byte),
) {
	for {
		select {
		case <-ctx.Done():
			errChannel <- errors.Wrap(
				ctx.Err(),
				"waiting for notify events interrupted",
			)
		case event := <-dataChannel:
			if event == nil {
				return
			}
			if strings.Contains(fmt.Sprint(event.Path), uuidAndService) {
				// make sure the event Body has at least two element
				if len(event.Body) < 2 {
					continue
				}

				// ensure the first body element is not a dbus object path
				if _, ok := event.Body[0].(dbus.ObjectPath); ok {
					continue
				}

				// also make sure the body is not the gatt characteristic str
				if event.Body[0] != bluez.GattCharacteristic1Interface {
					continue
				}

				// make sure the second body of the event is a map to dbus
				// variants
				props, ok := event.Body[1].(map[string]dbus.Variant)
				if !ok {
					continue
				}

				// ensure there is a value key
				val, ok := props["Value"]
				if !ok {
					continue
				}

				// ensure the value is a byte array
				b, ok := val.Value().([]byte)
				if ok {
					// call the user's callback with the value bytes
					cb(b)
				}
			}
		}
	}
}
