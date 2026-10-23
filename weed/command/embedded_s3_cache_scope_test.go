package command

import "testing"

func TestEmbeddedS3ExplicitCacheScope(t *testing.T) {
	size := int64(2048)
	options := S3Options{cacheSizeMB: &size}
	if !configureEmbeddedS3Scope(&options, "/cache", 51200) {
		t.Fatal("explicit S3 scope ignored")
	}
	if *options.cacheSizeMB != 2048 || *options.wholeCacheSizeMB != 51200 || *options.cacheDir != "/cache" {
		t.Fatal("scope settings changed")
	}
	if configureEmbeddedS3Scope(&S3Options{}, "/cache", 51200) {
		t.Fatal("implicit scope should reuse filer cache")
	}
}
