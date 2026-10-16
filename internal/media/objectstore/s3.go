//go:build s3

package objectstore

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
)

type S3 struct {
	client                                    *s3.Client
	bucket, privateBucket, prefix, publicBase string
}

func newS3(mc config.Media, _ bool) (Store, error) {
	c := mc.S3
	if c.PrivateBucket != "" && c.PrivateBucket == c.Bucket {
		return nil, fmt.Errorf("private archive bucket must differ from public media bucket")
	}
	load := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(c.Region)}
	if c.AccessKey != "" || !c.UseDefaultCredentials {
		load = append(load, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, c.SessionToken)))
	}
	if c.Endpoint != "" {
		endpoint := c.Endpoint
		if c.Insecure && !strings.Contains(endpoint, "://") {
			endpoint = "http://" + endpoint
		}
		load = append(load, awsconfig.WithBaseEndpoint(endpoint))
	}
	ac, err := awsconfig.LoadDefaultConfig(context.Background(), load...)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(ac, func(o *s3.Options) { o.UsePathStyle = c.PathStyle })
	prefix := strings.Trim(c.Prefix, "/")
	base := strings.TrimRight(mc.PublicBaseURL, "/")
	if base == "" {
		if c.Endpoint != "" {
			base = strings.TrimRight(c.Endpoint, "/")
			if !strings.Contains(base, "://") {
				if c.Insecure {
					base = "http://" + base
				} else {
					base = "https://" + base
				}
			}
			if c.PathStyle {
				base += "/" + c.Bucket
			} else if u, parseErr := url.Parse(base); parseErr == nil {
				u.Host = c.Bucket + "." + u.Host
				base = strings.TrimRight(u.String(), "/")
			}
		} else {
			base = "https://" + c.Bucket + ".s3." + c.Region + ".amazonaws.com"
		}
	}
	return &S3{client: client, bucket: c.Bucket, privateBucket: c.PrivateBucket, prefix: prefix, publicBase: base}, nil
}
func (s *S3) Backend() string { return "s3" }
func (s *S3) ID() string      { return "s3:" + s.bucket + ":" + s.prefix + ":" + s.publicBase }
func (s *S3) key(key string) (string, string, error) {
	k, e := CleanKey(key)
	if e != nil {
		return "", "", e
	}
	bucket := s.bucket
	if privateKey(k) {
		if s.privateBucket == "" || s.privateBucket == s.bucket {
			return "", "", fmt.Errorf("account archives require media.s3.private_bucket with public access disabled")
		}
		bucket = s.privateBucket
	}
	return bucket, path.Join(s.prefix, k), nil
}
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, o PutOptions) (Object, error) {
	bucket, k, e := s.key(key)
	if e != nil {
		return Object{}, e
	}
	in := &s3.PutObjectInput{Bucket: &bucket, Key: &k, Body: r, ContentType: aws.String(o.ContentType), CacheControl: aws.String(o.CacheControl)}
	if size >= 0 {
		in.ContentLength = aws.Int64(size)
	}
	_, e = s.client.PutObject(ctx, in)
	if e != nil {
		return Object{}, e
	}
	return s.Stat(ctx, key)
}
func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, Object, error) {
	bucket, k, e := s.key(key)
	if e != nil {
		return nil, Object{}, e
	}
	out, e := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &k})
	if e != nil {
		return nil, Object{}, e
	}
	return out.Body, Object{Key: key, Size: aws.ToInt64(out.ContentLength), ContentType: aws.ToString(out.ContentType), ETag: strings.Trim(aws.ToString(out.ETag), "\"")}, nil
}
func (s *S3) Stat(ctx context.Context, key string) (Object, error) {
	bucket, k, e := s.key(key)
	if e != nil {
		return Object{}, e
	}
	o, e := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &k})
	if e != nil {
		return Object{}, e
	}
	var mt time.Time
	if o.LastModified != nil {
		mt = *o.LastModified
	}
	return Object{Key: key, Size: aws.ToInt64(o.ContentLength), ContentType: aws.ToString(o.ContentType), ETag: strings.Trim(aws.ToString(o.ETag), "\""), ModTime: mt}, nil
}
func (s *S3) Delete(ctx context.Context, key string) error {
	bucket, k, e := s.key(key)
	if e != nil {
		return e
	}
	_, e = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &k})
	return e
}
func (s *S3) List(ctx context.Context, prefix string, fn func(Object) error) error {
	bucket, k, e := s.key(prefix)
	if e != nil {
		return e
	}
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &k})
	for p.HasMorePages() {
		page, e := p.NextPage(ctx)
		if e != nil {
			return e
		}
		for _, v := range page.Contents {
			key := strings.TrimPrefix(aws.ToString(v.Key), strings.TrimRight(s.prefix, "/")+"/")
			if e = fn(Object{Key: key, Size: aws.ToInt64(v.Size), ETag: strings.Trim(aws.ToString(v.ETag), "\""), ModTime: aws.ToTime(v.LastModified)}); e != nil {
				return e
			}
		}
	}
	return nil
}
func (s *S3) URL(key string) string {
	bucket, k, e := s.key(key)
	if e != nil || bucket != s.bucket {
		return ""
	}
	return s.publicBase + "/" + strings.ReplaceAll(url.PathEscape(k), "%2F", "/")
}
func (s *S3) Check(ctx context.Context) error {
	_, e := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.bucket})
	if e != nil {
		return fmt.Errorf("head bucket %s: %w", s.bucket, e)
	}
	return nil
}
