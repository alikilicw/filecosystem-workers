// Package worker wires a queue delivery to the image processor: fetch the
// input, make the version, store what it produced and report back over the
// event exchange.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/alikilicw/filecosystem-workers/internal/contracts"
	imageproc "github.com/alikilicw/filecosystem-workers/internal/processor/image"
	"github.com/alikilicw/filecosystem-workers/internal/queue"
	"github.com/alikilicw/filecosystem-workers/internal/storage"
)

// intermediateFactor bounds an intermediate relative to the source limit. A
// decoded image stored as a lightly compressed PNG is much larger than a JPEG
// of the same pixels, so it gets more room than an upload does.
const intermediateFactor = 10

type ImageWorker struct {
	storage   *storage.Storage
	broker    *queue.Client
	processor *imageproc.Processor
	log       *slog.Logger
	maxSource int64
	timeout   time.Duration
}

func NewImageWorker(
	store *storage.Storage,
	broker *queue.Client,
	processor *imageproc.Processor,
	log *slog.Logger,
	maxSource int64,
	timeout time.Duration,
) *ImageWorker {
	return &ImageWorker{
		storage:   store,
		broker:    broker,
		processor: processor,
		log:       log,
		maxSource: maxSource,
		timeout:   timeout,
	}
}

// Handle always reports an outcome to the API, including failures. A job that
// simply disappeared would leave the session locked until its lock expires, so
// the only errors returned here are the ones that make reporting impossible.
func (w *ImageWorker) Handle(ctx context.Context, body []byte) error {
	var msg contracts.JobMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return fmt.Errorf("decode job message: %w", err)
	}
	if msg.JobID == "" || msg.InputKey == "" || msg.OutputPrefix == "" {
		return fmt.Errorf("job message is missing job_id, input_key or output_prefix")
	}

	jobCtx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()

	started := time.Now()
	result, err := w.process(jobCtx, msg)
	if err != nil {
		w.log.Error("job failed",
			"job_id", msg.JobID,
			"operation", msg.Operation,
			"duration_ms", time.Since(started).Milliseconds(),
			"error", err,
		)
		return w.publish(ctx, contracts.JobResult{
			JobID:      msg.JobID,
			Status:     contracts.StatusFailed,
			Error:      err.Error(),
			FinishedAt: time.Now().UTC(),
		})
	}

	w.log.Info("job succeeded",
		"job_id", msg.JobID,
		"operation", msg.Operation,
		"result_size", result.ResultSize,
		"duration_ms", time.Since(started).Milliseconds(),
	)
	return w.publish(ctx, result)
}

func (w *ImageWorker) process(ctx context.Context, msg contracts.JobMessage) (contracts.JobResult, error) {
	switch msg.Operation {
	case contracts.OpSource:
		return w.source(ctx, msg)
	case contracts.OpEdit:
		return w.edit(ctx, msg)
	}
	return contracts.JobResult{}, fmt.Errorf("unknown operation %q", msg.Operation)
}

// source opens an upload. The version it makes is the original itself, so
// its result points at the uploaded object and nothing is re-encoded.
func (w *ImageWorker) source(ctx context.Context, msg contracts.JobMessage) (contracts.JobResult, error) {
	clock := newPhases()
	data, err := w.download(ctx, msg.InputKey, w.maxSource)
	if err != nil {
		return contracts.JobResult{}, err
	}
	clock.mark("download")
	out, err := w.processor.Source(data, msg.SourceMime)
	if err != nil {
		return contracts.JobResult{}, err
	}
	clock.mark("process")

	intermediateKey := msg.OutputPrefix + "intermediate.png"
	previewKey := msg.OutputPrefix + "preview.webp"
	if err := w.putAll(ctx,
		object{intermediateKey, out.Intermediate, "image/png", "intermediate.png"},
		object{previewKey, out.Preview, "image/webp", "preview.webp"},
	); err != nil {
		return contracts.JobResult{}, err
	}
	clock.mark("upload")
	w.log.Info("job phases", "job_id", msg.JobID, "input_bytes", len(data), "intermediate_bytes", len(out.Intermediate), clock.attr())

	enc := out.Encode
	return contracts.JobResult{
		JobID:           msg.JobID,
		Status:          contracts.StatusSucceeded,
		ResultKey:       msg.InputKey,
		ResultName:      msg.SourceName,
		ResultSize:      int64(len(data)),
		ResultMime:      msg.SourceMime,
		IntermediateKey: intermediateKey,
		PreviewKey:      previewKey,
		Width:           out.Width,
		Height:          out.Height,
		Animation:       out.Animation,
		Encode:          &enc,
		FinishedAt:      time.Now().UTC(),
	}, nil
}

// edit makes one version from its parent's intermediate.
func (w *ImageWorker) edit(ctx context.Context, msg contracts.JobMessage) (contracts.JobResult, error) {
	if msg.Encode == nil {
		return contracts.JobResult{}, fmt.Errorf("edit job has no encode settings")
	}
	ext, mime, ok := contracts.FormatInfo(msg.Encode.Format)
	if !ok {
		return contracts.JobResult{}, fmt.Errorf("unsupported output format %q", msg.Encode.Format)
	}

	clock := newPhases()
	data, err := w.download(ctx, msg.InputKey, w.maxSource*intermediateFactor)
	if err != nil {
		return contracts.JobResult{}, err
	}
	clock.mark("download")
	out, err := w.processor.Edit(data, msg.Animation, msg.Step, *msg.Encode)
	if err != nil {
		return contracts.JobResult{}, err
	}
	clock.mark("process")

	name := resultName(msg.SourceName, ext)
	resultKey := msg.OutputPrefix + "result" + ext
	previewKey := msg.OutputPrefix + "preview.webp"
	objects := []object{
		{resultKey, out.Result, mime, name},
		{previewKey, out.Preview, "image/webp", "preview.webp"},
	}
	// No step means the pixels did not change: the parent's intermediate is
	// still exactly this version's.
	intermediateKey := msg.InputKey
	if out.Intermediate != nil {
		intermediateKey = msg.OutputPrefix + "intermediate.png"
		objects = append(objects, object{intermediateKey, out.Intermediate, "image/png", "intermediate.png"})
	}
	if err := w.putAll(ctx, objects...); err != nil {
		return contracts.JobResult{}, err
	}
	clock.mark("upload")
	w.log.Info("job phases", "job_id", msg.JobID, "input_bytes", len(data), "intermediate_bytes", len(out.Intermediate), "result_bytes", len(out.Result), clock.attr())

	enc := out.Encode
	return contracts.JobResult{
		JobID:           msg.JobID,
		Status:          contracts.StatusSucceeded,
		ResultKey:       resultKey,
		ResultName:      name,
		ResultSize:      int64(len(out.Result)),
		ResultMime:      mime,
		IntermediateKey: intermediateKey,
		PreviewKey:      previewKey,
		Width:           out.Width,
		Height:          out.Height,
		Animation:       out.Animation,
		Encode:          &enc,
		FinishedAt:      time.Now().UTC(),
	}, nil
}

type object struct {
	key  string
	data []byte
	mime string
	name string
}

// putAll stores the objects side by side: a job's time is mostly round trips
// to the bucket, so they should not queue behind each other.
func (w *ImageWorker) putAll(ctx context.Context, objects ...object) error {
	errs := make([]error, len(objects))
	var wg sync.WaitGroup
	for i, o := range objects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = w.storage.Put(ctx, o.key, bytes.NewReader(o.data), int64(len(o.data)), o.mime, o.name)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// phases times the steps of one job for the log.
type phases struct {
	last time.Time
	ms   []any
}

func newPhases() *phases { return &phases{last: time.Now()} }

func (p *phases) mark(name string) {
	now := time.Now()
	p.ms = append(p.ms, slog.Int64(name+"_ms", now.Sub(p.last).Milliseconds()))
	p.last = now
}

func (p *phases) attr() slog.Attr { return slog.Group("phases", p.ms...) }

func (w *ImageWorker) download(ctx context.Context, key string, limit int64) ([]byte, error) {
	reader, err := w.storage.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()

	// One byte over the limit is enough to tell that the object is too large
	// without buffering the whole thing.
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("input exceeds %d bytes", limit)
	}
	return data, nil
}

func (w *ImageWorker) publish(ctx context.Context, result contracts.JobResult) error {
	body, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode job result: %w", err)
	}
	if err := w.broker.Publish(ctx, contracts.ExchangeEvents, contracts.RoutingKeyJobResult, body); err != nil {
		return fmt.Errorf("publish job result: %w", err)
	}
	return nil
}

// resultName keeps the original stem and marks the file as edited, so a
// download folder full of results stays readable.
func resultName(sourceName, ext string) string {
	base := strings.TrimSuffix(sourceName, path.Ext(sourceName))
	if base == "" {
		base = "image"
	}
	return base + "-edited" + ext
}
