package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bojand/ghz/protodesc"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/jhump/protoreflect/dynamic/grpcdynamic"
	"github.com/jhump/protoreflect/grpcreflect"

	"go.uber.org/multierr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	reflectpb "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
)

// Max size of the buffer of result channel.
const maxResult = 1000000

// result of a call
type callResult struct {
	err       error
	status    string
	duration  time.Duration
	timestamp time.Time
}

// Requester is used for doing the requests
type Requester struct {
	conns    []*grpc.ClientConn
	stubs    []grpcdynamic.Stub
	handlers []*statsHandler

	mtd      *desc.MethodDescriptor
	reporter *Reporter

	config *RunConfig

	results chan *callResult

	reqCounter int64

	arrayJSONData []string

	lock sync.Mutex
}

func newRequester(c *RunConfig) (*Requester, error) {

	var err error
	var mtd *desc.MethodDescriptor

	reqr := &Requester{
		config:  c,
		results: make(chan *callResult, min(c.c*1000, maxResult)),
		conns:   make([]*grpc.ClientConn, 0, c.nConns),
		stubs:   make([]grpcdynamic.Stub, 0, c.nConns),
	}

	if c.proto != "" {
		mtd, err = protodesc.GetMethodDescFromProto(c.call, c.proto, c.importPaths)
	} else if c.protoset != "" {
		mtd, err = protodesc.GetMethodDescFromProtoSet(c.call, c.protoset)
	} else {
		// use reflection to get method descriptor
		var cc *grpc.ClientConn
		// temporary connection for reflection, do not store as requester connections
		cc, err = reqr.newClientConn(false)
		if err != nil {
			return nil, err
		}

		defer func() {
			// purposefully ignoring error as we do not care if there
			// is an error on close
			_ = cc.Close()
		}()

		// cancel is ignored here as connection.Close() is used.
		// See https://godoc.org/google.golang.org/grpc#DialContext
		ctx, _ := context.WithTimeout(context.Background(), c.dialTimeout)

		md := make(metadata.MD)
		if c.rmd != nil && len(c.rmd) > 0 {
			md = metadata.New(c.rmd)
		}

		refCtx := metadata.NewOutgoingContext(ctx, md)

		refClient := grpcreflect.NewClient(refCtx, reflectpb.NewServerReflectionClient(cc))

		mtd, err = protodesc.GetMethodDescFromReflect(c.call, refClient)
	}

	if err != nil {
		return nil, err
	}

	md := mtd.GetInputType()
	payloadMessage := dynamic.NewMessage(md)
	if payloadMessage == nil {
		return nil, fmt.Errorf("No input type of method: %s", mtd.GetName())
	}

	// fill in the rest
	reqr.mtd = mtd

	// fill in JSON string array data for optimization for non client-streaming
	reqr.arrayJSONData = nil
	if !c.binary && !reqr.mtd.IsClientStreaming() {
		if strings.IndexRune(string(c.data), '[') == 0 { // it's an array
			var dat []map[string]interface{}
			if err := json.Unmarshal(c.data, &dat); err != nil {
				return nil, err
			}

			reqr.arrayJSONData = make([]string, len(dat))
			for i, d := range dat {
				var strd []byte
				if strd, err = json.Marshal(d); err != nil {
					return nil, err
				}

				reqr.arrayJSONData[i] = string(strd)
			}
		}
	}

	return reqr, nil
}

// Run makes all the requests and returns a report of results
// It blocks until all work is done.
func (b *Requester) Run(stopCh chan StopReason) (*Report, error) {
	start := time.Now()

	cc, connErr := b.openClientConns()
	if connErr != nil {
		return nil, connErr
	}

	b.lock.Lock()
	// create a client stub for each connection
	for n := 0; n < b.config.nConns; n++ {
		stub := grpcdynamic.NewStub(cc[n])
		b.stubs = append(b.stubs, stub)
	}

	b.reporter = newReporter(b.results, b.config)
	b.lock.Unlock()

	stopReason := ReasonNormalEnd

	stop := make(chan bool, 1)
	done := make(chan error, 1)

	go func() {
		if b.config.loadStrategy == StrategyConcurrency &&
			b.config.loadSchedule == ScheduleConst {
			err := b.runConstConcurrencyWorkers(stop)
			done <- err
		}
	}()

	go func() {
		b.reporter.Run()
	}()

	for {
		select {
		case reason := <-stopCh:
			stopReason = reason

			stop <- true

			if b.config.zstop == "close" {
				b.closeClientConns()
			} else if b.config.zstop == "ignore" {
				for _, h := range b.handlers {
					h.Ignore(true)
				}
				b.closeClientConns()
			}
		case err := <-done:

			total := time.Since(start)

			close(b.results)
			close(stop)
			close(done)

			if b.config.hasLog {
				b.config.log.Debug("Waiting for report")
			}

			// Wait until the reporter is done.

			<-b.reporter.done

			if b.config.hasLog {
				b.config.log.Debug("Finilizing report")
			}

			report := b.reporter.Finalize(stopReason, total)

			b.closeClientConns()

			return report, err
		}
	}
}

func (b *Requester) openClientConns() ([]*grpc.ClientConn, error) {
	b.lock.Lock()
	defer b.lock.Unlock()

	if len(b.conns) == b.config.nConns {
		return b.conns, nil
	}

	for n := 0; n < b.config.nConns; n++ {
		c, err := b.newClientConn(true)
		if err != nil {
			if b.config.hasLog {
				b.config.log.Errorf("Error creating client connection: %+v", err.Error())
			}

			return nil, err
		}

		b.conns = append(b.conns, c)
	}

	return b.conns, nil
}

func (b *Requester) closeClientConns() {
	if b.config.hasLog {
		b.config.log.Debug("Closing client connections")
	}

	b.lock.Lock()
	defer b.lock.Unlock()
	if b.conns == nil {
		return
	}

	for _, cc := range b.conns {
		_ = cc.Close()
	}

	b.conns = nil
}

func (b *Requester) newClientConn(withStatsHandler bool) (*grpc.ClientConn, error) {
	var opts []grpc.DialOption

	if b.config.insecure {
		opts = append(opts, grpc.WithInsecure())
	} else {
		opts = append(opts, grpc.WithTransportCredentials(b.config.creds))
	}

	if b.config.authority != "" {
		opts = append(opts, grpc.WithAuthority(b.config.authority))
	}

	ctx := context.Background()
	ctx, _ = context.WithTimeout(ctx, b.config.dialTimeout)
	// cancel is ignored here as connection.Close() is used.
	// See https://godoc.org/google.golang.org/grpc#DialContext

	if b.config.keepaliveTime > 0 {
		opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    b.config.keepaliveTime,
			Timeout: b.config.keepaliveTime,
		}))
	}

	if withStatsHandler {
		sh := &statsHandler{
			id:      len(b.handlers),
			results: b.results,
			hasLog:  b.config.hasLog,
			log:     b.config.log,
		}

		b.handlers = append(b.handlers, sh)

		opts = append(opts, grpc.WithStatsHandler(sh))
	}

	if b.config.hasLog {
		b.config.log.Debugw("Creating client connection", "options", opts)
	}

	// increase max receive and send message sizes
	opts = append(opts,
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(math.MaxInt32),
			grpc.MaxCallSendMsgSize(math.MaxInt32),
		))

	// create client connection
	return grpc.DialContext(ctx, b.config.host, opts...)
}

func (b *Requester) runConstConcurrencyWorkers(stop chan bool) error {
	nReqPerWorker := b.config.n / b.config.c

	if b.config.c == 0 {
		return nil
	}

	errC := make(chan error, b.config.c)

	workers := make(map[string]*Worker)

	// Ignore the case where b.N % b.C != 0.

	n := 0                            // connection counter
	for i := 0; i < b.config.c; i++ { // concurrency counter

		wID := "g" + strconv.Itoa(i) + "c" + strconv.Itoa(n)

		if len(b.config.name) > 0 {
			wID = b.config.name + ":" + wID
		}

		if b.config.hasLog {
			b.config.log.Debugw("Creating worker with ID: "+wID,
				"workerID", wID, "requests per worker", nReqPerWorker)
		}

		w := &Worker{
			stub:          b.stubs[n],
			mtd:           b.mtd,
			config:        b.config,
			reqCounter:    &b.reqCounter,
			workerID:      wID,
			arrayJSONData: b.arrayJSONData,
			done:          make(chan bool, 1),
		}

		w.setActive(true)

		workers[wID] = w

		n++ // increment connection counter

		// wrap around connections if needed
		if n == b.config.nConns {
			n = 0
		}

		go func() {
			errC <- w.runWorker(func(id string, err error, reqCount int, duration time.Duration) bool {
				return reqCount < nReqPerWorker
			}, true)
		}()
	}

	done := make(chan error, 1)

	go func() {
		var err error
		for i := 0; i < len(workers); i++ {
			err = multierr.Append(err, <-errC)
		}

		done <- err
	}()

	finishWorkers := func() {
		for _, wrk := range workers {
			wrk := wrk
			if wrk.isActive() {
				wrk.setActive(false)
				if wrk.done != nil {
					wrk.done <- true
				}
			}
		}
	}

	for {
		select {
		case <-stop:
			finishWorkers()
		case err := <-done:

			for _, wrk := range workers {
				wrk := wrk
				if wrk.done != nil {
					close(wrk.done)
				}
			}

			close(errC)
			return err
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
