package teranode

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/encoding/protowire"
)

// Verdict is a decoded rejected-tx or invalid-block Kafka message. Both teranode messages
// are flat protobufs of strings (util/kafka/kafka_message/kafka_messages.proto):
//
//	KafkaRejectedTxTopicMessage  { 1: txHash, 2: reason, 3: peer_id }
//	KafkaInvalidBlockTopicMessage{ 1: blockHash, 2: reason, 3: peer_id, 4: peer_url }
type Verdict struct {
	Node      string    `json:"node"`
	Kind      string    `json:"kind"` // rejected_tx | invalid_block
	Hash      string    `json:"hash"`
	Reason    string    `json:"reason"`
	PeerID    string    `json:"peerID,omitempty"`
	PeerURL   string    `json:"peerURL,omitempty"`
	Topic     string    `json:"topic"`
	Timestamp time.Time `json:"timestamp"`
}

// VerdictTopic maps a topic name to a node name and kind.
type VerdictTopic struct {
	Topic string
	Node  string
	Kind  string
}

// ConsumeVerdicts tails the given topics from their current end and calls fn for every
// decoded message until ctx is cancelled. It returns once the client is closed.
func ConsumeVerdicts(ctx context.Context, brokers []string, topics []VerdictTopic, lg *slog.Logger, fn func(Verdict)) error {
	if len(topics) == 0 {
		return nil
	}
	byTopic := map[string]VerdictTopic{}
	names := make([]string, 0, len(topics))
	for _, t := range topics {
		byTopic[t.Topic] = t
		names = append(names, t.Topic)
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumeTopics(names...),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
		kgo.AllowAutoTopicCreation(),
	)
	if err != nil {
		return fmt.Errorf("kafka client: %w", err)
	}
	defer cl.Close()
	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			return nil
		}
		fetches.EachError(func(topic string, _ int32, err error) {
			lg.Warn("kafka fetch error", "topic", topic, "err", err)
		})
		fetches.EachRecord(func(r *kgo.Record) {
			t := byTopic[r.Topic]
			v := decodeVerdict(r.Value)
			v.Node, v.Kind, v.Topic, v.Timestamp = t.Node, t.Kind, r.Topic, r.Timestamp
			fn(v)
		})
	}
}

func decodeVerdict(b []byte) Verdict {
	var v Verdict
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		if typ != protowire.BytesType {
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				break
			}
			b = b[m:]
			continue
		}
		s, m := protowire.ConsumeString(b)
		if m < 0 {
			break
		}
		b = b[m:]
		switch num {
		case 1:
			v.Hash = s
		case 2:
			v.Reason = s
		case 3:
			v.PeerID = s
		case 4:
			v.PeerURL = s
		}
	}
	return v
}
