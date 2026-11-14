package config

import "testing"

func TestProductionStorageRequiresDurabilityAndTLS(t *testing.T) {
	c := validConfig()
	c.AppEnv = "production"
	c.AppBaseURL = "https://pic.test"
	if err := c.Validate(); err == nil {
		t.Fatal("production memory accepted")
	}
	c.StorageDriver = "s3"
	c.S3Bucket = "test"
	c.S3AccessKeyID = "test"
	c.S3SecretAccessKey = "test"
	c.S3Endpoint = "http://storage.test"
	if err := c.Validate(); err == nil {
		t.Fatal("plaintext production S3 accepted")
	}
	c.S3Endpoint = "https://storage.test"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
