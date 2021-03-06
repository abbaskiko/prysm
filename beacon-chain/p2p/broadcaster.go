package p2p

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/gogo/protobuf/proto"
	"github.com/pkg/errors"
	eth "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/shared/hashutil"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/traceutil"
	"go.opencensus.io/trace"
)

// ErrMessageNotMapped occurs on a Broadcast attempt when a message has not been defined in the
// GossipTypeMapping.
var ErrMessageNotMapped = errors.New("message type is not mapped to a PubSub topic")

// Max number of attempts to search the network for a specific subnet.
const maxSubnetDiscoveryAttempts = 3

// Broadcast a message to the p2p network.
func (s *Service) Broadcast(ctx context.Context, msg proto.Message) error {
	ctx, span := trace.StartSpan(ctx, "p2p.Broadcast")
	defer span.End()

	twoSlots := time.Duration(2*params.BeaconConfig().SecondsPerSlot) * time.Second
	ctx, cancel := context.WithTimeout(ctx, twoSlots)
	defer cancel()

	forkDigest, err := s.forkDigest()
	if err != nil {
		err := errors.Wrap(err, "could not retrieve fork digest")
		traceutil.AnnotateError(span, err)
		return err
	}

	topic, ok := GossipTypeMapping[reflect.TypeOf(msg)]
	if !ok {
		traceutil.AnnotateError(span, ErrMessageNotMapped)
		return ErrMessageNotMapped
	}
	return s.broadcastObject(ctx, msg, fmt.Sprintf(topic, forkDigest))
}

// BroadcastAttestation broadcasts an attestation to the p2p network.
func (s *Service) BroadcastAttestation(ctx context.Context, subnet uint64, att *eth.Attestation) error {
	_, span := trace.StartSpan(ctx, "p2p.BroadcastAttestation")
	defer span.End()
	forkDigest, err := s.forkDigest()
	if err != nil {
		err := errors.Wrap(err, "could not retrieve fork digest")
		traceutil.AnnotateError(span, err)
		return err
	}

	// Non-blocking broadcast, with attempts to discover a subnet peer if none available.
	go s.broadcastAttestation(ctx, subnet, att, forkDigest)

	return nil
}

func (s *Service) broadcastAttestation(ctx context.Context, subnet uint64, att *eth.Attestation, forkDigest [4]byte) {
	requestKey := ctx.Value("x-request-key")
	ctx, span := trace.StartSpan(context.Background(), "p2p.broadcastAttestation")
	defer span.End()
	ctx = trace.NewContext(context.Background(), span) // clear parent context / deadline.

	oneEpoch := time.Duration(1*params.BeaconConfig().SlotsPerEpoch*params.BeaconConfig().SecondsPerSlot) * time.Second
	ctx, cancel := context.WithTimeout(ctx, oneEpoch)
	defer cancel()

	// Ensure we have peers with this subnet.
	s.subnetLocker(subnet).RLock()
	hasPeer := s.hasPeerWithSubnet(subnet)
	s.subnetLocker(subnet).RUnlock()

	span.AddAttributes(
		trace.BoolAttribute("hasPeer", hasPeer),
		trace.Int64Attribute("slot", int64(att.Data.Slot)),
		trace.Int64Attribute("subnet", int64(subnet)),
	)

	log := log.WithFields(logrus.Fields{
		"slot":        att.Data.Slot,
		"subnet":      subnet,
		"forkDigest":  forkDigest,
		"request_key": requestKey,
	})
	log.Info("--------- broadcastAttestation START --------------")

	if !hasPeer {
		log.Info("--------------- INITIAL PEER NOT FOUND ----------------")
		attestationBroadcastAttempts.Inc()
		if err := func() error {
			s.subnetLocker(subnet).Lock()
			defer s.subnetLocker(subnet).Unlock()
			for i := 0; i < maxSubnetDiscoveryAttempts; i++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				ok, err := s.FindPeersWithSubnet(ctx, subnet)
				if err != nil {
					log.WithError(err).Error("------- FAILED TO FIND PEERS WITH SUBNET ---------------")
					return err
				}
				if ok {
					log.WithFields(logrus.Fields{
						"subnet":  subnet,
						"attempt": i,
					}).Error("--------- PEER FOUND! ---------------")
					savedAttestationBroadcasts.Inc()
					return nil
				} else {
					log.WithFields(logrus.Fields{
						"subnet":  subnet,
						"attempt": i,
					}).Error("--------- SUBNET PEER NOT FOUND! ---------------")
				}
			}
			return errors.New("failed to find peers for subnet")
		}(); err != nil {
			log.WithError(err).Error("Failed to find peers")
			traceutil.AnnotateError(span, err)
		}
	}

	log.WithField("topic", attestationToTopic(subnet, forkDigest)).Info("---------- BROADCAST TOPIC --------------")
	if err := s.broadcastObject(context.WithValue(ctx, "x-request-key", requestKey), att, attestationToTopic(subnet, forkDigest)); err != nil {
		log.WithError(err).Error("------------ FAILED TO BROADCAST ATTESTATION --------------")
		traceutil.AnnotateError(span, err)
	}
	log.Info("------------ BROADCASTED ATTESTATION!!! --------------")
}

// method to broadcast messages to other peers in our gossip mesh.
func (s *Service) broadcastObject(ctx context.Context, obj interface{}, topic string) error {
	_, span := trace.StartSpan(ctx, "p2p.broadcastObject")
	defer span.End()

	span.AddAttributes(trace.StringAttribute("topic", topic))

	buf := new(bytes.Buffer)
	if _, err := s.Encoding().EncodeGossip(buf, obj); err != nil {
		err := errors.Wrap(err, "could not encode message")
		traceutil.AnnotateError(span, err)
		return err
	}

	if span.IsRecordingEvents() {
		id := hashutil.FastSum64(buf.Bytes())
		messageLen := int64(buf.Len())
		span.AddMessageSendEvent(int64(id), messageLen /*uncompressed*/, messageLen /*compressed*/)
	}

	if err := s.PublishToTopic(ctx, topic+s.Encoding().ProtocolSuffix(), buf.Bytes()); err != nil {
		err := errors.Wrap(err, "could not publish message")
		traceutil.AnnotateError(span, err)
		return err
	}
	return nil
}

func attestationToTopic(subnet uint64, forkDigest [4]byte) string {
	return fmt.Sprintf(AttestationSubnetTopicFormat, forkDigest, subnet)
}
