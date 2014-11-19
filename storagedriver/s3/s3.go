package s3

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/crowdmob/goamz/aws"
	"github.com/crowdmob/goamz/s3"
	"github.com/docker/docker-registry/storagedriver"
	"github.com/docker/docker-registry/storagedriver/factory"
)

const driverName = "s3"

// minChunkSize defines the minimum multipart upload chunk size
// S3 API requires multipart upload chunks to be at least 5MB
const minChunkSize = uint64(5 * 1024 * 1024)

// listPartsMax is the largest amount of parts you can request from S3
const listPartsMax = 1000

func init() {
	factory.Register(driverName, &s3DriverFactory{})
}

// s3DriverFactory implements the factory.StorageDriverFactory interface
type s3DriverFactory struct{}

func (factory *s3DriverFactory) Create(parameters map[string]string) (storagedriver.StorageDriver, error) {
	return FromParameters(parameters)
}

// Driver is a storagedriver.StorageDriver implementation backed by Amazon S3
// Objects are stored at absolute keys in the provided bucket
type Driver struct {
	S3      *s3.S3
	Bucket  *s3.Bucket
	Encrypt bool
}

// FromParameters constructs a new Driver with a given parameters map
// Required parameters:
// - accesskey
// - secretkey
// - region
// - bucket
// - encrypt
func FromParameters(parameters map[string]string) (*Driver, error) {
	accessKey, ok := parameters["accesskey"]
	if !ok || accessKey == "" {
		return nil, fmt.Errorf("No accesskey parameter provided")
	}

	secretKey, ok := parameters["secretkey"]
	if !ok || secretKey == "" {
		return nil, fmt.Errorf("No secretkey parameter provided")
	}

	regionName, ok := parameters["region"]
	if !ok || regionName == "" {
		return nil, fmt.Errorf("No region parameter provided")
	}
	region := aws.GetRegion(regionName)
	if region.Name == "" {
		return nil, fmt.Errorf("Invalid region provided: %v", region)
	}

	bucket, ok := parameters["bucket"]
	if !ok || bucket == "" {
		return nil, fmt.Errorf("No bucket parameter provided")
	}

	encrypt, ok := parameters["encrypt"]
	if !ok {
		return nil, fmt.Errorf("No encrypt parameter provided")
	}

	encryptBool, err := strconv.ParseBool(encrypt)
	if err != nil {
		return nil, fmt.Errorf("Unable to parse the encrypt parameter: %v", err)
	}
	return New(accessKey, secretKey, region, encryptBool, bucket)
}

// New constructs a new Driver with the given AWS credentials, region, encryption flag, and
// bucketName
func New(accessKey string, secretKey string, region aws.Region, encrypt bool, bucketName string) (*Driver, error) {
	auth := aws.Auth{AccessKey: accessKey, SecretKey: secretKey}
	s3obj := s3.New(auth, region)
	bucket := s3obj.Bucket(bucketName)

	if err := bucket.PutBucket(getPermissions()); err != nil {
		s3Err, ok := err.(*s3.Error)
		if !(ok && s3Err.Code == "BucketAlreadyOwnedByYou") {
			return nil, err
		}
	}

	return &Driver{s3obj, bucket, encrypt}, nil
}

// Implement the storagedriver.StorageDriver interface

// GetContent retrieves the content stored at "path" as a []byte.
func (d *Driver) GetContent(path string) ([]byte, error) {
	content, err := d.Bucket.Get(path)
	if err != nil {
		return nil, storagedriver.PathNotFoundError{Path: path}
	}
	return content, nil
}

// PutContent stores the []byte content at a location designated by "path".
func (d *Driver) PutContent(path string, contents []byte) error {
	return d.Bucket.Put(path, contents, d.getContentType(), getPermissions(), d.getOptions())
}

// ReadStream retrieves an io.ReadCloser for the content stored at "path" with a
// given byte offset.
func (d *Driver) ReadStream(path string, offset uint64) (io.ReadCloser, error) {
	headers := make(http.Header)
	headers.Add("Range", "bytes="+strconv.FormatUint(offset, 10)+"-")

	resp, err := d.Bucket.GetResponseWithHeaders(path, headers)
	if err != nil {
		return nil, storagedriver.PathNotFoundError{Path: path}
	}
	return resp.Body, nil
}

// WriteStream stores the contents of the provided io.ReadCloser at a location
// designated by the given path.
func (d *Driver) WriteStream(path string, offset, size uint64, reader io.ReadCloser) error {
	defer reader.Close()

	chunkSize := minChunkSize
	for size/chunkSize >= listPartsMax {
		chunkSize *= 2
	}

	partNumber := 1
	totalRead := uint64(0)
	multi, parts, err := d.getAllParts(path)
	if err != nil {
		return err
	}

	if (offset) > uint64(len(parts))*chunkSize || (offset < size && offset%chunkSize != 0) {
		return storagedriver.InvalidOffsetError{Path: path, Offset: offset}
	}

	if len(parts) > 0 {
		partNumber = int(offset/chunkSize) + 1
		totalRead = offset
		parts = parts[0 : partNumber-1]
	}

	buf := make([]byte, chunkSize)
	for {
		bytesRead, err := io.ReadFull(reader, buf)
		totalRead += uint64(bytesRead)

		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return err
		} else if (uint64(bytesRead) < chunkSize) && totalRead != size {
			break
		} else {
			part, err := multi.PutPart(int(partNumber), bytes.NewReader(buf[0:bytesRead]))
			if err != nil {
				return err
			}

			parts = append(parts, part)
			if totalRead == size {
				multi.Complete(parts)
				break
			}

			partNumber++
		}
	}

	return nil
}

// CurrentSize retrieves the curernt size in bytes of the object at the given
// path.
func (d *Driver) CurrentSize(path string) (uint64, error) {
	_, parts, err := d.getAllParts(path)
	if err != nil {
		return 0, err
	}

	if len(parts) == 0 {
		return 0, nil
	}

	return (((uint64(len(parts)) - 1) * uint64(parts[0].Size)) + uint64(parts[len(parts)-1].Size)), nil
}

// List returns a list of the objects that are direct descendants of the given
// path.
func (d *Driver) List(path string) ([]string, error) {
	if path[len(path)-1] != '/' {
		path = path + "/"
	}
	listResponse, err := d.Bucket.List(path, "/", "", listPartsMax)
	if err != nil {
		return nil, err
	}

	files := []string{}
	directories := []string{}

	for {
		for _, key := range listResponse.Contents {
			files = append(files, key.Key)
		}

		for _, commonPrefix := range listResponse.CommonPrefixes {
			directories = append(directories, commonPrefix[0:len(commonPrefix)-1])
		}

		if listResponse.IsTruncated {
			listResponse, err = d.Bucket.List(path, "/", listResponse.NextMarker, listPartsMax)
			if err != nil {
				return nil, err
			}
		} else {
			break
		}
	}

	return append(files, directories...), nil
}

// Move moves an object stored at sourcePath to destPath, removing the original
// object.
func (d *Driver) Move(sourcePath string, destPath string) error {
	/* This is terrible, but aws doesn't have an actual move. */
	_, err := d.Bucket.PutCopy(destPath, getPermissions(),
		s3.CopyOptions{Options: d.getOptions(), MetadataDirective: "", ContentType: d.getContentType()},
		d.Bucket.Name+"/"+sourcePath)
	if err != nil {
		return storagedriver.PathNotFoundError{Path: sourcePath}
	}

	return d.Delete(sourcePath)
}

// Delete recursively deletes all objects stored at "path" and its subpaths.
func (d *Driver) Delete(path string) error {
	listResponse, err := d.Bucket.List(path, "", "", listPartsMax)
	if err != nil || len(listResponse.Contents) == 0 {
		return storagedriver.PathNotFoundError{Path: path}
	}

	s3Objects := make([]s3.Object, listPartsMax)

	for len(listResponse.Contents) > 0 {
		for index, key := range listResponse.Contents {
			s3Objects[index].Key = key.Key
		}

		err := d.Bucket.DelMulti(s3.Delete{Quiet: false, Objects: s3Objects[0:len(listResponse.Contents)]})
		if err != nil {
			return nil
		}

		listResponse, err = d.Bucket.List(path, "", "", listPartsMax)
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *Driver) getHighestIDMulti(path string) (multi *s3.Multi, err error) {
	multis, _, err := d.Bucket.ListMulti(path, "")
	if err != nil && !hasCode(err, "NoSuchUpload") {
		return nil, err
	}

	uploadID := ""

	if len(multis) > 0 {
		for _, m := range multis {
			if m.Key == path && m.UploadId >= uploadID {
				uploadID = m.UploadId
				multi = m
			}
		}
		return multi, nil
	}
	multi, err = d.Bucket.InitMulti(path, d.getContentType(), getPermissions(), d.getOptions())
	return multi, err
}

func (d *Driver) getAllParts(path string) (*s3.Multi, []s3.Part, error) {
	multi, err := d.getHighestIDMulti(path)
	if err != nil {
		return nil, nil, err
	}

	parts, err := multi.ListParts()
	return multi, parts, err
}

func hasCode(err error, code string) bool {
	s3err, ok := err.(*aws.Error)
	return ok && s3err.Code == code
}

func (d *Driver) getOptions() s3.Options {
	return s3.Options{SSE: d.Encrypt}
}

func getPermissions() s3.ACL {
	return s3.Private
}

func (d *Driver) getContentType() string {
	return "application/octet-stream"
}
