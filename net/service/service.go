package service

import (
	"context"
	"errors"
	host "gx/ipfs/QmPsRtodRuBUir32nz5v4zuSBTSszrR1d3fA6Ahb6eaejj/go-libp2p-host"
	inet "gx/ipfs/QmQx1dHDDYENugYgqA22BaBrRfuv1coSsuPiM7rYh1wwGH/go-libp2p-net"
	ggio "gx/ipfs/QmZ4Qi3GaRbjcx28Sme5eMH7RQjGkt8wHxt2a65oLaeFEV/gogo-protobuf/io"
	protocol "gx/ipfs/QmZNkThpqfVXs9GNbexPrfBbXSLNYeKrE7jwFM2oqHbyqN/go-libp2p-protocol"
	ps "gx/ipfs/QmeXj9VAjmYQZxpmVz7VzccbJrpmr8qkCDSjfVNsPTWTYU/go-libp2p-peerstore"
	peer "gx/ipfs/QmfMmLGoKzCHDN7cGgk64PJr4iipzidDRME8HABSJqvmhC/go-libp2p-peer"
	"sync"

	"github.com/OpenBazaar/openbazaar-go/core"
	"github.com/OpenBazaar/openbazaar-go/pb"
	"github.com/OpenBazaar/openbazaar-go/repo"
	"github.com/ipfs/go-ipfs/commands"
	ctxio "github.com/jbenet/go-context/io"
	"github.com/op/go-logging"
)

var log = logging.MustGetLogger("service")

var ProtocolOpenBazaar protocol.ID = "/openbazaar/app/1.0.0"

type OpenBazaarService struct {
	host      host.Host
	self      peer.ID
	peerstore ps.Peerstore
	cmdCtx    commands.Context
	ctx       context.Context
	broadcast chan interface{}
	datastore repo.Datastore
	node      *core.OpenBazaarNode
	sender    map[peer.ID]*messageSender
	senderlk  sync.Mutex
}

func New(node *core.OpenBazaarNode, ctx commands.Context, datastore repo.Datastore) *OpenBazaarService {
	service := &OpenBazaarService{
		host:      node.IpfsNode.PeerHost.(host.Host),
		self:      node.IpfsNode.Identity,
		peerstore: node.IpfsNode.PeerHost.Peerstore(),
		cmdCtx:    ctx,
		ctx:       node.IpfsNode.Context(),
		broadcast: node.Broadcast,
		datastore: datastore,
		node:      node,
		sender:    make(map[peer.ID]*messageSender),
	}
	node.IpfsNode.PeerHost.SetStreamHandler(ProtocolOpenBazaar, service.HandleNewStream)
	log.Infof("OpenBazaar service running at %s", ProtocolOpenBazaar)
	return service
}

func (service *OpenBazaarService) HandleNewStream(s inet.Stream) {
	go service.handleNewMessage(s)
}

func (service *OpenBazaarService) handleNewMessage(s inet.Stream) {
	cr := ctxio.NewReader(service.ctx, s)
	cw := ctxio.NewWriter(service.ctx, s)
	r := ggio.NewDelimitedReader(cr, inet.MessageSizeMax)
	w := ggio.NewDelimitedWriter(cw)
	mPeer := s.Conn().RemotePeer()
	// Check if banned
	if service.node.BanManager.IsBanned(mPeer) {
		return
	}

	// ensure the message sender for this peer is updated with this stream, so we reply over it
	service.messageSenderForPeer(mPeer, &s)
	defer s.Close()
	i := 0 // REMOVE
	for {
		log.Info("inbound stream reuse count:", i)
		i++
		// Receive msg
		pmes := new(pb.Message)
		if err := r.ReadMsg(pmes); err != nil {
			log.Errorf("Error unmarshaling data: %s", err)
			return
		}

		// Get handler for this msg type
		handler := service.HandlerForMsgType(pmes.MessageType)
		if handler == nil {
			log.Debug("Got back nil handler from handlerForMsgType")
			return
		}

		// Dispatch handler
		rpmes, err := handler(mPeer, pmes, nil)
		if err != nil {
			log.Debugf("handle message error: %s", err)
			return
		}

		// If nil response, return it before serializing
		if rpmes == nil {
			continue
		}

		// Send out response msg
		if err := w.WriteMsg(rpmes); err != nil {
			log.Debugf("send response error: %s", err)
			return
		}
	}
}

func (service *OpenBazaarService) SendRequest(ctx context.Context, p peer.ID, pmes *pb.Message) (*pb.Message, error) {
	log.Debugf("Sending %s request to %s", pmes.MessageType.String(), p.Pretty())
	ms := service.messageSenderForPeer(p, nil)

	rpmes, err := ms.SendRequest(ctx, pmes)
	if err != nil {
		log.Debugf("No response from %s", p.Pretty())
		return nil, err
	}

	if rpmes == nil {
		log.Debugf("No response from %s", p.Pretty())
		return nil, errors.New("no response from peer")
	}

	log.Debugf("Received response from %s", p.Pretty())

	return rpmes, nil
}

func (service *OpenBazaarService) SendMessage(ctx context.Context, p peer.ID, pmes *pb.Message) error {
	log.Debugf("Sending %s message to %s", pmes.MessageType.String(), p.Pretty())
	ms := service.messageSenderForPeer(p, nil)

	if err := ms.SendMessage(ctx, pmes); err != nil {
		return err
	}
	return nil
}
