// S3-backed implementation of Store.
//
// Each cache entry is laid out under a fixed prefix so the bucket
// can be shared with non-hpcc tooling without scan loops tripping
// on stray objects:
//
//	cache/<hh>/<full-hex-key>/<name>
//
// Operations are network-bound, so every S3 call carries an
// explicit per-call deadline (the disk-store interface predates
// context, so we synthesise contexts from struct-level timeouts
// instead of threading ctx through Get/Put/Has). Eviction is
// watermark-gated: an in-memory size estimate is bumped on every
// Put, and a full-bucket scan + LRU delete only runs once the
// estimate exceeds maxSize by 10%, amortising what would otherwise
// be one full bucket scan per cache write.

package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// objectPrefix namespaces all hpcc cache objects so the bucket can
// be shared with other tools (audit log uploads, lifecycle config,
// telemetry, etc.) without scanEntries tripping on their objects.
const objectPrefix = "cache/"

// Per-operation timeouts. Disk-store callers don't pass a context,
// so we synthesise these. Tuned to match the plan's §3.3 budget
// ("default: 2s reads, 5s writes") plus generous list/init slots
// since paginated bucket scans take longer than single-object ops.
const (
	defaultS3GetTimeout  = 2 * time.Second
	defaultS3PutTimeout  = 5 * time.Second
	defaultS3HasTimeout  = 2 * time.Second
	defaultS3ListTimeout = 30 * time.Second
	defaultS3InitTimeout = 10 * time.Second
)

// defaultS3MaxBlobBytes caps how much an S3 GET will buffer in
// memory before erroring. 1 GiB covers any realistic compile
// artifact (.o, debug info, LTO blob); a malicious or runaway
// upload past that would OOM the worker.
const defaultS3MaxBlobBytes int64 = 1 << 30

// evictHysteresisDenom gates eviction: only run a scan-and-delete
// once the in-memory size estimate exceeds maxSize + maxSize/N. With
// N=10 we tolerate 10% overshoot in exchange for one eviction per
// maxSize/10 of writes instead of one per write.
const evictHysteresisDenom int64 = 10

// s3DeleteBatchMax is the per-call cap S3 enforces on
// DeleteObjects. We chunk to stay under it for any plausible
// future bulk-delete path; per-entry deletes today only have a
// handful of keys but the helper keeps that assumption honest.
const s3DeleteBatchMax = 1000

// S3Options captures everything NewS3CacheStore needs. Most
// fields default to sane values when zero — only Bucket is
// strictly required; supply MaxSize=0 for "unlimited".
type S3Options struct {
	Bucket     string
	Region     string
	Endpoint   string
	AccessKey  string
	SecretKey  string
	MaxSize    int64
	AutoCreate bool
	// MaxBlobBytes optionally overrides defaultS3MaxBlobBytes.
	MaxBlobBytes int64
}

// S3CacheStore is a content-addressable S3-backed Store. Safe for
// concurrent use across goroutines; the eviction state is mutex-
// protected and only one eviction runs at a time process-wide.
type S3CacheStore struct {
	bucket  string
	client  *s3.Client
	maxSize int64

	getTimeout, putTimeout, hasTimeout, listTimeout time.Duration
	maxBlobBytes                                    int64

	// Watermark eviction state. estimateSize is a best-effort
	// running sum that's seeded by a one-time bucket scan at
	// init and bumped per Put. Eviction resets it to the
	// post-scan ground truth.
	sizeMu       sync.Mutex
	estimateSize int64

	// evicting serialises eviction across goroutines. CompareAndSwap
	// false→true gates the scan; if another Put concurrently sees
	// the watermark crossed, it skips and trusts the in-flight
	// eviction to bring size back in line.
	evicting atomic.Bool
}

// NewS3CacheStore creates an S3-backed cache from opts. See package
// doc for the on-disk layout and eviction model.
func NewS3CacheStore(opts S3Options) (*S3CacheStore, error) {
	if opts.Bucket == "" {
		return nil, errors.New("s3 cache: bucket is required")
	}

	var loadOpts []func(*config.LoadOptions) error
	if opts.Region != "" {
		loadOpts = append(loadOpts, config.WithRegion(opts.Region))
	}
	if opts.AccessKey != "" && opts.SecretKey != "" {
		loadOpts = append(loadOpts,
			config.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(
					opts.AccessKey,
					opts.SecretKey,
					"",
				),
			),
		)
	}

	awsCfg, err := config.LoadDefaultConfig(context.Background(), loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	// Custom endpoint = MinIO/Ceph/etc. Force path-style addressing
	// (`http://host/bucket/key`) since virtual-hosted style requires
	// DNS for the bucket name and most local setups don't have it.
	var client *s3.Client
	if opts.Endpoint != "" {
		client = s3.NewFromConfig(awsCfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(opts.Endpoint)
			o.UsePathStyle = true
		})
	} else {
		client = s3.NewFromConfig(awsCfg)
	}

	maxBlob := opts.MaxBlobBytes
	if maxBlob <= 0 {
		maxBlob = defaultS3MaxBlobBytes
	}

	s := &S3CacheStore{
		bucket:       opts.Bucket,
		client:       client,
		maxSize:      opts.MaxSize,
		getTimeout:   defaultS3GetTimeout,
		putTimeout:   defaultS3PutTimeout,
		hasTimeout:   defaultS3HasTimeout,
		listTimeout:  defaultS3ListTimeout,
		maxBlobBytes: maxBlob,
	}

	// Init smoke test: a small ListObjectsV2 against our prefix.
	// HeadBucket would also work but requires bucket-level IAM
	// (`s3:ListBucket` on the bucket resource); the prefix-scoped
	// list works with object-level perms a regulated deployment
	// is more likely to grant.
	initCtx, cancel := context.WithTimeout(context.Background(), defaultS3InitTimeout)
	defer cancel()
	_, err = s.client.ListObjectsV2(initCtx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(opts.Bucket),
		Prefix:  aws.String(objectPrefix),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		var nsb *types.NoSuchBucket
		if opts.AutoCreate && errors.As(err, &nsb) {
			createCtx, cancel := context.WithTimeout(context.Background(), defaultS3InitTimeout)
			defer cancel()
			if _, cerr := s.client.CreateBucket(createCtx, &s3.CreateBucketInput{
				Bucket: aws.String(opts.Bucket),
			}); cerr != nil {
				return nil, fmt.Errorf("s3 bucket %q: missing and auto_create failed: %w", opts.Bucket, cerr)
			}
		} else {
			return nil, fmt.Errorf("s3 bucket %q access check: %w", opts.Bucket, err)
		}
	}

	// Seed the size estimate from a one-time scan so a fresh worker
	// against an existing-and-overflowing bucket evicts on first
	// Put rather than waiting until it has Put maxSize bytes
	// itself. Best-effort: a failure here just means we start at
	// 0 and converge as Puts accumulate — annoying but not wrong.
	scanCtx, scanCancel := context.WithTimeout(context.Background(), s.listTimeout)
	defer scanCancel()
	if _, total, err := s.scanEntries(scanCtx); err == nil {
		s.estimateSize = total
	}

	return s, nil
}

func (s *S3CacheStore) Get(key []byte, name string) ([]byte, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.getTimeout)
	defer cancel()

	objKey := s.objectKey(key, name)
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objKey),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, nil
		}
		return nil, fmt.Errorf("get S3 object %q: %w", objKey, err)
	}
	defer resp.Body.Close()

	// Read with a hard cap. Asking for maxBlobBytes+1 lets us
	// distinguish "exactly at limit" from "exceeds limit".
	data, err := io.ReadAll(io.LimitReader(resp.Body, s.maxBlobBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read S3 object %q: %w", objKey, err)
	}
	if int64(len(data)) > s.maxBlobBytes {
		return nil, fmt.Errorf("s3 object %q exceeds max blob size of %d bytes", objKey, s.maxBlobBytes)
	}
	return data, nil
}

func (s *S3CacheStore) Put(key []byte, name string, value []byte) error {
	if err := validateName(name); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.putTimeout)
	defer cancel()

	objKey := s.objectKey(key, name)
	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objKey),
		Body:   bytes.NewReader(value),
	}); err != nil {
		return fmt.Errorf("put S3 object %q: %w", objKey, err)
	}

	if s.maxSize > 0 {
		s.bumpEstimate(int64(len(value)))
		if s.shouldEvict() && s.evicting.CompareAndSwap(false, true) {
			defer s.evicting.Store(false)
			// Synchronous on the watermark crossing — Put pays the
			// eviction cost once per ~maxSize/10 writes. Errors are
			// non-fatal: the cache may temporarily overshoot but the
			// next watermark crossing will retry.
			_ = s.evict()
		}
	}
	return nil
}

func (s *S3CacheStore) Has(key []byte) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.hasTimeout)
	defer cancel()

	resp, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(s.entryPrefix(key)),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, fmt.Errorf("list S3 objects: %w", err)
	}
	return len(resp.Contents) > 0, nil
}

func (s *S3CacheStore) Bucket() string { return s.bucket }

func (s *S3CacheStore) Stats() (entries int, totalSize int64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.listTimeout)
	defer cancel()
	ents, total, err := s.scanEntries(ctx)
	return len(ents), total, err
}

// Clean evicts cache entries to satisfy the given constraints. If
// maxSize is positive, LRU entries are removed until total size is
// at or below it. If maxAge is positive, entries whose newest blob
// is older than maxAge are removed regardless of size.
func (s *S3CacheStore) Clean(maxSize int64, maxAge time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.listTimeout)
	defer cancel()

	entries, totalSize, err := s.scanEntries(ctx)
	if err != nil {
		return err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime < entries[j].modTime
	})

	cutoff := int64(0)
	if maxAge > 0 {
		cutoff = time.Now().Add(-maxAge).UnixNano()
	}

	for _, e := range entries {
		byAge := cutoff > 0 && e.modTime < cutoff
		bySize := maxSize > 0 && totalSize > maxSize
		if !byAge && !bySize {
			break
		}
		// Tolerate per-entry delete failures (concurrent eviction
		// may have already removed it; that's NoSuchKey to S3 but
		// a no-op to us). Counter survives so we converge on the
		// budget across calls.
		if err := s.deleteEntry(ctx, e.key); err == nil {
			totalSize -= e.size
		}
	}
	s.resetEstimate(totalSize)
	return nil
}

// objectKey builds an S3 key under objectPrefix in the shape
// "<prefix><hh>/<full-hex>/<name>". The two-char shard prefix is
// historical (S3 used to partition by key prefix); modern S3 doesn't
// care, but the layout matches the disk store and keeps any external
// tooling that pivots on prefixes happy.
func (s *S3CacheStore) objectKey(key []byte, name string) string {
	h := hex.EncodeToString(key)
	if len(h) < 2 {
		return fmt.Sprintf("%s%s/%s", objectPrefix, h, name)
	}
	return fmt.Sprintf("%s%s/%s/%s", objectPrefix, h[:2], h, name)
}

// entryPrefix returns the listable prefix for all blobs of an entry.
func (s *S3CacheStore) entryPrefix(key []byte) string {
	h := hex.EncodeToString(key)
	if len(h) < 2 {
		return fmt.Sprintf("%s%s/", objectPrefix, h)
	}
	return fmt.Sprintf("%s%s/%s/", objectPrefix, h[:2], h)
}

// parseObjectKey decodes a key produced by objectKey back into the
// cache key bytes. Returns ok=false for any shape mismatch — strays,
// non-cache objects under another prefix, manually-uploaded files —
// so scan loops can skip them without erroring.
func parseObjectKey(s string) (cacheKey []byte, ok bool) {
	if !strings.HasPrefix(s, objectPrefix) {
		return nil, false
	}
	rest := strings.TrimPrefix(s, objectPrefix)
	parts := strings.SplitN(rest, "/", 3)
	// Long form: "<hh>/<full-hex>/<name>"
	// Short form: "<full-hex>/<name>" (only for keys < 1 byte; not
	// produced today but tolerate for forward compat).
	var hexKey string
	switch len(parts) {
	case 3:
		if len(parts[0]) != 2 {
			return nil, false
		}
		hexKey = parts[1]
	case 2:
		hexKey = parts[0]
	default:
		return nil, false
	}
	k, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, false
	}
	return k, true
}

// s3EntryInfo summarises one cache entry for eviction accounting.
type s3EntryInfo struct {
	key     []byte
	size    int64
	modTime int64 // unix nanos of the entry's newest blob
}

// evict scans the bucket and deletes the oldest entries until the
// total drops below maxSize. Held under the evicting flag so only
// one eviction runs at a time across the process; concurrent Puts
// just bump the estimate and trust this pass.
func (s *S3CacheStore) evict() error {
	ctx, cancel := context.WithTimeout(context.Background(), s.listTimeout)
	defer cancel()

	entries, totalSize, err := s.scanEntries(ctx)
	if err != nil {
		return err
	}
	defer s.resetEstimate(totalSize)

	if totalSize <= s.maxSize {
		return nil
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime < entries[j].modTime
	})

	for _, e := range entries {
		if totalSize <= s.maxSize {
			break
		}
		// Concurrent Clean/eviction across workers could race on the
		// same entry. NoSuchKey on the second delete is harmless;
		// any error here just means we'll retry on the next pass.
		if err := s.deleteEntry(ctx, e.key); err == nil {
			totalSize -= e.size
		}
	}
	return nil
}

// scanEntries paginates every object under objectPrefix and rolls
// them up into per-entry size + modtime. Bucket-share-safe via the
// prefix filter; non-cache objects elsewhere in the bucket never
// hit this scan.
func (s *S3CacheStore) scanEntries(ctx context.Context) ([]s3EntryInfo, int64, error) {
	entryMap := make(map[string]*s3EntryInfo)

	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(objectPrefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, 0, fmt.Errorf("list S3 objects: %w", err)
		}
		for _, obj := range page.Contents {
			k, ok := parseObjectKey(aws.ToString(obj.Key))
			if !ok {
				continue
			}
			e, exists := entryMap[string(k)]
			if !exists {
				e = &s3EntryInfo{key: k}
				entryMap[string(k)] = e
			}
			e.size += aws.ToInt64(obj.Size)
			if obj.LastModified != nil {
				if t := obj.LastModified.UnixNano(); t > e.modTime {
					e.modTime = t
				}
			}
		}
	}

	entries := make([]s3EntryInfo, 0, len(entryMap))
	var total int64
	for _, e := range entryMap {
		entries = append(entries, *e)
		total += e.size
	}
	return entries, total, nil
}

// deleteEntry removes every blob under an entry's prefix. Chunks
// the DeleteObjects calls so we never exceed the per-call cap of
// s3DeleteBatchMax keys.
func (s *S3CacheStore) deleteEntry(ctx context.Context, key []byte) error {
	prefix := s.entryPrefix(key)

	resp, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return fmt.Errorf("list objects under %q: %w", prefix, err)
	}

	ids := make([]types.ObjectIdentifier, 0, len(resp.Contents))
	for _, obj := range resp.Contents {
		ids = append(ids, types.ObjectIdentifier{Key: obj.Key})
	}
	for _, batch := range chunkObjectIdentifiers(ids, s3DeleteBatchMax) {
		if _, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: batch},
		}); err != nil {
			return fmt.Errorf("delete S3 objects: %w", err)
		}
	}
	return nil
}

// chunkObjectIdentifiers slices ids into batches of at most n
// elements. Returns nil when ids is empty so callers can `for range`
// without a zero-batch DeleteObjects call.
func chunkObjectIdentifiers(ids []types.ObjectIdentifier, n int) [][]types.ObjectIdentifier {
	if len(ids) == 0 {
		return nil
	}
	out := make([][]types.ObjectIdentifier, 0, (len(ids)+n-1)/n)
	for i := 0; i < len(ids); i += n {
		end := i + n
		if end > len(ids) {
			end = len(ids)
		}
		out = append(out, ids[i:end])
	}
	return out
}

func (s *S3CacheStore) bumpEstimate(delta int64) {
	s.sizeMu.Lock()
	s.estimateSize += delta
	s.sizeMu.Unlock()
}

func (s *S3CacheStore) resetEstimate(actual int64) {
	s.sizeMu.Lock()
	s.estimateSize = actual
	s.sizeMu.Unlock()
}

// shouldEvict returns true once the running size estimate exceeds
// maxSize + maxSize/evictHysteresisDenom. Holding the read+compare
// under the mutex means a concurrent bumpEstimate either lands
// before our read (we see the new total) or after (next caller
// picks it up) — either way we don't double-count.
func (s *S3CacheStore) shouldEvict() bool {
	if s.maxSize <= 0 {
		return false
	}
	threshold := s.maxSize + s.maxSize/evictHysteresisDenom
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	return s.estimateSize > threshold
}

func (s *S3CacheStore) Dir() string { return fmt.Sprintf("s3://%s", s.bucket) }

// Compile-time assertion that S3CacheStore satisfies Store. Catches
// signature drift in either side at build time rather than the
// first runtime use.
var _ Store = (*S3CacheStore)(nil)
