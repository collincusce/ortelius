// (c) 2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package stream

import (
	"context"
	"fmt"

	"github.com/ava-labs/ortelius/services/metrics"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/wrappers"
	"github.com/segmentio/kafka-go"

	"github.com/ava-labs/ortelius/cfg"
	"github.com/ava-labs/ortelius/services"
)

// consumer takes events from Kafka and sends them to a service consumer
type consumerconsensus struct {
	id       string
	chainID  string
	reader   *kafka.Reader
	consumer services.Consumer
	conns    *services.Connections

	// metrics
	metricProcessedCountKey       string
	metricFailureCountKey         string
	metricProcessMillisCounterKey string
	metricSuccessCountKey         string
}

// NewConsumerConsensusFactory returns a processorFactory for the given service consumer
func NewConsumerConsensusFactory(factory serviceConsumerFactory) ProcessorFactory {
	return func(conf cfg.Config, chainVM string, chainID string) (Processor, error) {
		conns, err := services.NewConnectionsFromConfig(conf.Services, false)
		if err != nil {
			return nil, err
		}

		c := &consumerconsensus{
			chainID:                       chainID,
			conns:                         conns,
			metricProcessedCountKey:       fmt.Sprintf("consume_consensus_records_processed_%s", chainID),
			metricProcessMillisCounterKey: fmt.Sprintf("consume_consensus_records_process_millis_%s", chainID),
			metricSuccessCountKey:         fmt.Sprintf("consume_consensus_records_success_%s", chainID),
			metricFailureCountKey:         fmt.Sprintf("consume_consensus_records_failure_%s", chainID),
			id:                            fmt.Sprintf("consumer_consensus %d %s %s", conf.NetworkID, chainVM, chainID),
		}
		metrics.Prometheus.CounterInit(c.metricProcessedCountKey, "records processed")
		metrics.Prometheus.CounterInit(c.metricProcessMillisCounterKey, "records processed millis")
		metrics.Prometheus.CounterInit(c.metricSuccessCountKey, "records success")
		metrics.Prometheus.CounterInit(c.metricFailureCountKey, "records failure")

		initializeConsumerTasker(conns)

		// Create consumer backend
		c.consumer, err = factory(conns, conf.NetworkID, chainVM, chainID)
		if err != nil {
			return nil, err
		}

		// Bootstrap our service
		ctx, cancelFn := context.WithTimeout(context.Background(), consumerInitializeTimeout)
		defer cancelFn()
		if err = c.consumer.Bootstrap(ctx); err != nil {
			return nil, err
		}

		// Setup config
		groupName := conf.Consumer.GroupName
		if groupName == "" {
			groupName = c.consumer.Name()
		}
		if !conf.Consumer.StartTime.IsZero() {
			groupName = ""
		}

		// Create reader for the topic
		c.reader = kafka.NewReader(kafka.ReaderConfig{
			Topic:       GetTopicName(conf.NetworkID, chainID, EventTypeConsensus),
			Brokers:     conf.Kafka.Brokers,
			GroupID:     groupName,
			StartOffset: kafka.FirstOffset,
			MaxBytes:    ConsumerMaxBytesDefault,
		})

		// If the start time is set then seek to the correct offset
		if !conf.Consumer.StartTime.IsZero() {
			ctx, cancelFn := context.WithTimeout(context.Background(), kafkaReadTimeout)
			defer cancelFn()

			if err = c.reader.SetOffsetAt(ctx, conf.Consumer.StartTime); err != nil {
				return nil, err
			}
		}

		return c, nil
	}
}

func (c *consumerconsensus) ID() string {
	return c.id
}

// Close closes the consumer
func (c *consumerconsensus) Close() error {
	errs := wrappers.Errs{}
	errs.Add(c.conns.Close(), c.reader.Close())
	return errs.Err
}

// ProcessNextMessage waits for a new Message and adds it to the services
func (c *consumerconsensus) ProcessNextMessage() error {
	msg, err := c.nextMessage()
	if err != nil {
		if err != context.DeadlineExceeded {
			c.conns.Logger().Error("consumer.getNextMessage: %s", err.Error())
		}
		return err
	}

	collectors := metrics.NewCollectors(
		metrics.NewCounterIncCollect(c.metricProcessedCountKey),
		metrics.NewCounterObserveMillisCollect(c.metricProcessMillisCounterKey),
	)
	defer func() {
		err := collectors.Collect()
		if err != nil {
			c.conns.Logger().Error("collectors.Collect: %s", err)
		}
	}()

	if err = c.consumer.ConsumeConsensus(msg); err != nil {
		collectors.Error()
		c.conns.Logger().Error("consumer.Consume: %s", err)
		return err
	}
	return nil
}

func (c *consumerconsensus) nextMessage() (*Message, error) {
	ctx, cancelFn := context.WithTimeout(context.Background(), kafkaReadTimeout)
	defer cancelFn()

	return c.getNextMessage(ctx)
}

func (c *consumerconsensus) Failure() {
	err := metrics.Prometheus.CounterInc(c.metricFailureCountKey)
	if err != nil {
		c.conns.Logger().Error("prmetheus.CounterInc: %s", err)
	}
}

func (c *consumerconsensus) Success() {
	err := metrics.Prometheus.CounterInc(c.metricSuccessCountKey)
	if err != nil {
		c.conns.Logger().Error("prmetheus.CounterInc: %s", err)
	}
}

// getNextMessage gets the next Message from the Kafka Indexer
func (c *consumerconsensus) getNextMessage(ctx context.Context) (*Message, error) {
	// Get raw Message from Kafka
	msg, err := c.reader.ReadMessage(ctx)
	if err != nil {
		return nil, err
	}

	// Extract Message ID from key
	id, err := ids.ToID(msg.Key)
	if err != nil {
		return nil, err
	}

	return &Message{
		chainID:   c.chainID,
		body:      msg.Value,
		id:        id.String(),
		timestamp: msg.Time.UTC().Unix(),
	}, nil
}
