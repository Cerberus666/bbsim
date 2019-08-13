package devices

import (
	"context"
	"errors"
	"fmt"
	"gerrit.opencord.org/bbsim/api/openolt"
	"github.com/looplab/fsm"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"net"
	"os"
	"sync"
)

var oltLogger = log.WithFields(log.Fields{
	"module": "OLT",
})

func init() {
	//log.SetReportCaller(true)
	log.SetLevel(log.DebugLevel)
}

var olt = OltDevice{}

func GetOLT() OltDevice  {
	return olt
}

func CreateOLT(seq int, nni int, pon int, onuPerPon int) OltDevice {
	oltLogger.WithFields(log.Fields{
		"ID": seq,
		"NumNni":nni,
		"NumPon":pon,
		"NumOnuPerPon":onuPerPon,
	}).Debug("CreateOLT")

	olt = OltDevice{
		ID: seq,
		OperState: getOperStateFSM(func(e *fsm.Event) {
			oltLogger.Debugf("Changing OLT OperState from %s to %s", e.Src, e.Dst)
		}),
		NumNni:nni,
		NumPon:pon,
		NumOnuPerPon:onuPerPon,
		Pons: []PonPort{},
		Nnis: []NniPort{},
		channel: make(chan Message),
	}

	// OLT State machine
	// NOTE do we need 2 state machines for the OLT? (InternalState and OperState)
	olt.InternalState = fsm.NewFSM(
		"created",
		fsm.Events{
			{Name: "enable", Src: []string{"created"}, Dst: "enabled"},
			{Name: "disable", Src: []string{"enabled"}, Dst: "disabled"},
		},
		fsm.Callbacks{
			"enter_state": func(e *fsm.Event) {
				oltLogger.Debugf("Changing OLT InternalState from %s to %s", e.Src, e.Dst)
			},
		},
	)

	// create NNI Port
	nniPort := NniPort{
		ID: uint32(0),
		OperState: getOperStateFSM(func(e *fsm.Event) {
			oltLogger.Debugf("Changing NNI OperState from %s to %s", e.Src, e.Dst)
		}),
		Type: "nni",
	}
	olt.Nnis = append(olt.Nnis, nniPort)

	// create PON ports
	for i := 0; i < pon; i++ {
		p := PonPort{
			NumOnu: olt.NumOnuPerPon,
			ID: uint32(i),
			Type: "pon",
		}
		p.OperState = getOperStateFSM(func(e *fsm.Event) {
			oltLogger.WithFields(log.Fields{
				"ID": p.ID,
			}).Debugf("Changing PON Port OperState from %s to %s", e.Src, e.Dst)
		})

		// create ONU devices
		for j := 0; j < onuPerPon; j++ {
			o := CreateONU(olt, p, uint32(j + 1))
			p.Onus = append(p.Onus, o)
		}

		olt.Pons = append(olt.Pons, p)
	}

	wg := sync.WaitGroup{}

	wg.Add(1)
	go newOltServer(olt)
	wg.Wait()
	return olt
}

func newOltServer(o OltDevice) error {
	// TODO make configurable
	address :=  "0.0.0.0:50060"
	lis, err := net.Listen("tcp", address)
	if err != nil {
		oltLogger.Fatalf("OLT failed to listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	openolt.RegisterOpenoltServer(grpcServer, o)

	go grpcServer.Serve(lis)
	oltLogger.Debugf("OLT Listening on: %v", address)

	return nil
}

// Device Methods

func (o OltDevice) Enable (stream openolt.Openolt_EnableIndicationServer) error {

	oltLogger.Debug("Enable OLT called")

	wg := sync.WaitGroup{}
	wg.Add(1)

	// create a channel for all the OLT events
	go o.processOltMessages(stream)

	// enable the OLT
	olt_msg := Message{
		Type: OltIndication,
		Data: OltIndicationMessage{
			OperState: UP,
		},
	}
	o.channel <- olt_msg

	// send NNI Port Indications
	for _, nni := range o.Nnis {
		msg := Message{
			Type: NniIndication,
			Data: NniIndicationMessage{
				OperState: UP,
				NniPortID: nni.ID,
			},
		}
		o.channel <- msg
	}

	// send PON Port indications
	for _, pon := range o.Pons {
		msg := Message{
			Type: PonIndication,
			Data: PonIndicationMessage{
				OperState: UP,
				PonPortID: pon.ID,
			},
		}
		o.channel <- msg

		for _, onu := range pon.Onus {
			go onu.processOnuMessages(stream)
			msg := Message{
				Type:      OnuDiscIndication,
				Data: OnuDiscIndicationMessage{
					Onu:     onu,
					OperState: UP,
				},
			}
			onu.channel <- msg
		}
	}

	wg.Wait()
	return nil
}

// Helpers method

func (o OltDevice) getPonById(id uint32) (*PonPort, error) {
	for _, pon := range o.Pons {
		if pon.ID == id {
			return &pon, nil
		}
	}
	return nil, errors.New(fmt.Sprintf("Cannot find PonPort with id %d in OLT %d", id, o.ID))
}

func (o OltDevice) getNniById(id uint32) (*NniPort, error) {
	for _, nni := range o.Nnis {
		if nni.ID == id {
			return &nni, nil
		}
	}
	return nil, errors.New(fmt.Sprintf("Cannot find NniPort with id %d in OLT %d", id, o.ID))
}

func (o OltDevice) sendOltIndication(msg OltIndicationMessage, stream openolt.Openolt_EnableIndicationServer) {
	data := &openolt.Indication_OltInd{OltInd: &openolt.OltIndication{OperState: msg.OperState.String()}}
	if err := stream.Send(&openolt.Indication{Data: data}); err != nil {
		oltLogger.Error("Failed to send Indication_OltInd: %v", err)
	}

	oltLogger.WithFields(log.Fields{
		"OperState": msg.OperState,
	}).Debug("Sent Indication_OltInd")
}

func (o OltDevice) sendNniIndication(msg NniIndicationMessage, stream openolt.Openolt_EnableIndicationServer) {
	nni, _ := o.getNniById(msg.NniPortID)
	nni.OperState.Event("enable")
	// NOTE Operstate may need to be an integer
	operData := &openolt.Indication_IntfOperInd{IntfOperInd: &openolt.IntfOperIndication{
		Type: nni.Type,
		IntfId: nni.ID,
		OperState: nni.OperState.Current(),
	}}

	if err := stream.Send(&openolt.Indication{Data: operData}); err != nil {
		oltLogger.Error("Failed to send Indication_IntfOperInd for NNI: %v", err)
	}

	oltLogger.WithFields(log.Fields{
		"Type": nni.Type,
		"IntfId": nni.ID,
		"OperState": nni.OperState.Current(),
	}).Debug("Sent Indication_IntfOperInd for NNI")
}

func (o OltDevice) sendPonIndication(msg PonIndicationMessage, stream openolt.Openolt_EnableIndicationServer) {
	pon, _ := o.getPonById(msg.PonPortID)
	pon.OperState.Event("enable")
	discoverData := &openolt.Indication_IntfInd{IntfInd: &openolt.IntfIndication{
		IntfId: pon.ID,
		OperState: pon.OperState.Current(),
	}}

	if err := stream.Send(&openolt.Indication{Data: discoverData}); err != nil {
		oltLogger.Error("Failed to send Indication_IntfInd: %v", err)
	}

	oltLogger.WithFields(log.Fields{
		"IntfId": pon.ID,
		"OperState": pon.OperState.Current(),
	}).Debug("Sent Indication_IntfInd")

	operData := &openolt.Indication_IntfOperInd{IntfOperInd: &openolt.IntfOperIndication{
		Type: pon.Type,
		IntfId: pon.ID,
		OperState: pon.OperState.Current(),
	}}

	if err := stream.Send(&openolt.Indication{Data: operData}); err != nil {
		oltLogger.Error("Failed to send Indication_IntfOperInd for PON: %v", err)
	}

	oltLogger.WithFields(log.Fields{
		"Type": pon.Type,
		"IntfId": pon.ID,
		"OperState": pon.OperState.Current(),
	}).Debug("Sent Indication_IntfOperInd for PON")
}

func (o OltDevice) processOltMessages(stream openolt.Openolt_EnableIndicationServer) {
	oltLogger.Debug("Started OLT Indication Channel")
	for message := range o.channel {


		oltLogger.WithFields(log.Fields{
			"oltId": o.ID,
			"messageType": message.Type,
		}).Trace("Received message")

		switch message.Type {
		case OltIndication:
			msg, _ := message.Data.(OltIndicationMessage)
			if msg.OperState == UP {
				o.InternalState.Event("enable")
				o.OperState.Event("enable")
			} else if msg.OperState == DOWN {
				o.InternalState.Event("disable")
				o.OperState.Event("disable")
			}
			o.sendOltIndication(msg, stream)
		case NniIndication:
			msg, _ := message.Data.(NniIndicationMessage)
			o.sendNniIndication(msg, stream)
		case PonIndication:
			msg, _ := message.Data.(PonIndicationMessage)
			o.sendPonIndication(msg, stream)
		default:
			oltLogger.Warnf("Received unknown message data %v for type %v in OLT channel", message.Data, message.Type)
		}

	}
}

// GRPC Endpoints

func (o OltDevice) ActivateOnu(context context.Context, onu *openolt.Onu) (*openolt.Empty, error)  {
	oltLogger.WithFields(log.Fields{
		"onuSerialNumber": onu.SerialNumber,
	}).Info("Received ActivateOnu call from VOLTHA")

	pon, _ := o.getPonById(onu.IntfId)
	_onu, _ := pon.getOnuBySn(onu.SerialNumber)

	// NOTE we need to immediately activate the ONU or the OMCI state machine won't start
	msg := Message{
		Type:      OnuIndication,
		Data:      OnuIndicationMessage{
			OnuSN:     onu.SerialNumber,
			PonPortID: onu.IntfId,
			OperState: UP,
		},
	}
	_onu.channel <- msg
	return new(openolt.Empty) , nil
}

func (o OltDevice) DeactivateOnu(context.Context, *openolt.Onu) (*openolt.Empty, error)  {
	oltLogger.Error("DeactivateOnu not implemented")
	return new(openolt.Empty) , nil
}

func (o OltDevice) DeleteOnu(context.Context, *openolt.Onu) (*openolt.Empty, error)  {
	oltLogger.Error("DeleteOnu not implemented")
	return new(openolt.Empty) , nil
}

func (o OltDevice) DisableOlt(context.Context, *openolt.Empty) (*openolt.Empty, error)  {
	// NOTE when we disable the OLT should we disable NNI, PONs and ONUs altogether?
	olt_msg := Message{
		Type: OltIndication,
		Data: OltIndicationMessage{
			OperState: DOWN,
		},
	}
	o.channel <- olt_msg
	return new(openolt.Empty) , nil
}

func (o OltDevice) DisablePonIf(context.Context, *openolt.Interface) (*openolt.Empty, error)  {
	oltLogger.Error("DisablePonIf not implemented")
	return new(openolt.Empty) , nil
}

func (o OltDevice) EnableIndication(_ *openolt.Empty, stream openolt.Openolt_EnableIndicationServer) error  {
	oltLogger.WithField("oltId", o.ID).Info("OLT receives EnableIndication call from VOLTHA")
	o.Enable(stream)
	return nil
}

func (o OltDevice) EnablePonIf(context.Context, *openolt.Interface) (*openolt.Empty, error)  {
	oltLogger.Error("EnablePonIf not implemented")
	return new(openolt.Empty) , nil
}

func (o OltDevice) FlowAdd(context.Context, *openolt.Flow) (*openolt.Empty, error)  {
	oltLogger.Error("FlowAdd not implemented")
	return new(openolt.Empty) , nil
}

func (o OltDevice) FlowRemove(context.Context, *openolt.Flow) (*openolt.Empty, error)  {
	oltLogger.Error("FlowRemove not implemented")
	return new(openolt.Empty) , nil
}

func (o OltDevice) HeartbeatCheck(context.Context, *openolt.Empty) (*openolt.Heartbeat, error)  {
	oltLogger.Error("HeartbeatCheck not implemented")
	return new(openolt.Heartbeat) , nil
}

func (o OltDevice) GetDeviceInfo(context.Context, *openolt.Empty) (*openolt.DeviceInfo, error)  {

	oltLogger.WithField("oltId", o.ID).Info("OLT receives GetDeviceInfo call from VOLTHA")
	devinfo := new(openolt.DeviceInfo)
	devinfo.Vendor = "BBSim"
	devinfo.Model = "asfvolt16"
	devinfo.HardwareVersion = "emulated"
	devinfo.FirmwareVersion = ""
	devinfo.Technology = "xgspon"
	devinfo.PonPorts = 1
	devinfo.OnuIdStart = 1
	devinfo.OnuIdEnd = 255
	devinfo.AllocIdStart = 1024
	devinfo.AllocIdEnd = 16383
	devinfo.GemportIdStart = 1024
	devinfo.GemportIdEnd = 65535
	devinfo.FlowIdStart = 1
	devinfo.FlowIdEnd = 16383
	devinfo.DeviceSerialNumber = fmt.Sprintf("BBSIM_OLT_%d", o.ID)

	return devinfo, nil
}

func (o OltDevice) OmciMsgOut(ctx context.Context, omci_msg *openolt.OmciMsg) (*openolt.Empty, error)  {
	oltLogger.Debugf("Recevied OmciMsgOut - IntfId: %d OnuId: %d", omci_msg.IntfId, omci_msg.OnuId)
	pon, _ := o.getPonById(omci_msg.IntfId)
	onu, _ := pon.getOnuById(omci_msg.OnuId)
	msg := Message{
		Type:      OMCI,
		Data:      OmciMessage{
			OnuSN:  onu.SerialNumber,
			OnuId: 	onu.ID,
			msg: 	omci_msg,
		},
	}
	onu.channel <- msg
	return new(openolt.Empty) , nil
}

func (o OltDevice) OnuPacketOut(context.Context, *openolt.OnuPacket) (*openolt.Empty, error)  {
	oltLogger.Error("OnuPacketOut not implemented")
	return new(openolt.Empty) , nil
}

func (o OltDevice) Reboot(context.Context, *openolt.Empty) (*openolt.Empty, error)  {
	oltLogger.Info("Shutting Down, hope you're running in K8s...")
	os.Exit(0)
	return new(openolt.Empty) , nil
}

func (o OltDevice) ReenableOlt(context.Context, *openolt.Empty) (*openolt.Empty, error) {
	oltLogger.Error("ReenableOlt not implemented")
	return new(openolt.Empty) , nil
}

func (o OltDevice) UplinkPacketOut(context context.Context, packet *openolt.UplinkPacket) (*openolt.Empty, error) {
	oltLogger.Error("UplinkPacketOut not implemented")
	return new(openolt.Empty) , nil
}

func (o OltDevice) CollectStatistics(context.Context, *openolt.Empty) (*openolt.Empty, error)  {
	oltLogger.Error("CollectStatistics not implemented")
	return new(openolt.Empty) , nil
}

func (o OltDevice) CreateTconts(context context.Context, packet *openolt.Tconts) (*openolt.Empty, error) {
	oltLogger.Error("CreateTconts not implemented")
	return new(openolt.Empty) , nil
}

func (o OltDevice) RemoveTconts(context context.Context, packet *openolt.Tconts) (*openolt.Empty, error) {
	oltLogger.Error("RemoveTconts not implemented")
	return new(openolt.Empty) , nil
}

func (o OltDevice) GetOnuInfo(context context.Context, packet *openolt.Onu) (*openolt.OnuIndication, error) {
	oltLogger.Error("GetOnuInfo not implemented")
	return new(openolt.OnuIndication) , nil
}

func (o OltDevice) GetPonIf(context context.Context, packet *openolt.Interface) (*openolt.IntfIndication, error) {
	oltLogger.Error("GetPonIf not implemented")
	return new(openolt.IntfIndication) , nil
}