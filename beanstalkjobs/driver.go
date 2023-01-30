package beanstalkjobs

import (
	"bytes"
	"context"
	"encoding/gob"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/roadrunner-server/api/v4/plugins/v1/jobs"
	pq "github.com/roadrunner-server/api/v4/plugins/v1/priority_queue"
	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/sdk/v4/utils"
	"go.uber.org/zap"
)

const pluginName string = "beanstalk"

var _ jobs.Driver = (*Driver)(nil)

type Configurer interface {
	// UnmarshalKey takes a single key and unmarshals it into a Struct.
	UnmarshalKey(name string, out any) error
	// Has checks if config section exists.
	Has(name string) bool
}

type Driver struct {
	log        *zap.Logger
	pq         pq.Queue
	consumeAll bool

	pipeline  atomic.Pointer[jobs.Pipeline]
	listeners uint32

	// beanstalk
	pool           *ConnPool
	addr           string
	network        string
	reserveTimeout time.Duration
	reconnectCh    chan struct{}
	tout           time.Duration
	// tube name
	tName        string
	tubePriority *uint32
	priority     int64

	stopCh chan struct{}
}

func FromConfig(configKey string, log *zap.Logger, cfg Configurer, pipe jobs.Pipeline, pq pq.Queue, _ chan<- jobs.Commander) (*Driver, error) {
	const op = errors.Op("new_beanstalk_consumer")

	// PARSE CONFIGURATION -------
	var conf config
	if !cfg.Has(configKey) {
		return nil, errors.E(op, errors.Errorf("no configuration by provided key: %s", configKey))
	}

	// if no global section
	if !cfg.Has(pluginName) {
		return nil, errors.E(op, errors.Str("no global beanstalk configuration, global configuration should contain beanstalk addrs and timeout"))
	}

	err := cfg.UnmarshalKey(configKey, &conf)
	if err != nil {
		return nil, errors.E(op, err)
	}

	err = cfg.UnmarshalKey(pluginName, &conf)
	if err != nil {
		return nil, errors.E(op, err)
	}

	conf.InitDefault()

	// PARSE CONFIGURATION -------

	dsn := strings.Split(conf.Addr, "://")
	if len(dsn) != 2 {
		return nil, errors.E(op, errors.Errorf("invalid socket DSN (tcp://127.0.0.1:11300, unix://beanstalk.sock), provided: %s", conf.Addr))
	}

	cPool, err := NewConnPool(dsn[0], dsn[1], conf.Tube, conf.Timeout, log)
	if err != nil {
		return nil, errors.E(op, err)
	}

	// initialize job Driver
	jc := &Driver{
		pq:             pq,
		log:            log,
		pool:           cPool,
		network:        dsn[0],
		addr:           dsn[1],
		consumeAll:     conf.ConsumeAll,
		tout:           conf.Timeout,
		tName:          conf.Tube,
		reserveTimeout: conf.ReserveTimeout,
		tubePriority:   conf.TubePriority,
		priority:       conf.PipePriority,

		// buffered with two because jobs root plugin can call Stop at the same time as Pause
		stopCh:      make(chan struct{}, 2),
		reconnectCh: make(chan struct{}, 2),
	}

	jc.pipeline.Store(&pipe)

	return jc, nil
}

func FromPipeline(pipe jobs.Pipeline, log *zap.Logger, cfg Configurer, pq pq.Queue, _ chan<- jobs.Commander) (*Driver, error) {
	const op = errors.Op("new_beanstalk_consumer")

	// PARSE CONFIGURATION -------
	var conf config
	// if no global section
	if !cfg.Has(pluginName) {
		return nil, errors.E(op, errors.Str("no global beanstalk configuration, global configuration should contain beanstalk 'addrs' and timeout"))
	}

	err := cfg.UnmarshalKey(pluginName, &conf)
	if err != nil {
		return nil, errors.E(op, err)
	}

	conf.InitDefault()
	// PARSE CONFIGURATION -------

	dsn := strings.Split(conf.Addr, "://")
	if len(dsn) != 2 {
		return nil, errors.E(op, errors.Errorf("invalid socket DSN (tcp://127.0.0.1:11300, unix://beanstalk.sock), provided: %s", conf.Addr))
	}

	cPool, err := NewConnPool(dsn[0], dsn[1], pipe.String(tube, "default"), conf.Timeout, log)
	if err != nil {
		return nil, errors.E(op, err)
	}

	// initialize job Driver
	jc := &Driver{
		pq:             pq,
		log:            log,
		pool:           cPool,
		network:        dsn[0],
		addr:           dsn[1],
		tout:           conf.Timeout,
		consumeAll:     pipe.Bool(consumeAll, false),
		tName:          pipe.String(tube, "default"),
		reserveTimeout: time.Second * time.Duration(pipe.Int(reserveTimeout, 5)),
		tubePriority:   utils.Uint32(uint32(pipe.Int(tubePriority, 1))),
		priority:       pipe.Priority(),

		// buffered with two because jobs root plugin can call Stop at the same time as Pause
		stopCh:      make(chan struct{}, 2),
		reconnectCh: make(chan struct{}, 2),
	}

	jc.pipeline.Store(&pipe)

	return jc, nil
}
func (d *Driver) Push(ctx context.Context, jb jobs.Job) error {
	const op = errors.Op("beanstalk_push")
	// check if the pipeline registered

	// load atomic value
	pipe := *d.pipeline.Load()
	if pipe.Name() != jb.Pipeline() {
		return errors.E(op, errors.Errorf("no such pipeline: %s, actual: %s", jb.Pipeline(), pipe.Name()))
	}

	err := d.handleItem(ctx, fromJob(jb))
	if err != nil {
		return errors.E(op, err)
	}

	return nil
}

// State https://github.com/beanstalkd/beanstalkd/blob/master/doc/protocol.txt#L514
func (d *Driver) State(ctx context.Context) (*jobs.State, error) {
	const op = errors.Op("beanstalk_state")
	stat, err := d.pool.Stats(ctx)
	if err != nil {
		return nil, errors.E(op, err)
	}

	pipe := *d.pipeline.Load()

	out := &jobs.State{
		Priority: uint64(pipe.Priority()),
		Pipeline: pipe.Name(),
		Driver:   pipe.Driver(),
		Queue:    d.tName,
		Ready:    ready(atomic.LoadUint32(&d.listeners)),
	}

	// set stat, skip errors (replace with 0)
	// https://github.com/beanstalkd/beanstalkd/blob/master/doc/protocol.txt#L523
	if v, err := strconv.Atoi(stat["current-jobs-ready"]); err == nil {
		out.Active = int64(v)
	}

	// https://github.com/beanstalkd/beanstalkd/blob/master/doc/protocol.txt#L525
	if v, err := strconv.Atoi(stat["current-jobs-reserved"]); err == nil {
		// this is not an error, reserved in beanstalk behaves like an active jobs
		out.Reserved = int64(v)
	}

	// https://github.com/beanstalkd/beanstalkd/blob/master/doc/protocol.txt#L528
	if v, err := strconv.Atoi(stat["current-jobs-delayed"]); err == nil {
		out.Delayed = int64(v)
	}

	return out, nil
}

func (d *Driver) Run(_ context.Context, p jobs.Pipeline) error {
	const op = errors.Op("beanstalk_run")
	start := time.Now()

	// load atomic value
	// check if the pipeline registered
	pipe := *d.pipeline.Load()
	if pipe.Name() != p.Name() {
		return errors.E(op, errors.Errorf("no such pipeline: %s, actual: %s", p.Name(), pipe.Name()))
	}

	atomic.AddUint32(&d.listeners, 1)

	go d.listen()

	d.log.Debug("pipeline was started", zap.String("driver", pipe.Driver()), zap.String("pipeline", pipe.Name()), zap.Time("start", start), zap.Duration("elapsed", time.Since(start)))
	return nil
}

func (d *Driver) Stop(context.Context) error {
	start := time.Now()
	pipe := *d.pipeline.Load()

	if atomic.LoadUint32(&d.listeners) == 1 {
		d.stopCh <- struct{}{}
	}

	// release associated resources
	d.pool.Stop()

	d.log.Debug("pipeline was stopped", zap.String("driver", pipe.Driver()), zap.String("pipeline", pipe.Name()), zap.Time("start", start), zap.Duration("elapsed", time.Since(start)))
	return nil
}

func (d *Driver) Pause(_ context.Context, p string) error {
	start := time.Now()
	// load atomic value
	pipe := *d.pipeline.Load()
	if pipe.Name() != p {
		return errors.Errorf("no such pipeline: %s", pipe.Name())
	}

	l := atomic.LoadUint32(&d.listeners)
	// no active listeners
	if l == 0 {
		return errors.Str("no active listeners, nothing to pause")
	}

	atomic.AddUint32(&d.listeners, ^uint32(0))

	d.stopCh <- struct{}{}
	d.log.Debug("pipeline was paused", zap.String("driver", pipe.Driver()), zap.String("pipeline", pipe.Name()), zap.Time("start", start), zap.Duration("elapsed", time.Since(start)))

	return nil
}

func (d *Driver) Resume(_ context.Context, p string) error {
	start := time.Now()
	// load atomic value
	pipe := *d.pipeline.Load()
	if pipe.Name() != p {
		return errors.Errorf("no such pipeline: %s", pipe.Name())
	}

	l := atomic.LoadUint32(&d.listeners)
	// no active listeners
	if l == 1 {
		return errors.Str("sqs listener already in the active state")
	}

	// start listener
	go d.listen()

	// increase num of listeners
	atomic.AddUint32(&d.listeners, 1)
	d.log.Debug("pipeline was resumed", zap.String("driver", pipe.Driver()), zap.String("pipeline", pipe.Name()), zap.Time("start", start), zap.Duration("elapsed", time.Since(start)))

	return nil
}

func (d *Driver) handleItem(ctx context.Context, item *Item) error {
	const op = errors.Op("beanstalk_handle_item")

	bb := new(bytes.Buffer)
	bb.Grow(64)
	err := gob.NewEncoder(bb).Encode(item)
	if err != nil {
		return errors.E(op, err)
	}

	body := make([]byte, bb.Len())
	copy(body, bb.Bytes())
	bb.Reset()
	bb = nil

	// https://github.com/beanstalkd/beanstalkd/blob/master/doc/protocol.txt#L458
	// <pri> is an integer < 2**32. Jobs with smaller priority values will be
	// scheduled before jobs with larger priorities. The most urgent priority is 0;
	// the least urgent priority is 4,294,967,295.
	//
	// <delay> is an integer number of seconds to wait before putting the job in
	// the ready queue. The job will be in the "delayed" state during this time.
	// Maximum delay is 2**32-1.
	//
	// <ttr> -- time to run -- is an integer number of seconds to allow a worker
	// to run this job. This time is counted from the moment a worker reserves
	// this job. If the worker does not delete, release, or bury the job within
	// <ttr> seconds, the job will time out and the server will release the job.
	//	The minimum ttr is 1. If the client sends 0, the server will silently
	// increase the ttr to 1. Maximum ttr is 2**32-1.
	id, err := d.pool.Put(ctx, body, *d.tubePriority, item.Options.DelayDuration(), d.tout)
	if err != nil {
		errD := d.pool.Delete(ctx, id)
		if errD != nil {
			return errors.E(op, errors.Errorf("%s:%s", err.Error(), errD.Error()))
		}
		return errors.E(op, err)
	}

	return nil
}

func ready(r uint32) bool {
	return r > 0
}