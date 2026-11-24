//go:build windows

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/Microsoft/go-winio"
	ttrpcevents "github.com/containerd/containerd/api/services/ttrpc/events/v1"
	apitypes "github.com/containerd/containerd/api/types"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// publisher forwards the task's events to containerd's ttrpc address, in
// the order they happened, from one goroutine, dialing it again if
// containerd restarted.
type publisher struct {
	address string
	ns      string
	queue   chan *apitypes.Envelope
	done    chan struct{}

	client *ttrpc.Client
	events ttrpcevents.TTRPCEventsService
}

func newPublisher(address, ns string) *publisher {
	p := &publisher{address: address, ns: ns, queue: make(chan *apitypes.Envelope, 128), done: make(chan struct{})}
	go p.run()
	return p
}

// publish queues an event on topic.
func (p *publisher) publish(topic string, ev proto.Message) {
	a, err := anypb.New(ev)
	if err != nil {
		diagf("event %s: %v", topic, err)
		return
	}
	// containerd's events are known by their full names, as typeurl
	// registers them.
	a.TypeUrl = string(proto.MessageName(ev))
	p.queue <- &apitypes.Envelope{Timestamp: timestamppb.Now(), Namespace: p.ns, Topic: topic, Event: a}
}

// close sends what is queued, waiting at most 5 s.
func (p *publisher) close() {
	close(p.queue)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
	}
}

func (p *publisher) run() {
	defer close(p.done)
	for env := range p.queue {
		for try := 0; ; try++ {
			err := p.forward(env)
			if err == nil {
				break
			}
			if try == 4 {
				diagf("could not send containerd the %s event: %v", env.Topic, err)
				break
			}
			time.Sleep(time.Duration(try+1) * 200 * time.Millisecond)
		}
	}
	if p.client != nil {
		p.client.Close()
	}
}

func (p *publisher) forward(env *apitypes.Envelope) error {
	if p.address == "" {
		return fmt.Errorf("containerd gave no ttrpc address")
	}
	if p.events == nil {
		timeout := 5 * time.Second
		c, err := winio.DialPipe(p.address, &timeout)
		if err != nil {
			return err
		}
		p.client = ttrpc.NewClient(c)
		p.events = ttrpcevents.NewTTRPCEventsClient(p.client)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.events.Forward(ctx, &ttrpcevents.ForwardRequest{Envelope: env})
	if err != nil {
		p.client.Close()
		p.client, p.events = nil, nil
	}
	return err
}
