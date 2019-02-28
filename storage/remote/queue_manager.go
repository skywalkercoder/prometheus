// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package remote

import (
	"context"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/config"
	pkgrelabel "github.com/prometheus/prometheus/pkg/relabel"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/relabel"
	"github.com/prometheus/tsdb"
)

// String constants for instrumentation.
const (
	namespace = "prometheus"
	subsystem = "remote_storage"
	queue     = "queue"

	// We track samples in/out and how long pushes take using an Exponentially
	// Weighted Moving Average.
	ewmaWeight          = 0.2
	shardUpdateDuration = 10 * time.Second

	// Allow 30% too many shards before scaling down.
	shardToleranceFraction = 0.3
)

var (
	succeededSamplesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "succeeded_samples_total",
			Help:      "Total number of samples successfully sent to remote storage.",
		},
		[]string{queue},
	)
	failedSamplesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "failed_samples_total",
			Help:      "Total number of samples which failed on send to remote storage, non-recoverable errors.",
		},
		[]string{queue},
	)
	retriedSamplesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "retried_samples_total",
			Help:      "Total number of samples which failed on send to remote storage but were retried because the send error was recoverable.",
		},
		[]string{queue},
	)
	droppedSamplesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "dropped_samples_total",
			Help:      "Total number of samples which were dropped after being read from the WAL before being sent via remote write.",
		},
		[]string{queue},
	)
	enqueueRetriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "enqueue_retries_total",
			Help:      "Total number of times enqueue has failed because a shards queue was full.",
		},
		[]string{queue},
	)
	sentBatchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "sent_batch_duration_seconds",
			Help:      "Duration of sample batch send calls to the remote storage.",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{queue},
	)
	queueHighestSentTimestamp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "queue_highest_sent_timestamp_seconds",
			Help:      "Timestamp from a WAL sample, the highest timestamp successfully sent by this queue, in seconds since epoch.",
		},
		[]string{queue},
	)
	queuePendingSamples = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "pending_samples",
			Help:      "The number of samples pending in the queues shards to be sent to the remote storage.",
		},
		[]string{queue},
	)
	shardCapacity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "shard_capacity",
			Help:      "The capacity of each shard of the queue used for parallel sending to the remote storage.",
		},
		[]string{queue},
	)
	numShards = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "shards",
			Help:      "The number of shards used for parallel sending to the remote storage.",
		},
		[]string{queue},
	)
)

func init() {
	prometheus.MustRegister(succeededSamplesTotal)
	prometheus.MustRegister(failedSamplesTotal)
	prometheus.MustRegister(retriedSamplesTotal)
	prometheus.MustRegister(droppedSamplesTotal)
	prometheus.MustRegister(enqueueRetriesTotal)
	prometheus.MustRegister(sentBatchDuration)
	prometheus.MustRegister(queueHighestSentTimestamp)
	prometheus.MustRegister(queuePendingSamples)
	prometheus.MustRegister(shardCapacity)
	prometheus.MustRegister(numShards)
}

// StorageClient defines an interface for sending a batch of samples to an
// external timeseries database.
type StorageClient interface {
	// Store stores the given samples in the remote storage.
	Store(context.Context, []byte) error
	// Name identifies the remote storage implementation.
	Name() string
}

// QueueManager manages a queue of samples to be sent to the Storage
// indicated by the provided StorageClient. Implements writeTo interface
// used by WAL Watcher.
type QueueManager struct {
	logger log.Logger

	flushDeadline              time.Duration
	cfg                        config.QueueConfig
	externalLabels             model.LabelSet
	relabelConfigs             []*pkgrelabel.Config
	client                     StorageClient
	queueName                  string
	watcher                    *WALWatcher
	highestSentTimestampMetric prometheus.Gauge
	pendingSamplesMetric       prometheus.Gauge
	enqueueRetriesMetric       prometheus.Counter

	highestSentTimestamp int64
	timestampLock        sync.Mutex

	highestTimestampIn *int64 // highest timestamp of any sample ingested by remote storage via scrape (Appender)

	seriesMtx            sync.Mutex
	seriesLabels         map[uint64][]prompb.Label
	seriesSegmentIndexes map[uint64]int
	droppedSeries        map[uint64]struct{}

	shards      *shards
	numShards   int
	reshardChan chan int
	quit        chan struct{}
	wg          sync.WaitGroup

	samplesIn, samplesDropped, samplesOut, samplesOutDuration *ewmaRate
	integralAccumulator                                       float64
}

// NewQueueManager builds a new QueueManager.
func NewQueueManager(logger log.Logger, walDir string, samplesIn *ewmaRate, highestTimestampIn *int64, cfg config.QueueConfig, externalLabels model.LabelSet, relabelConfigs []*pkgrelabel.Config, client StorageClient, flushDeadline time.Duration) *QueueManager {
	if logger == nil {
		logger = log.NewNopLogger()
	} else {
		logger = log.With(logger, "queue", client.Name())
	}
	t := &QueueManager{
		logger:         logger,
		flushDeadline:  flushDeadline,
		cfg:            cfg,
		externalLabels: externalLabels,
		relabelConfigs: relabelConfigs,
		client:         client,
		queueName:      client.Name(),

		highestTimestampIn: highestTimestampIn,

		seriesLabels:         make(map[uint64][]prompb.Label),
		seriesSegmentIndexes: make(map[uint64]int),
		droppedSeries:        make(map[uint64]struct{}),

		numShards:   cfg.MinShards,
		reshardChan: make(chan int),
		quit:        make(chan struct{}),

		samplesIn:          samplesIn,
		samplesDropped:     newEWMARate(ewmaWeight, shardUpdateDuration),
		samplesOut:         newEWMARate(ewmaWeight, shardUpdateDuration),
		samplesOutDuration: newEWMARate(ewmaWeight, shardUpdateDuration),
	}

	t.highestSentTimestampMetric = queueHighestSentTimestamp.WithLabelValues(t.queueName)
	t.pendingSamplesMetric = queuePendingSamples.WithLabelValues(t.queueName)
	t.enqueueRetriesMetric = enqueueRetriesTotal.WithLabelValues(t.queueName)
	t.watcher = NewWALWatcher(logger, client.Name(), t, walDir)
	t.shards = t.newShards()

	numShards.WithLabelValues(t.queueName).Set(float64(t.numShards))
	shardCapacity.WithLabelValues(t.queueName).Set(float64(t.cfg.Capacity))

	// Initialize counter labels to zero.
	sentBatchDuration.WithLabelValues(t.queueName)
	succeededSamplesTotal.WithLabelValues(t.queueName)
	failedSamplesTotal.WithLabelValues(t.queueName)
	droppedSamplesTotal.WithLabelValues(t.queueName)
	retriedSamplesTotal.WithLabelValues(t.queueName)
	// Reset pending samples metric to 0.
	t.pendingSamplesMetric.Set(0)

	return t
}

// Append queues a sample to be sent to the remote storage. Blocks until all samples are
// enqueued on their shards or a shutdown signal is received.
func (t *QueueManager) Append(s []tsdb.RefSample) bool {
	type enqueuable struct {
		ts  prompb.TimeSeries
		ref uint64
	}

	tempSamples := make([]enqueuable, 0, len(s))
	t.seriesMtx.Lock()
	for _, sample := range s {
		// If we have no labels for the series, due to relabelling or otherwise, don't send the sample.
		if _, ok := t.seriesLabels[sample.Ref]; !ok {
			droppedSamplesTotal.WithLabelValues(t.queueName).Inc()
			t.samplesDropped.incr(1)
			if _, ok := t.droppedSeries[sample.Ref]; !ok {
				level.Info(t.logger).Log("msg", "dropped sample for series that was not explicitly dropped via relabelling", "ref", sample.Ref)
			}
			continue
		}
		tempSamples = append(tempSamples, enqueuable{
			ts: prompb.TimeSeries{
				Labels: t.seriesLabels[sample.Ref],
				Samples: []prompb.Sample{
					prompb.Sample{
						Value:     float64(sample.V),
						Timestamp: sample.T,
					},
				},
			},
			ref: sample.Ref,
		})
	}
	t.seriesMtx.Unlock()

outer:
	for _, sample := range tempSamples {
		// This will only loop if the queues are being resharded.
		backoff := t.cfg.MinBackoff
		for {
			select {
			case <-t.quit:
				return false
			default:
			}

			if t.shards.enqueue(sample.ref, sample.ts) {
				continue outer
			}

			t.enqueueRetriesMetric.Inc()
			time.Sleep(time.Duration(backoff))
			backoff = backoff * 2
			if backoff > t.cfg.MaxBackoff {
				backoff = t.cfg.MaxBackoff
			}
		}
	}
	return true
}

// Start the queue manager sending samples to the remote storage.
// Does not block.
func (t *QueueManager) Start() {
	t.shards.start(t.numShards)
	t.watcher.Start()

	t.wg.Add(2)
	go t.updateShardsLoop()
	go t.reshardLoop()
}

// Stop stops sending samples to the remote storage and waits for pending
// sends to complete.
func (t *QueueManager) Stop() {
	level.Info(t.logger).Log("msg", "Stopping remote storage...")
	defer level.Info(t.logger).Log("msg", "Remote storage stopped.")

	close(t.quit)
	t.shards.stop()
	t.watcher.Stop()
	t.wg.Wait()
}

// StoreSeries keeps track of which series we know about for lookups when sending samples to remote.
func (t *QueueManager) StoreSeries(series []tsdb.RefSeries, index int) {
	temp := make(map[uint64][]prompb.Label, len(series))
	for _, s := range series {
		ls := make(model.LabelSet, len(s.Labels))
		for _, label := range s.Labels {
			ls[model.LabelName(label.Name)] = model.LabelValue(label.Value)
		}
		t.processExternalLabels(ls)
		rl := relabel.Process(ls, t.relabelConfigs...)
		if len(rl) == 0 {
			t.droppedSeries[s.Ref] = struct{}{}
			continue
		}
		temp[s.Ref] = labelsetToLabelsProto(rl)
	}

	t.seriesMtx.Lock()
	defer t.seriesMtx.Unlock()
	for ref, labels := range temp {
		t.seriesLabels[ref] = labels
		t.seriesSegmentIndexes[ref] = index
	}
}

// SeriesReset is used when reading a checkpoint. WAL Watcher should have
// stored series records with the checkpoints index number, so we can now
// delete any ref ID's lower than that # from the two maps.
func (t *QueueManager) SeriesReset(index int) {
	t.seriesMtx.Lock()
	defer t.seriesMtx.Unlock()

	// Check for series that are in segments older than the checkpoint
	// that were not also present in the checkpoint.
	for k, v := range t.seriesSegmentIndexes {
		if v < index {
			delete(t.seriesLabels, k)
			delete(t.seriesSegmentIndexes, k)
		}
	}
}

func (t *QueueManager) processExternalLabels(ls model.LabelSet) {
	for ln, lv := range t.externalLabels {
		if _, ok := ls[ln]; !ok {
			ls[ln] = lv
		}
	}
}

func (t *QueueManager) updateShardsLoop() {
	defer t.wg.Done()

	ticker := time.NewTicker(shardUpdateDuration)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.calculateDesiredShards()
		case <-t.quit:
			return
		}
	}
}

func (t *QueueManager) calculateDesiredShards() {
	t.samplesIn.tick()
	t.samplesOut.tick()
	t.samplesDropped.tick()
	t.samplesOutDuration.tick()

	// We use the number of incoming samples as a prediction of how much work we
	// will need to do next iteration.  We add to this any pending samples
	// (received - send) so we can catch up with any backlog. We use the average
	// outgoing batch latency to work out how many shards we need.
	var (
		samplesIn          = t.samplesIn.rate()
		samplesOut         = t.samplesOut.rate()
		samplesDropped     = t.samplesDropped.rate()
		samplesPending     = samplesIn - samplesDropped - samplesOut
		samplesOutDuration = t.samplesOutDuration.rate()
	)

	// We use an integral accumulator, like in a PID, to help dampen oscillation.
	t.integralAccumulator = t.integralAccumulator + (samplesPending * 0.1)

	if samplesOut <= 0 {
		return
	}

	var (
		timePerSample = samplesOutDuration / samplesOut
		desiredShards = (timePerSample * (samplesIn - samplesDropped + samplesPending + t.integralAccumulator)) / float64(time.Second)
	)
	level.Debug(t.logger).Log("msg", "QueueManager.caclulateDesiredShards",
		"samplesIn", samplesIn, "samplesDropped", samplesDropped,
		"samplesOut", samplesOut, "samplesPending", samplesPending,
		"desiredShards", desiredShards)

	// Changes in the number of shards must be greater than shardToleranceFraction.
	var (
		lowerBound = float64(t.numShards) * (1. - shardToleranceFraction)
		upperBound = float64(t.numShards) * (1. + shardToleranceFraction)
	)
	level.Debug(t.logger).Log("msg", "QueueManager.updateShardsLoop",
		"lowerBound", lowerBound, "desiredShards", desiredShards, "upperBound", upperBound)
	if lowerBound <= desiredShards && desiredShards <= upperBound {
		return
	}

	numShards := int(math.Ceil(desiredShards))
	if numShards > t.cfg.MaxShards {
		numShards = t.cfg.MaxShards
	} else if numShards < t.cfg.MinShards {
		numShards = t.cfg.MinShards
	}
	if numShards == t.numShards {
		return
	}

	// Resharding can take some time, and we want this loop
	// to stay close to shardUpdateDuration.
	select {
	case t.reshardChan <- numShards:
		level.Info(t.logger).Log("msg", "Remote storage resharding", "from", t.numShards, "to", numShards)
		t.numShards = numShards
	default:
		level.Info(t.logger).Log("msg", "Currently resharding, skipping.")
	}
}

func (t *QueueManager) reshardLoop() {
	defer t.wg.Done()

	for {
		select {
		case numShards := <-t.reshardChan:
			// We start the newShards after we have stopped (the therefore completely
			// flushed) the oldShards, to guarantee we only every deliver samples in
			// order.
			t.shards.stop()
			t.shards.start(numShards)
		case <-t.quit:
			return
		}
	}
}

func (t *QueueManager) newShards() *shards {
	s := &shards{
		qm:   t,
		done: make(chan struct{}),
	}
	return s
}

// Check and set highestSentTimestamp
func (t *QueueManager) setHighestSentTimestamp(highest int64) {
	t.timestampLock.Lock()
	defer t.timestampLock.Unlock()
	if highest > t.highestSentTimestamp {
		t.highestSentTimestamp = highest
		t.highestSentTimestampMetric.Set(float64(t.highestSentTimestamp) / 1000.)
	}
}

type shards struct {
	mtx sync.RWMutex // With the WAL, this is never actually contended.

	qm     *QueueManager
	queues []chan prompb.TimeSeries

	// Emulate a wait group with a channel and an atomic int, as you
	// cannot select on a wait group.
	done    chan struct{}
	running int32

	// Soft shutdown context will prevent new enqueues and deadlocks.
	softShutdown chan struct{}

	// Hard shutdown context is used to terminate outgoing HTTP connections
	// after giving them a chance to terminate.
	hardShutdown context.CancelFunc
}

// start the shards; must be called before any call to enqueue.
func (s *shards) start(n int) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	newQueues := make([]chan prompb.TimeSeries, n)
	for i := 0; i < n; i++ {
		newQueues[i] = make(chan prompb.TimeSeries, s.qm.cfg.Capacity)
	}

	s.queues = newQueues

	var hardShutdownCtx context.Context
	hardShutdownCtx, s.hardShutdown = context.WithCancel(context.Background())
	s.softShutdown = make(chan struct{})
	s.running = int32(n)
	s.done = make(chan struct{})
	for i := 0; i < n; i++ {
		go s.runShard(hardShutdownCtx, i, newQueues[i])
	}
	numShards.WithLabelValues(s.qm.queueName).Set(float64(n))
}

// stop the shards; subsequent call to enqueue will return false.
func (s *shards) stop() {
	// Attempt a clean shutdown, but only wait flushDeadline for all the shards
	// to cleanly exit.  As we're doing RPCs, enqueue can block indefinately.
	// We must be able so call stop concurrently, hence we can only take the
	// RLock here.
	s.mtx.RLock()
	close(s.softShutdown)
	s.mtx.RUnlock()

	// Enqueue should now be unblocked, so we can take the write lock.  This
	// also ensures we don't race with writes to the queues, and get a panic:
	// send on closed channel.
	s.mtx.Lock()
	defer s.mtx.Unlock()
	for _, queue := range s.queues {
		close(queue)
	}
	select {
	case <-s.done:
		return
	case <-time.After(s.qm.flushDeadline):
		level.Error(s.qm.logger).Log("msg", "Failed to flush all samples on shutdown")
	}

	// Force an unclean shutdown.
	s.hardShutdown()
	<-s.done
}

// enqueue a sample.  If we are currently in the process of shutting down or resharding,
// will return false; in this case, you should back off and retry.
func (s *shards) enqueue(ref uint64, sample prompb.TimeSeries) bool {
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	select {
	case <-s.softShutdown:
		return false
	default:
	}

	shard := uint64(ref) % uint64(len(s.queues))
	select {
	case <-s.softShutdown:
		return false
	case s.queues[shard] <- sample:
		return true
	}
}

func (s *shards) runShard(ctx context.Context, i int, queue chan prompb.TimeSeries) {
	defer func() {
		if atomic.AddInt32(&s.running, -1) == 0 {
			close(s.done)
		}
	}()

	shardNum := strconv.Itoa(i)

	// Send batches of at most MaxSamplesPerSend samples to the remote storage.
	// If we have fewer samples than that, flush them out after a deadline
	// anyways.
	pendingSamples := []prompb.TimeSeries{}

	max := s.qm.cfg.MaxSamplesPerSend
	timer := time.NewTimer(time.Duration(s.qm.cfg.BatchSendDeadline))
	stop := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	defer stop()

	for {
		select {
		case <-ctx.Done():
			return

		case sample, ok := <-queue:
			if !ok {
				if len(pendingSamples) > 0 {
					level.Debug(s.qm.logger).Log("msg", "Flushing samples to remote storage...", "count", len(pendingSamples))
					s.sendSamples(ctx, pendingSamples)
					s.qm.pendingSamplesMetric.Sub(float64(len(pendingSamples)))
					level.Debug(s.qm.logger).Log("msg", "Done flushing.")
				}
				return
			}

			// Number of pending samples is limited by the fact that sendSamples (via sendSamplesWithBackoff)
			// retries endlessly, so once we reach > 100 samples, if we can never send to the endpoint we'll
			// stop reading from the queue (which has a size of 10).
			pendingSamples = append(pendingSamples, sample)
			s.qm.pendingSamplesMetric.Inc()

			if len(pendingSamples) >= max {
				s.sendSamples(ctx, pendingSamples[:max])
				pendingSamples = pendingSamples[max:]
				s.qm.pendingSamplesMetric.Sub(float64(max))

				stop()
				timer.Reset(time.Duration(s.qm.cfg.BatchSendDeadline))
			}

		case <-timer.C:
			if len(pendingSamples) > 0 {
				level.Debug(s.qm.logger).Log("msg", "runShard timer ticked, sending samples", "samples", len(pendingSamples), "shard", shardNum)
				n := len(pendingSamples)
				s.sendSamples(ctx, pendingSamples)
				pendingSamples = pendingSamples[:0]
				s.qm.pendingSamplesMetric.Sub(float64(n))
			}
			timer.Reset(time.Duration(s.qm.cfg.BatchSendDeadline))
		}
	}
}

func (s *shards) sendSamples(ctx context.Context, samples []prompb.TimeSeries) {
	begin := time.Now()
	err := s.sendSamplesWithBackoff(ctx, samples)
	if err != nil {
		level.Error(s.qm.logger).Log("msg", "non-recoverable error", "count", len(samples), "err", err)
		failedSamplesTotal.WithLabelValues(s.qm.queueName).Add(float64(len(samples)))
	}

	// These counters are used to calculate the dynamic sharding, and as such
	// should be maintained irrespective of success or failure.
	s.qm.samplesOut.incr(int64(len(samples)))
	s.qm.samplesOutDuration.incr(int64(time.Since(begin)))
}

// sendSamples to the remote storage with backoff for recoverable errors.
func (s *shards) sendSamplesWithBackoff(ctx context.Context, samples []prompb.TimeSeries) error {
	backoff := s.qm.cfg.MinBackoff
	req, highest, err := buildWriteRequest(samples)
	if err != nil {
		// Failing to build the write request is non-recoverable, since it will
		// only error if marshaling the proto to bytes fails.
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		begin := time.Now()
		err := s.qm.client.Store(ctx, req)

		sentBatchDuration.WithLabelValues(s.qm.queueName).Observe(time.Since(begin).Seconds())

		if err == nil {
			succeededSamplesTotal.WithLabelValues(s.qm.queueName).Add(float64(len(samples)))
			s.qm.setHighestSentTimestamp(highest)
			return nil
		}

		if _, ok := err.(recoverableError); !ok {
			return err
		}
		retriedSamplesTotal.WithLabelValues(s.qm.queueName).Add(float64(len(samples)))

		time.Sleep(time.Duration(backoff))
		backoff = backoff * 2
		if backoff > s.qm.cfg.MaxBackoff {
			backoff = s.qm.cfg.MaxBackoff
		}
	}
}

func buildWriteRequest(samples []prompb.TimeSeries) ([]byte, int64, error) {
	var highest int64
	for _, ts := range samples {
		// At the moment we only ever append a TimeSeries with a single sample in it.
		if ts.Samples[0].Timestamp > highest {
			highest = ts.Samples[0].Timestamp
		}
	}
	req := &prompb.WriteRequest{
		Timeseries: samples,
	}

	data, err := proto.Marshal(req)
	if err != nil {
		return nil, highest, err
	}

	compressed := snappy.Encode(nil, data)
	return compressed, highest, nil
}
