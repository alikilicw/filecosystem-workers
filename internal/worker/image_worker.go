// Package worker wires a queue delivery to the image processor: fetch the
// source, transform it, store the result and report back over the event
// exchange.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/alikilicw/filecosystem-workers/internal/contracts"
	imageproc "github.com/alikilicw/filecosystem-workers/internal/processor/image"
	"github.com/alikilicw/filecosystem-workers/internal/queue"
	"github.com/alikilicw/filecosystem-workers/internal/storage"
)

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
// simply disappeared would leave the user staring at a spinner, so the only
// errors returned here are the ones that make reporting impossible.
func (w *ImageWorker) Handle(ctx context.Context, body []byte) error {
	var msg contracts.JobMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return fmt.Errorf("decode job message: %w", err)
	}
	if msg.JobID == "" || msg.SourceKey == "" {
		return fmt.Errorf("job message is missing job_id or source_key")
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
	source, err := w.download(ctx, msg.SourceKey)
	if err != nil {
		return contracts.JobResult{}, err
	}

	out, err := w.processor.Process(msg.Operation, msg.Params, source)
	if err != nil {
		return contracts.JobResult{}, err
	}

	name := resultName(msg, out.Ext)
	key := fmt.Sprintf("results/%s/%s", msg.JobID, name)
	if err := w.storage.Put(ctx, key, bytes.NewReader(out.Data), int64(len(out.Data)), out.Mime); err != nil {
		return contracts.JobResult{}, err
	}

	return contracts.JobResult{
		JobID:      msg.JobID,
		Status:     contracts.StatusSucceeded,
		ResultKey:  key,
		ResultName: name,
		ResultSize: int64(len(out.Data)),
		ResultMime: out.Mime,
		Width:      out.Width,
		Height:     out.Height,
		FinishedAt: time.Now().UTC(),
	}, nil
}

func (w *ImageWorker) download(ctx context.Context, key string) ([]byte, error) {
	reader, err := w.storage.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()

	// One byte over the limit is enough to tell that the object is too large
	// without buffering the whole thing.
	data, err := io.ReadAll(io.LimitReader(reader, w.maxSource+1))
	if err != nil {
		return nil, fmt.Errorf("read source: %w", err)
	}
	if int64(len(data)) > w.maxSource {
		return nil, fmt.Errorf("source exceeds %d bytes", w.maxSource)
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

// resultName keeps the original stem and describes the transformation, so a
// download folder full of results stays readable.
func resultName(msg contracts.JobMessage, ext string) string {
	base := strings.TrimSuffix(msg.SourceName, path.Ext(msg.SourceName))
	if base == "" {
		base = "image"
	}

	switch msg.Operation {
	case contracts.OpResize:
		return fmt.Sprintf("%s-%s%s", base, dimensionSuffix(msg.Params), ext)
	case contracts.OpCompress:
		return base + "-compressed" + ext
	default:
		return base + ext
	}
}

func dimensionSuffix(params contracts.ImageParams) string {
	switch {
	case params.Width > 0 && params.Height > 0:
		return fmt.Sprintf("%dx%d", params.Width, params.Height)
	case params.Width > 0:
		return fmt.Sprintf("w%d", params.Width)
	default:
		return fmt.Sprintf("h%d", params.Height)
	}
}
