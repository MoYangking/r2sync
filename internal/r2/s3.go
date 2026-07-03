package r2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const multipartThreshold = int64(4 * 1024 * 1024 * 1024)
const multipartPartSize = int64(64 * 1024 * 1024)

type S3Store struct {
	client *s3.Client
	bucket string
}

func NewS3Store(accountID, bucket, accessKeyID, secretAccessKey string) *S3Store {
	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID)
	cfg := aws.Config{
		Region:      "auto",
		Credentials: credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, ""),
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	return &S3Store{client: client, bucket: bucket}
}

func (s *S3Store) HeadBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err != nil {
		return fmt.Errorf("head R2 bucket %q: %w", s.bucket, err)
	}
	return nil
}

func (s *S3Store) Head(ctx context.Context, key string) (Object, bool, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return Object{Key: key, Exists: false}, false, nil
		}
		return Object{}, false, fmt.Errorf("head R2 object %s: %w", key, err)
	}
	obj := Object{
		Key:          key,
		Exists:       true,
		Size:         aws.ToInt64(out.ContentLength),
		ETag:         strings.Trim(aws.ToString(out.ETag), "\""),
		Metadata:     out.Metadata,
		LastModified: aws.ToTime(out.LastModified),
	}
	obj.SHA256 = metadataValue(out.Metadata, "r2sync-sha256")
	return obj, true, nil
}

func (s *S3Store) Download(ctx context.Context, key string, w io.Writer) (Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return Object{}, fmt.Errorf("download R2 object %s: %w", key, err)
	}
	defer out.Body.Close()
	if _, err := io.Copy(w, out.Body); err != nil {
		return Object{}, fmt.Errorf("write downloaded object %s: %w", key, err)
	}
	obj := Object{
		Key:          key,
		Exists:       true,
		Size:         aws.ToInt64(out.ContentLength),
		ETag:         strings.Trim(aws.ToString(out.ETag), "\""),
		Metadata:     out.Metadata,
		LastModified: aws.ToTime(out.LastModified),
	}
	obj.SHA256 = metadataValue(out.Metadata, "r2sync-sha256")
	return obj, nil
}

func (s *S3Store) Upload(ctx context.Context, key string, filePath string, metadata map[string]string) (Object, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return Object{}, fmt.Errorf("stat upload file %s: %w", filePath, err)
	}
	if info.Size() >= multipartThreshold {
		return s.uploadMultipart(ctx, key, filePath, info.Size(), metadata)
	}
	f, err := os.Open(filePath)
	if err != nil {
		return Object{}, fmt.Errorf("open upload file %s: %w", filePath, err)
	}
	defer f.Close()
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          f,
		ContentLength: aws.Int64(info.Size()),
		Metadata:      metadata,
		StorageClass:  types.StorageClassStandard,
	})
	if err != nil {
		return Object{}, fmt.Errorf("upload R2 object %s: %w", key, err)
	}
	obj := Object{
		Key:      key,
		Exists:   true,
		Size:     info.Size(),
		ETag:     strings.Trim(aws.ToString(out.ETag), "\""),
		Metadata: metadata,
		SHA256:   metadataValue(metadata, "r2sync-sha256"),
	}
	return obj, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete R2 object %s: %w", key, err)
	}
	return nil
}

func (s *S3Store) uploadMultipart(ctx context.Context, key, filePath string, size int64, metadata map[string]string) (Object, error) {
	create, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(key),
		Metadata:     metadata,
		StorageClass: types.StorageClassStandard,
	})
	if err != nil {
		return Object{}, fmt.Errorf("create multipart upload for %s: %w", key, err)
	}
	uploadID := aws.ToString(create.UploadId)
	f, err := os.Open(filePath)
	if err != nil {
		_ = s.abortMultipart(ctx, key, uploadID)
		return Object{}, fmt.Errorf("open multipart upload file %s: %w", filePath, err)
	}
	defer f.Close()

	var completed []types.CompletedPart
	partNumber := int32(1)
	buf := make([]byte, int(multipartPartSize))
	for {
		n, readErr := io.ReadFull(f, buf)
		if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
			_ = s.abortMultipart(ctx, key, uploadID)
			return Object{}, fmt.Errorf("read multipart part: %w", readErr)
		}
		if n == 0 {
			break
		}
		part, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(key),
			UploadId:      aws.String(uploadID),
			PartNumber:    aws.Int32(partNumber),
			Body:          bytes.NewReader(buf[:n]),
			ContentLength: aws.Int64(int64(n)),
		})
		if err != nil {
			_ = s.abortMultipart(ctx, key, uploadID)
			return Object{}, fmt.Errorf("upload multipart part %d for %s: %w", partNumber, key, err)
		}
		completed = append(completed, types.CompletedPart{
			ETag:       part.ETag,
			PartNumber: aws.Int32(partNumber),
		})
		partNumber++
		if errors.Is(readErr, io.ErrUnexpectedEOF) || errors.Is(readErr, io.EOF) {
			break
		}
	}
	complete, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completed,
		},
	})
	if err != nil {
		_ = s.abortMultipart(ctx, key, uploadID)
		return Object{}, fmt.Errorf("complete multipart upload for %s: %w", key, err)
	}
	return Object{
		Key:      key,
		Exists:   true,
		Size:     size,
		ETag:     strings.Trim(aws.ToString(complete.ETag), "\""),
		Metadata: metadata,
		SHA256:   metadataValue(metadata, "r2sync-sha256"),
	}, nil
}

func (s *S3Store) abortMultipart(ctx context.Context, key, uploadID string) error {
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	return err
}

func isNotFound(err error) bool {
	var notFound *types.NotFound
	return errors.As(err, &notFound)
}

func metadataValue(metadata map[string]string, key string) string {
	if metadata == nil {
		return ""
	}
	for k, v := range metadata {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}
