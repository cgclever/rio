package redbox

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/cgclever/redbox/s3box"
)

const defaultNManifests = 4

var (
	// errShippingInProgress captures operations when a send is in progress.
	errShippingInProgress = fmt.Errorf("Cannot perform any action when shipping is in progress.")

	// errIncompleteArgs captures when not enough arguments are given for generating a new Redbox
	errIncompleteArgs = fmt.Errorf("Creating a redshift box requires a schema, table and an s3 bucket.")

	// errInvalidJSONInput captures when the input data can't be marshalled into JSON.
	errInvalidJSONInput = fmt.Errorf("Only JSON-able inputs are supported for syncing to Redshift.")
)

// Redbox manages piping data into Redshift. The core idea is to buffer data locally, ship to s3 when too much is buffered, and finally box to Redshift.
type Redbox struct {
	// Inheret mutex locking/unlocking
	sync.Mutex

	// schema is the schema of the destination
	schema string

	// table is the table name of the destination
	table string

	// awsKey is the AWS ACCESS KEY ID to the s3bucket
	awsKey string

	// awsPassword is the AWS SECRET ACCESS KEY to the s3Bucket
	awsPassword string

	// s3Bucket is the bucket storing our data
	s3Bucket string

	// s3Region is the location of the destination s3Bucket
	s3Region string

	// nManifests is the number of manifests to split streamed data into.
	nManifests int

	// s3Box manages the transport of data to Redshift
	s3Box s3box.S3BoxAPI

	// redshift is the direct redshift connection
	redshift *sql.DB

	// shippingInProgress indicates if a send is in progress
	shippingInProgress bool

	// truncate indicates if we should truncate the destination table
	truncate bool
}

// NewRedboxOptions is the expected input for creating a new Redbox
type NewRedboxOptions struct {
	// Schema is the destination Redshift table schema
	Schema string

	// Table is the destination Redshift table name
	Table string

	// S3Bucket specifies the intermediary bucket before ultimately piping to Redshift. The user should have access to this bucket.
	S3Bucket string

	// S3Region is the location of the S3Bucket. If not provided Redbox will
	// location the region via the AWS API.
	S3Region string

	// AWSKey is the AWS ACCESS KEY ID
	AWSKey string

	// AWSPassword is the AWS SECRET ACCESS KEY
	AWSPassword string

	// BufferSize is the maximum size of data we're willing to buffer
	// before creating an s3 file
	BufferSize int

	// NManifests is an optional parameter choosing how many manifests
	// to break data into. When data transfer gets to several gigabytes
	// the user should experiment with larger manifest numbers to prevent
	// timeouts.
	//
	// Note: This number isn't attempted to be autocalculated as
	// different cluster configurations can handle different influxes
	// of data. However the number defaults to 4.
	NManifests int

	// Truncate indicates if we should truncate the destination table
	Truncate bool

	// RedshiftConfiguration specifies the destination Redshift configuration
	RedshiftConfiguration RedshiftConfiguration
}

// newRedboxGivenS3BoxAndRedshift returns an Redbox with given input s3Box and redshift inputs.
func newRedboxGivenS3BoxAndRedshift(options NewRedboxOptions, s3Box s3box.S3BoxAPI, redshift *sql.DB) *Redbox {
	return &Redbox{
		schema:      options.Schema,
		table:       options.Table,
		s3Bucket:    options.S3Bucket,
		s3Region:    options.S3Region,
		nManifests:  options.NManifests,
		awsKey:      options.AWSKey,
		awsPassword: options.AWSPassword,
		s3Box:       s3Box,
		redshift:    redshift,
		truncate:    options.Truncate,
	}
}

// NewRedbox creates a new Redbox given the input options.
// Errors occur if there's an invalid input or if there's
// difficulty setting up either an s3 or redshift connection.
func NewRedbox(options NewRedboxOptions) (*Redbox, error) {
	if options.Schema == "" || options.Table == "" || options.S3Bucket == "" {
		return nil, errIncompleteArgs
	}

	if options.AWSKey == "" {
		options.AWSKey = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	if options.AWSPassword == "" {
		options.AWSPassword = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}

	if options.S3Region == "" {
		s3Region, err := s3box.GetRegionForBucket(options.S3Bucket)
		if err != nil {
			return nil, err
		}
		options.S3Region = s3Region
	}

	s3Box, err := s3box.NewS3Box(s3box.NewS3BoxOptions{
		S3Bucket:    options.S3Bucket,
		AWSKey:      options.AWSKey,
		AWSPassword: options.AWSPassword,
		BufferSize:  options.BufferSize,
	})
	if err != nil {
		return nil, err
	}

	redshift, err := options.RedshiftConfiguration.RedshiftConnection()
	if err != nil {
		return nil, err
	}

	if options.NManifests <= 0 {
		options.NManifests = defaultNManifests
	}

	return newRedboxGivenS3BoxAndRedshift(options, s3Box, redshift), nil
}

// Pack writes a single row of bytes. Currently only configured to accept JSON inputs,
// but will support CSV inputs in the future.
func (rb *Redbox) Pack(row []byte) error {
	if rb.isShippingInProgress() {
		return errShippingInProgress
	}

	var tempMap map[string]interface{}
	if err := json.Unmarshal(row, &tempMap); err != nil {
		return errInvalidJSONInput
	}
	return rb.s3Box.Pack(row)
}

// Ship ships written data to the destination Redshift table.
// While a send is in progress, no other operations are permitted.
// If a send succeeds a new s3Box will be created, allowing for further
// packing.
func (rb *Redbox) Ship() ([]string, error) {
	if rb.isShippingInProgress() {
		return nil, errShippingInProgress
	}

	// Kick off the s3-to-Redshift job
	rb.setShippingInProgress(true)
	defer func() {
		rb.setShippingInProgress(false)
	}()

	manifests, err := rb.s3Box.CreateManifests(rb.manifestSlug(), rb.nManifests)
	if err != nil {
		return nil, err
	}
	if len(manifests) == 0 { // If no data was written, there's nothing to ship.
		return nil, nil
	}

	if err := rb.copyToRedshift(manifests); err != nil {
		return nil, err
	}

	return manifests, nil
}

// manifestSlug defines a convention for the slug of each manifest file.
func (rb *Redbox) manifestSlug() string {
	return fmt.Sprintf("%s_%s_%s", rb.schema, rb.table, time.Now().Format(time.RFC3339))
}

// copyToRedshift transports data pointed to by the manifests into Redshift.
// If the truncate flag is present we clear the destination table first.
func (rb *Redbox) copyToRedshift(manifests []string) error {
	tx, err := rb.redshift.Begin()
	if err != nil {
		return err
	}

	if rb.truncate {
		delStmt := fmt.Sprintf("DELETE FROM \"%s\".\"%s\"", rb.schema, rb.table)
		if _, err := tx.Exec(delStmt); err != nil {
			tx.Rollback()
			return err
		}
	}

	for _, manifest := range manifests {
		copyStmt := rb.copyStatement(manifest)
		if _, err := tx.Exec(copyStmt); err != nil {
			tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

// copyStatment generates the COPY statement for the given manifest and Redbox configuration
func (rb *Redbox) copyStatement(manifest string) string {
	manifestURL := fmt.Sprintf("s3://%s/%s", rb.s3Bucket, manifest)
	copy := fmt.Sprintf("COPY \"%s\".\"%s\" FROM '%s' MANIFEST REGION '%s'", rb.schema, rb.table, manifestURL, rb.s3Region)
	dataFormat := "GZIP JSON 'auto'"
	options := "TIMEFORMAT 'auto' TRUNCATECOLUMNS STATUPDATE ON COMPUPDATE ON"
	creds := fmt.Sprintf("CREDENTIALS 'aws_access_key_id=%s;aws_secret_access_key=%s'", rb.awsKey, rb.awsPassword)
	return fmt.Sprintf("%s %s %s %s", copy, dataFormat, options, creds)
}

func (rb *Redbox) setShippingInProgress(inProgress bool) {
	rb.Lock()
	defer rb.Unlock()
	rb.shippingInProgress = inProgress
}

// isShippingInProgress exposes whether a send is in progress.
func (rb *Redbox) isShippingInProgress() bool {
	rb.Lock()
	defer rb.Unlock()
	return rb.shippingInProgress
}
