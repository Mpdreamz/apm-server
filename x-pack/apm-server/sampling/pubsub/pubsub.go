// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package pubsub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/pkg/errors"
	"go.elastic.co/fastjson"
	"golang.org/x/sync/errgroup"

	"github.com/elastic/beats/v7/libbeat/logp"
	"github.com/elastic/go-elasticsearch/v7/esapi"
	"github.com/elastic/go-elasticsearch/v7/esutil"

	"github.com/elastic/apm-server/elasticsearch"
	logs "github.com/elastic/apm-server/log"
)

var errIndexNotFound = errors.New("index not found")

// Pubsub provides a means of publishing and subscribing to sampled trace IDs,
// using Elasticsearch for temporary storage.
//
// An independent process will periodically reap old documents in the index.
type Pubsub struct {
	config  Config
	indexer elasticsearch.BulkIndexer
}

// New returns a new Pubsub which can publish and subscribe sampled trace IDs,
// using Elasticsearch for storage.
//
// Documents are expected to be indexed through a pipeline which sets the
// `event.ingested` timestamp field. Another process will periodically reap
// events older than a configured age.
func New(config Config) (*Pubsub, error) {
	if err := config.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid pubsub config")
	}
	if config.Logger == nil {
		config.Logger = logp.NewLogger(logs.Sampling)
	}
	indexer, err := config.Client.NewBulkIndexer(elasticsearch.BulkIndexerConfig{
		Index:         config.DataStream.String(),
		FlushInterval: config.FlushInterval,
		OnError: func(ctx context.Context, err error) {
			config.Logger.With(logp.Error(err)).Debug("publishing sampled trace IDs failed")
		},
	})
	if err != nil {
		return nil, err
	}
	return &Pubsub{
		config:  config,
		indexer: indexer,
	}, nil
}

// PublishSampledTraceIDs bulk indexes traceIDs into Elasticsearch.
func (p *Pubsub) PublishSampledTraceIDs(ctx context.Context, traceID ...string) error {
	now := time.Now()
	for _, id := range traceID {
		var json fastjson.Writer
		p.marshalTraceIDDocument(&json, id, now, p.config.DataStream)
		if err := p.indexer.Add(ctx, elasticsearch.BulkIndexerItem{
			Action:    "create",
			Body:      bytes.NewReader(json.Bytes()),
			OnFailure: p.onBulkIndexerItemFailure,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pubsub) onBulkIndexerItemFailure(ctx context.Context, item elasticsearch.BulkIndexerItem, resp elasticsearch.BulkIndexerResponseItem, err error) {
	p.config.Logger.With(logp.Error(err)).Debug("publishing sampled trace ID failed", resp.Error)
}

// SubscribeSampledTraceIDs subscribes to new sampled trace IDs, sending them to the
// traceIDs channel.
func (p *Pubsub) SubscribeSampledTraceIDs(ctx context.Context, traceIDs chan<- string) error {
	ticker := time.NewTicker(p.config.SearchInterval)
	defer ticker.Stop()

	observedSeqnos := make(map[string]int64)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if err := p.searchTraceIDs(ctx, traceIDs, observedSeqnos); err != nil {
			// Errors may occur due to rate limiting, or while the index is
			// still being created, so just log and continue.
			p.config.Logger.With(logp.Error(err)).Debug("error searching for trace IDs")
		}
	}
}

// searchTraceIDs searches the configured data stream for new sampled trace IDs, sending them to the out channel.
//
// searchTraceIDs works by fetching the global checkpoint for each index backing the data stream, and comparing
// this to the most recently observed sequence number for the indices. If the global checkpoint is greater, then
// we search through every document with a sequence number greater than the most recently observed, and less than
// or equal to the global checkpoint.
//
// Immediately after observing an updated global checkpoint we will force-refresh indices to ensure all documents
// up to the global checkpoint are visible in proceeding searches.
func (p *Pubsub) searchTraceIDs(ctx context.Context, out chan<- string, observedSeqnos map[string]int64) error {
	globalCheckpoints, err := getGlobalCheckpoints(ctx, p.config.Client, p.config.DataStream.String())
	if err != nil {
		return err
	}

	// Remove old indices from the observed _seq_no map.
	for index := range observedSeqnos {
		if _, ok := globalCheckpoints[index]; !ok {
			delete(observedSeqnos, index)
		}
	}

	// Force-refresh the indices with updated global checkpoints.
	indices := make([]string, 0, len(globalCheckpoints))
	for index, globalCheckpoint := range globalCheckpoints {
		observedSeqno, ok := observedSeqnos[index]
		if ok && globalCheckpoint <= observedSeqno {
			delete(globalCheckpoints, index)
			continue
		}
		indices = append(indices, index)
	}
	if err := p.refreshIndices(ctx, indices); err != nil {
		return err
	}

	g, ctx := errgroup.WithContext(ctx)
	for _, index := range indices {
		globalCheckpoint := globalCheckpoints[index]
		observedSeqno, ok := observedSeqnos[index]
		if !ok {
			observedSeqno = -1
		}
		index := index // copy for closure
		g.Go(func() error {
			maxSeqno, err := p.searchIndexTraceIDs(ctx, out, index, observedSeqno, globalCheckpoint)
			if err != nil {
				return err
			}
			if maxSeqno > observedSeqno {
				observedSeqno = maxSeqno
			}
			observedSeqnos[index] = observedSeqno
			return nil
		})
	}
	return g.Wait()
}

func (p *Pubsub) refreshIndices(ctx context.Context, indices []string) error {
	if len(indices) == 0 {
		return nil
	}
	ignoreUnavailable := true
	resp, err := esapi.IndicesRefreshRequest{
		Index:             indices,
		IgnoreUnavailable: &ignoreUnavailable,
	}.Do(ctx, p.config.Client)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.IsError() {
		message, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("index refresh request failed: %s", message)
	}
	return nil
}

// searchIndexTraceIDs searches index sampled trace IDs, whose documents have a _seq_no
// greater than minSeqno and less than or equal to maxSeqno, and returns the greatest
// observed _seq_no. Sampled trace IDs are sent to out.
func (p *Pubsub) searchIndexTraceIDs(ctx context.Context, out chan<- string, index string, minSeqno, maxSeqno int64) (int64, error) {
	var maxObservedSeqno int64 = -1
	for maxObservedSeqno < maxSeqno {
		// Include only documents after the old global checkpoint,
		// and up to and including the new global checkpoint.
		filters := []map[string]interface{}{{
			"range": map[string]interface{}{
				"_seq_no": map[string]interface{}{
					"lte": maxSeqno,
				},
			},
		}}
		if minSeqno >= 0 {
			filters = append(filters, map[string]interface{}{
				"range": map[string]interface{}{
					"_seq_no": map[string]interface{}{
						"gt": minSeqno,
					},
				},
			})
		}

		searchBody := map[string]interface{}{
			"size":                1000,
			"sort":                []interface{}{map[string]interface{}{"_seq_no": "asc"}},
			"seq_no_primary_term": true,
			"track_total_hits":    false,
			"query": map[string]interface{}{
				"bool": map[string]interface{}{
					// Filter out local observations.
					"must_not": map[string]interface{}{
						"term": map[string]interface{}{
							"observer.id": map[string]interface{}{
								"value": p.config.BeatID,
							},
						},
					},
					"filter": filters,
				},
			},
		}

		var result struct {
			Hits struct {
				Hits []struct {
					Seqno  int64           `json:"_seq_no"`
					Source traceIDDocument `json:"_source"`
					Sort   []interface{}   `json:"sort"`
				}
			}
		}
		if err := p.doSearchRequest(ctx, index, esutil.NewJSONReader(searchBody), &result); err != nil {
			if err == errIndexNotFound {
				// Index was deleted.
				break
			}
			return -1, err
		}
		if len(result.Hits.Hits) == 0 {
			break
		}
		for _, hit := range result.Hits.Hits {
			select {
			case <-ctx.Done():
				return -1, ctx.Err()
			case out <- hit.Source.Trace.ID:
			}
		}
		maxObservedSeqno = result.Hits.Hits[len(result.Hits.Hits)-1].Seqno
	}
	return maxObservedSeqno, nil
}

func (p *Pubsub) doSearchRequest(ctx context.Context, index string, body io.Reader, out interface{}) error {
	resp, err := esapi.SearchRequest{
		Index: []string{index},
		Body:  body,
	}.Do(ctx, p.config.Client)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.IsError() {
		if resp.StatusCode == http.StatusNotFound {
			return errIndexNotFound
		}
		message, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("search request failed: %s", message)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (p *Pubsub) marshalTraceIDDocument(w *fastjson.Writer, traceID string, timestamp time.Time, dataStream DataStreamConfig) {
	w.RawString(`{"@timestamp":"`)
	w.Time(timestamp.UTC(), time.RFC3339Nano)
	w.RawString(`","data_stream.type":`)
	w.String(dataStream.Type)
	w.RawString(`,"data_stream.dataset":`)
	w.String(dataStream.Dataset)
	w.RawString(`,"data_stream.namespace":`)
	w.String(dataStream.Namespace)
	w.RawString(`,"observer":{"id":`)
	w.String(p.config.BeatID)
	w.RawString(`},`)
	w.RawString(`"trace":{"id":`)
	w.String(traceID)
	w.RawString(`}}`)
}

type traceIDDocument struct {
	// Observer identifies the entity (typically an APM Server) that observed
	// and indexed the/ trace ID document. This can be used to filter out local
	// observations.
	Observer struct {
		// ID holds the unique ID of the observer.
		ID string `json:"id"`
	} `json:"observer"`

	// Trace identifies a trace.
	Trace struct {
		// ID holds the unique ID of the trace.
		ID string `json:"id"`
	} `json:"trace"`
}
