package kafka

/**
 * Copyright 2016 Confluent Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import (
	"fmt"
	"unsafe"
)

/*
#include <stdlib.h>
#include <librdkafka/rdkafka.h>
*/
import "C"

// RebalanceCb provides a per-Subscribe*() rebalance event callback.
// The passed Event will be either AssignedPartitions or RevokedPartitions
type RebalanceCb func(*Consumer, Event) error

// Consumer implements a High-level Apache Kafka Consumer instance
type Consumer struct {
	Events             chan Event
	handle             handle
	eventsChanEnable   bool
	readerTermChan     chan bool
	rebalanceCb        RebalanceCb
	appReassigned      bool
	appRebalanceEnable bool // config setting
}

// Strings returns a human readable name for a Consumer instance
func (c *Consumer) String() string {
	return c.handle.String()
}

// getHandle implements the Handle interface
func (c *Consumer) gethandle() *handle {
	return &c.handle
}

// Subscribe to a single topic
// This replaces the current subscription
func (c *Consumer) Subscribe(topic string, rebalanceCb RebalanceCb) error {
	return c.SubscribeTopics([]string{topic}, rebalanceCb)
}

// SubscribeTopics subscribes to the provided list of topics.
// This replaces the current subscription.
func (c *Consumer) SubscribeTopics(topics []string, rebalanceCb RebalanceCb) (err error) {
	ctopics := C.rd_kafka_topic_partition_list_new(C.int(len(topics)))
	defer C.rd_kafka_topic_partition_list_destroy(ctopics)

	for _, topic := range topics {
		ctopic := C.CString(topic)
		defer C.free(unsafe.Pointer(ctopic))
		C.rd_kafka_topic_partition_list_add(ctopics, ctopic, C.RD_KAFKA_PARTITION_UA)
	}

	e := C.rd_kafka_subscribe(c.handle.rk, ctopics)
	if e != C.RD_KAFKA_RESP_ERR_NO_ERROR {
		return newError(e)
	}

	c.rebalanceCb = rebalanceCb
	c.handle.currAppRebalanceEnable = c.rebalanceCb != nil || c.appRebalanceEnable

	return nil
}

// Unsubscribe from the current subscription, if any.
func (c *Consumer) Unsubscribe() (err error) {
	C.rd_kafka_unsubscribe(c.handle.rk)
	return nil
}

// Assign an atomic set of partitions to consume.
// This replaces the current assignment.
func (c *Consumer) Assign(partitions []TopicPartition) (err error) {
	c.appReassigned = true

	cparts := newCPartsFromTopicPartitions(partitions)
	defer C.rd_kafka_topic_partition_list_destroy(cparts)

	e := C.rd_kafka_assign(c.handle.rk, cparts)
	if e != C.RD_KAFKA_RESP_ERR_NO_ERROR {
		return newError(e)
	}

	return nil
}

// Unassign the current set of partitions to consume.
func (c *Consumer) Unassign() (err error) {
	c.appReassigned = true

	e := C.rd_kafka_assign(c.handle.rk, nil)
	if e != C.RD_KAFKA_RESP_ERR_NO_ERROR {
		return newError(e)
	}

	return nil
}

// commit offsets for specified offsets.
// If offsets is nil the currently assigned partitions' offsets are committed.
// This is a blocking call, caller will need to wrap in go-routine to
// get async or throw-away behaviour.
func (c *Consumer) commit(offsets []TopicPartition) (committedOffsets []TopicPartition, err error) {
	var rkqu *C.rd_kafka_queue_t

	rkqu = C.rd_kafka_queue_new(c.handle.rk)
	defer C.rd_kafka_queue_destroy(rkqu)

	var coffsets *C.rd_kafka_topic_partition_list_t
	if offsets != nil {
		coffsets = newCPartsFromTopicPartitions(offsets)
		defer C.rd_kafka_topic_partition_list_destroy(coffsets)
	}

	cErr := C.rd_kafka_commit_queue(c.handle.rk, coffsets, rkqu, nil, nil)
	if cErr != C.RD_KAFKA_RESP_ERR_NO_ERROR {
		return nil, newError(cErr)
	}

	rkev := C.rd_kafka_queue_poll(rkqu, C.int(-1))
	if rkev == nil {
		// shouldn't happen
		return nil, newError(C.RD_KAFKA_RESP_ERR__DESTROY)
	}
	defer C.rd_kafka_event_destroy(rkev)

	if C.rd_kafka_event_type(rkev) != C.RD_KAFKA_EVENT_OFFSET_COMMIT {
		panic(fmt.Sprintf("Expected OFFSET_COMMIT, got %s",
			C.GoString(C.rd_kafka_event_name(rkev))))
	}

	cErr = C.rd_kafka_event_error(rkev)
	if cErr != C.RD_KAFKA_RESP_ERR_NO_ERROR {
		return nil, newErrorFromCString(cErr, C.rd_kafka_event_error_string(rkev))
	}

	cRetoffsets := C.rd_kafka_event_topic_partition_list(rkev)
	if cRetoffsets == nil {
		// no offsets, no error
		return nil, nil
	}
	committedOffsets = newTopicPartitionsFromCparts(cRetoffsets)

	return committedOffsets, nil
}

// Commit offsets for currently assigned partitions
// This is a blocking call.
// Returns the committed offsets on success.
func (c *Consumer) Commit() ([]TopicPartition, error) {
	return c.commit(nil)
}

// CommitMessage commits offset based on the provided message.
// This is a blocking call.
// Returns the committed offsets on success.
func (c *Consumer) CommitMessage(m *Message) ([]TopicPartition, error) {
	if m.TopicPartition.Error != nil {
		return nil, Error{ErrInvalidArg, "Can't commit errored message"}
	}
	offsets := []TopicPartition{m.TopicPartition}
	offsets[0].Offset++
	return c.commit(offsets)
}

// CommitOffsets commits the provided list of offsets
// This is a blocking call.
// Returns the committed offsets on success.
func (c *Consumer) CommitOffsets(offsets []TopicPartition) ([]TopicPartition, error) {
	return c.commit(offsets)
}

// Poll the consumer for messages or events.
//
// Will block for at most timeoutMs milliseconds
//
// The following callbacks may be triggered:
//   Subscribe()'s rebalanceCb
//
// Returns nil on timeout, else an Event
func (c *Consumer) Poll(timeoutMs int) (event Event) {
	ev, _ := c.handle.eventPoll(nil, timeoutMs, 1, nil)
	return ev
}

// Close Consumer instance.
// The object is no longer usable after this call.
func (c *Consumer) Close() (err error) {

	if c.eventsChanEnable {
		// Wait for consumerReader() to terminate (by closing readerTermChan)
		close(c.readerTermChan)
		c.handle.waitTerminated(1)

	}

	C.rd_kafka_queue_destroy(c.handle.rkq)
	c.handle.rkq = nil

	e := C.rd_kafka_consumer_close(c.handle.rk)
	if e != C.RD_KAFKA_RESP_ERR_NO_ERROR {
		return newError(e)
	}

	c.handle.cleanup()

	C.rd_kafka_destroy(c.handle.rk)

	return nil
}

// NewConsumer creates a new high-level Consumer instance.
//
// Supported special configuration properties:
//   go.application.rebalance.enable (bool, false) - Forward rebalancing responsibility to application via the Events channel.
//                                        If set to true the app must handle the AssignedPartitions and
//                                        RevokedPartitions events and call Assign() and Unassign()
//                                        respectively.
//   go.events.channel.enable (bool, false) - Enable the Events channel. Messages and events will be pushed on the Events channel and the Poll() interface will be disabled. (Experimental)
//   go.events.channel.size (int, 1000) - Events channel size
//
// WARNING: Due to the buffering nature of channels (and queues in general) the
// use of the events channel risks receiving outdated events and
// messages. Minimizing go.events.channel.size reduces the risk
// and number of outdated events and messages but does not eliminate
// the factor completely. With a channel size of 1 at most one
// event or message may be outdated.
func NewConsumer(conf *ConfigMap) (*Consumer, error) {

	groupid, _ := conf.get("group.id", nil)
	if groupid == nil {
		// without a group.id the underlying cgrp subsystem in librdkafka wont get started
		// and without it there is no way to consume assigned partitions.
		// So for now require the group.id, this might change in the future.
		return nil, newErrorFromString(ErrInvalidArg, "Required property group.id not set")
	}

	c := &Consumer{}

	v, err := conf.extract("go.application.rebalance.enable", false)
	if err != nil {
		return nil, err
	}
	c.appRebalanceEnable = v.(bool)

	v, err = conf.extract("go.events.channel.enable", false)
	if err != nil {
		return nil, err
	}
	c.eventsChanEnable = v.(bool)

	v, err = conf.extract("go.events.channel.size", 1000)
	if err != nil {
		return nil, err
	}
	eventsChanSize := v.(int)

	cConf, err := conf.convert()
	if err != nil {
		return nil, err
	}
	cErrstr := (*C.char)(C.malloc(C.size_t(256)))
	defer C.free(unsafe.Pointer(cErrstr))

	C.rd_kafka_conf_set_events(cConf, C.RD_KAFKA_EVENT_REBALANCE|C.RD_KAFKA_EVENT_OFFSET_COMMIT)

	c.handle.rk = C.rd_kafka_new(C.RD_KAFKA_CONSUMER, cConf, cErrstr, 256)
	if c.handle.rk == nil {
		return nil, newErrorFromCString(C.RD_KAFKA_RESP_ERR__INVALID_ARG, cErrstr)
	}

	C.rd_kafka_poll_set_consumer(c.handle.rk)

	c.handle.c = c
	c.handle.setup()
	c.handle.rkq = C.rd_kafka_queue_get_consumer(c.handle.rk)
	if c.handle.rkq == nil {
		// no cgrp (no group.id configured), revert to main queue.
		c.handle.rkq = C.rd_kafka_queue_get_main(c.handle.rk)
	}

	if c.eventsChanEnable {
		c.Events = make(chan Event, eventsChanSize)
		c.readerTermChan = make(chan bool)

		/* Start rdkafka consumer queue reader -> Events writer goroutine */
		go consumerReader(c, c.readerTermChan)
	}

	return c, nil
}

// rebalance calls the application's rebalance callback, if any.
// Returns true if the underlying assignment was updated, else false.
func (c *Consumer) rebalance(ev Event) bool {
	c.appReassigned = false

	if c.rebalanceCb != nil {
		c.rebalanceCb(c, ev)
	}

	return c.appReassigned
}

// consumerReader reads messages and events from the librdkafka consumer queue
// and posts them on the consumer channel.
// Runs until termChan closes
func consumerReader(c *Consumer, termChan chan bool) {

out:
	for true {
		select {
		case _ = <-termChan:
			break out
		default:
			_, term := c.handle.eventPoll(c.Events, 100, 1000, termChan)
			if term {
				break out
			}

		}
	}

	c.handle.terminatedChan <- "consumerReader"
	return

}

// GetMetadata queries broker for cluster and topic metadata.
// If topic is non-nil only information about that topic is returned, else if
// allTopics is false only information about locally used topics is returned,
// else information about all topics is returned.
func (c *Consumer) GetMetadata(topic *string, allTopics bool, timeoutMs int) (*Metadata, error) {
	return getMetadata(c, topic, allTopics, timeoutMs)
}

// QueryWatermarkOffsets returns the broker's low and high offsets for the given topic
// and partition.
func (c *Consumer) QueryWatermarkOffsets(topic string, partition int32, timeoutMs int) (low, high int64, err error) {
	return queryWatermarkOffsets(c, topic, partition, timeoutMs)
}
