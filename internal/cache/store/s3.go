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
	"time"

	hpccconfig "github.com/aarani/hpcc/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3CacheStore is a content-addressable S3-based store. Each key maps
// to objects with keys `<hh>/<full-hex-key>/<name>`.
type S3CacheStore struct {
	bucket  string
	client  *s3.Client
	maxSize int64 // bytes; 0 means unlimited
}

// NewS3CacheStore creates an S3-backed cache. If endpoint is non-empty,
// it is used as a custom S3 endpoint (for MinIO or S3-compatible services).
// Provide region for signing when using a custom endpoint; region may be
// empty when using the default AWS endpoints.
func NewS3CacheStore(bucket, maxSize, region, endpoint, accessKey, secretKey string) (*S3CacheStore, error) {
	sz, err := hpccconfig.ParseSize(maxSize)
	if err != nil {
		return nil, fmt.Errorf("s3 cache max_size: %w", err)
	}

	var loadOpts []func(*config.LoadOptions) error
	if region != "" {
		loadOpts = append(loadOpts, config.WithRegion(region))
	}

	if accessKey != "" && secretKey != "" {
		loadOpts = append(loadOpts,
			config.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(
					accessKey,
					secretKey,
					"",
				),
			),
		)
	}

	cfg, err := config.LoadDefaultConfig(context.TODO(), loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	// If using a custom endpoint (e.g. MinIO), enable path-style addressing.
	var client *s3.Client
	if endpoint != "" {
		client = s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		})
	} else {
		client = s3.NewFromConfig(cfg)
	}

	s := &S3CacheStore{bucket: bucket, client: client, maxSize: sz}

	// Validate we can access the bucket early to surface config/credential errors.
	// If the bucket does not exist and we can create it (common for local
	// MinIO testing), attempt to create it. If creation fails, return the
	// original error to help users diagnose permissions/config issues.
	if _, err := s.client.HeadBucket(context.TODO(), &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		// Try to create the bucket as a best-effort for local/test setups.
		_, createErr := s.client.CreateBucket(context.TODO(), &s3.CreateBucketInput{Bucket: aws.String(bucket)})
		if createErr != nil {
			return nil, fmt.Errorf("s3 bucket access check: %w (create attempt: %v)", err, createErr)
		}
		// If create succeeded, optionally wait until it exists; proceed.
	}

	return s, nil
}

func (s *S3CacheStore) Get(key []byte, name string) ([]byte, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	objKey := s.objectKey(key, name)
	resp, err := s.client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objKey),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, nil
		}
		return nil, fmt.Errorf("get S3 object: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read S3 object body: %w", err)
	}
	return data, nil
}

func (s *S3CacheStore) Put(key []byte, name string, value []byte) error {
	if err := validateName(name); err != nil {
		return err
	}
	objKey := s.objectKey(key, name)
	_, err := s.client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(objKey),
		Body:   bytes.NewReader(value),
	})
	if err != nil {
		return fmt.Errorf("put S3 object: %w", err)
	}
	if s.maxSize > 0 {
		return s.evict()
	}
	return nil
}

func (s *S3CacheStore) Has(key []byte) (bool, error) {
	objKey := s.entryPrefix(key)
	// Check if any object exists under the prefix
	resp, err := s.client.ListObjectsV2(context.TODO(), &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(objKey),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, fmt.Errorf("list S3 objects: %w", err)
	}
	return len(resp.Contents) > 0, nil
}

// Bucket returns the bucket name of this cache store.
func (s *S3CacheStore) Bucket() string { return s.bucket }

// Stats returns the number of cache entries and their total size in bytes.
func (s *S3CacheStore) Stats() (entries int, totalSize int64, err error) {
	ents, total, err := s.scanEntries()
	return len(ents), total, err
}

// Clean evicts cache entries to satisfy the given constraints. If maxSize
// is positive, LRU entries are removed until total size is at or below it.
// If maxAge is positive, entries whose newest blob is older than maxAge are
// removed regardless of size. Both constraints can be applied in one call.
func (s *S3CacheStore) Clean(maxSize int64, maxAge time.Duration) error {
	entries, totalSize, err := s.scanEntries()
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
		if err := s.deleteEntry(e.key); err != nil {
			continue
		}
		totalSize -= e.size
	}
	return nil
}

// objectKey returns the S3 object key for the given key and name: <hh>/<full-hex-key>/<name>
func (s *S3CacheStore) objectKey(key []byte, name string) string {
	h := hex.EncodeToString(key)
	if len(h) >= 2 {
		return fmt.Sprintf("%s/%s/%s", h[:2], h, name)
	}
	return fmt.Sprintf("%s/%s", h, name)
}

// entryPrefix returns the prefix for all objects of an entry: <hh>/<full-hex-key>/
func (s *S3CacheStore) entryPrefix(key []byte) string {
	h := hex.EncodeToString(key)
	if len(h) >= 2 {
		return fmt.Sprintf("%s/%s/", h[:2], h)
	}
	return fmt.Sprintf("%s/", h)
}

// s3EntryInfo describes a single cache entry for eviction.
type s3EntryInfo struct {
	key     []byte
	size    int64
	modTime int64
}

func (s *S3CacheStore) evict() error {
	entries, totalSize, err := s.scanEntries()
	if err != nil {
		return err
	}
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
		if err := s.deleteEntry(e.key); err != nil {
			continue
		}
		totalSize -= e.size
	}
	return nil
}

// scanEntries lists all entries and their stats.
func (s *S3CacheStore) scanEntries() ([]s3EntryInfo, int64, error) {
	var entries []s3EntryInfo
	var total int64
	entryMap := make(map[string]*s3EntryInfo)

	// List all objects
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.TODO())
		if err != nil {
			return nil, 0, fmt.Errorf("list S3 objects: %w", err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			// Parse key to get entry key
			parts := strings.Split(key, "/")
			if len(parts) < 2 {
				continue
			}
			var entryKey string
			if len(parts) >= 3 {
				entryKey = parts[1]
			} else {
				entryKey = parts[0]
			}
			// Decode hex
			k, err := hex.DecodeString(entryKey)
			if err != nil {
				continue
			}
			mapKey := string(k)
			if entryMap[mapKey] == nil {
				entryMap[mapKey] = &s3EntryInfo{key: k}
			}
			entryMap[mapKey].size += aws.ToInt64(obj.Size)
			if t := obj.LastModified.UnixNano(); t > entryMap[mapKey].modTime {
				entryMap[mapKey].modTime = t
			}
		}
	}

	for _, e := range entryMap {
		entries = append(entries, *e)
		total += e.size
	}
	return entries, total, nil
}

func (s *S3CacheStore) deleteEntry(key []byte) error {
	prefix := s.entryPrefix(key)
	// List and delete all objects under prefix
	resp, err := s.client.ListObjectsV2(context.TODO(), &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return fmt.Errorf("list objects for deletion: %w", err)
	}
	var objects []types.ObjectIdentifier
	for _, obj := range resp.Contents {
		objects = append(objects, types.ObjectIdentifier{Key: obj.Key})
	}
	if len(objects) > 0 {
		_, err = s.client.DeleteObjects(context.TODO(), &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: objects},
		})
		if err != nil {
			return fmt.Errorf("delete S3 objects: %w", err)
		}
	}
	return nil
}

// Dir returns the root location of this cache store for display purposes.
func (s *S3CacheStore) Dir() string { return fmt.Sprintf("s3://%s", s.bucket) }
