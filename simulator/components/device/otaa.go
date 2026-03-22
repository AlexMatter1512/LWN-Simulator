package device

import (
	crand "crypto/rand"
	"encoding/binary"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arslab/lwnsimulator/simulator/util"

	act "github.com/arslab/lwnsimulator/simulator/components/device/activation"
	"github.com/arslab/lwnsimulator/simulator/components/device/classes"
	dl "github.com/arslab/lwnsimulator/simulator/components/device/frames/downlink"
	"github.com/brocaar/lorawan"
)

const (
	JOINACCEPTDELAY1 = time.Duration(5 * time.Second)
	JOINACCEPTDELAY2 = time.Duration(6 * time.Second)
)

var (
	devNonceInitOnce sync.Once
	devNonceCounter  uint32
)

func nextGlobalDevNonce() lorawan.DevNonce {
	devNonceInitOnce.Do(func() {
		var seedBytes [2]byte
		if _, err := crand.Read(seedBytes[:]); err == nil {
			seed := binary.BigEndian.Uint16(seedBytes[:])
			if seed == 0 {
				seed = 1
			}
			// Keep the first generated value equal to seed.
			atomic.StoreUint32(&devNonceCounter, uint32(seed-1))
			return
		}

		// Fallback if OS random is unavailable.
		atomic.StoreUint32(&devNonceCounter, 0)
	})

	for {
		next := atomic.AddUint32(&devNonceCounter, 1)
		nonce := uint16(next & 0xFFFF)
		if nonce != 0 {
			return lorawan.DevNonce(nonce)
		}
	}
}

func (d *Device) OtaaActivation() {
	consecutiveFailures := 0

	for !d.Info.Status.Joined {

		d.Info.Status.Mode = util.Activation

		if !d.CanExecute() { //stop simulator
			return
		}

		d.SwitchClass(classes.ClassA)

		d.SendJoinRequest()

		joinedInWindows, sawAnyDownlink := d.receiveJoinAcceptWindows()
		if !sawAnyDownlink {
			d.Print("None downlink received", nil, util.PrintBoth)
		}
		if joinedInWindows {
			d.Print("Joined", nil, util.PrintBoth)
			d.Info.Status.Mode = util.Normal
			return
		}

		d.Print("Unjoined", nil, util.PrintBoth)
		consecutiveFailures++

		// Backoff before retrying join to avoid synchronized rejoin storms under
		// high-concurrency activation conditions.
		retryBase := 2 * time.Second
		if consecutiveFailures > 1 {
			step := consecutiveFailures - 1
			if step > 3 {
				step = 3
			}
			retryBase = retryBase << step // 2s, 4s, 8s, 16s (capped)
		}
		retryDelay := retryBase + time.Duration(rand.Intn(1500))*time.Millisecond
		timerRetry := time.NewTimer(retryDelay)
		<-timerRetry.C
		timerRetry.Stop()

	}

}

func (d *Device) receiveJoinAcceptWindows() (bool, bool) {
	d.Print("Open RXs", nil, util.PrintBoth)

	type windowCfg struct {
		idx   int
		delay time.Duration
	}

	windows := []windowCfg{
		{idx: 0, delay: JOINACCEPTDELAY1},
		{idx: 1, delay: JOINACCEPTDELAY2},
	}

	sawAnyDownlink := false

	for _, w := range windows {
		rx := &d.Info.RX[w.idx]
		freq := rx.GetListeningFrequency()

		d.Info.Forwarder.Register(freq, d.Info.DevEUI, &d.Info.ReceivedDownlink)

		timerDelay := time.NewTimer(w.delay)
		<-timerDelay.C
		timerDelay.Stop()

		deadline := time.Now().Add(rx.DurationOpen)
		for time.Now().Before(deadline) {
			phy := d.Info.ReceivedDownlink.TryPull()
			if phy == nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}

			sawAnyDownlink = true
			d.Print("Downlink received", nil, util.PrintBoth)

			_, err := d.ProcessDownlink(*phy)
			if err != nil {
				d.Print("", err, util.PrintBoth)
				continue
			}

			if d.Info.Status.Joined {
				d.Info.Forwarder.UnRegister(freq, d.Info.DevEUI)
				return true, sawAnyDownlink
			}
		}

		d.Info.Forwarder.UnRegister(freq, d.Info.DevEUI)
	}

	return false, sawAnyDownlink
}

func (d *Device) CreateJoinRequest() []byte {
	// Allocate a fresh DevNonce from a process-wide sequence so concurrent OTAA
	// attempts do not reuse the same DevNonce across devices.
	d.Info.DevNonce = nextGlobalDevNonce()

	phy := lorawan.PHYPayload{
		MHDR: lorawan.MHDR{
			MType: lorawan.JoinRequest,
			Major: lorawan.LoRaWANR1,
		},
		MACPayload: &lorawan.JoinRequestPayload{
			JoinEUI:  d.Info.JoinEUI, // appEUI
			DevEUI:   d.Info.DevEUI,
			DevNonce: d.Info.DevNonce,
		},
	}

	if err := phy.SetUplinkJoinMIC(d.Info.AppKey); err != nil {

		d.Print("", err, util.PrintBoth)

		return []byte{}
	}

	bytes, err := phy.MarshalBinary()
	if err != nil {

		d.Print("", err, util.PrintBoth)

		return []byte{}
	}

	return bytes

}

func (d *Device) ProcessJoinAccept(JoinAccPayload *lorawan.JoinAcceptPayload) (*dl.InformationDownlink, error) {

	var downlink dl.InformationDownlink
	var err error

	//setkeys
	d.Info.NwkSKey, err = act.GetKey(JoinAccPayload.HomeNetID, JoinAccPayload.JoinNonce, d.Info.DevNonce, d.Info.AppKey, act.PadNwkSKey)
	if err != nil {
		return nil, err
	}

	d.Info.AppSKey, err = act.GetKey(JoinAccPayload.HomeNetID, JoinAccPayload.JoinNonce, d.Info.DevNonce, d.Info.AppKey, act.PadAppSKey)
	if err != nil {
		return nil, err
	}

	d.Info.Status.Joined = true

	//cflist
	if JoinAccPayload.CFList != nil {

		d.Print("Apply CFList", nil, util.PrintBoth)

		cflist, err := JoinAccPayload.CFList.Payload.MarshalBinary()
		if err != nil {
			return nil, err
		}

		if JoinAccPayload.CFList.CFListType == lorawan.CFListChannel { //list of channel

			var CFList lorawan.CFListChannelPayload

			err = CFList.UnmarshalBinary(false, cflist)
			if err != nil {
				return nil, err
			}

			for i, c := range CFList.Channels {
				index := i + d.Info.Configuration.Region.GetNbReservedChannels()
				d.setChannel(uint8(index), c, 0, 5)
			}

		} else { //list of ChMask

			var CFList lorawan.CFListChannelMaskPayload
			err = CFList.UnmarshalBinary(false, cflist)
			if err != nil {
				return nil, err
			}

			for i, mask := range CFList.ChannelMasks {

				for j, enable := range mask {

					index := j + i*16
					d.Info.Configuration.Channels[index].EnableUplink = enable

				}
			}

		}
	}

	d.Info.JoinNonce = JoinAccPayload.JoinNonce
	d.Info.DevAddr = JoinAccPayload.DevAddr
	d.Info.NetID = JoinAccPayload.HomeNetID

	Delay := 1000
	if JoinAccPayload.RXDelay != 0 {
		Delay = Delay * int(JoinAccPayload.RXDelay)
	}

	d.Info.RX[0].Delay = time.Duration(Delay) * time.Millisecond
	d.Info.RX[1].Delay = time.Duration(Delay) * time.Millisecond

	d.Info.Configuration.RX1DROffset = JoinAccPayload.DLSettings.RX1DROffset
	d.Info.RX[1].DataRate = JoinAccPayload.DLSettings.RX2DataRate
	downlink.MType = lorawan.JoinAccept

	return &downlink, nil
}
